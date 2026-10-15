// Package results keeps match results a server's match plugin reports (ADR-0024): MatchZy on a CS2
// server posts a series' events to the hub's events route and uploads each map's demo to its demo
// route, from the server's own host. A series is the plugin's match id on that server; the live
// lease on the server, if any, is linked to it, and its end ends the lease's match.
//
// MatchZy 0.9.1 sends no player totals in map_result: each round_end carries the running totals of
// the map so far, so the last one is the map's. Players are keyed by their SteamID64 (principle 5);
// who they are is resolved when the result is read.
package results

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/gravel-project/gravel/games/cs2/matchzy"
	"github.com/gravel-project/gravel/internal/objectstore"
	"github.com/gravel-project/gravel/internal/store"
)

// Source is MatchZy's source name on a result.
const Source = "matchzy"

// Provider is the identity provider MatchZy's players are known by.
const Provider = "steam"

// DemoTTL is how long a demo's download link works.
const DemoTTL = 15 * time.Minute

// ErrNoDemo is a map with no demo kept, or a hub that keeps none.
var ErrNoDemo = errors.New("results: no demo")

// Store is what the results need from the database.
type Store interface {
	StartMatchResult(ctx context.Context, r store.MatchResult) (store.MatchResult, error)
	MatchResultFor(ctx context.Context, orgID uuid.UUID, serverID, source string, externalID int64) (store.MatchResult, error)
	EndMatchResult(ctx context.Context, id uuid.UUID, team1, team2 int, winner string, at time.Time) error
	SaveMatchResultMap(ctx context.Context, m store.MatchResultMap) error
	SetMatchResultDemo(ctx context.Context, resultID uuid.UUID, mapNumber int, key string, bytes int64) error
	ReplaceMatchResultPlayers(ctx context.Context, resultID uuid.UUID, mapNumber int, players []store.MatchResultPlayer) error
	GetMatchResult(ctx context.Context, orgID, id uuid.UUID) (store.MatchResult, error)
	ListMatchResults(ctx context.Context, orgID uuid.UUID, f store.MatchResultFilter) ([]store.MatchResult, error)
}

// Leases is what the results tell and ask of server leases.
type Leases interface {
	// LiveLease is a server's live lease, if it has one.
	LiveLease(serverID string) (uuid.UUID, bool)
	// MatchEnded ends a server's live lease's match.
	MatchEnded(ctx context.Context, serverID string)
}

// Service keeps the results.
type Service struct {
	st      Store
	orgID   uuid.UUID
	leases  Leases
	mapName func(serverID string) string
	// extensions are the stat extensions a server's game declares (ADR-0033), what a player's
	// row keeps of the plugin's values.
	extensions func(ctx context.Context, serverID string) []string
	objects    objectstore.Store
	maxDemo    int64
	logger     *slog.Logger
	now        func() time.Time
	events     *prometheus.CounterVec
	demos      *prometheus.CounterVec
}

// New builds the service. mapName says the map a server is on now (the monitor's last look);
// extensions, the stat extensions its game's spec declares (nil keeps none); objects keeps demos up
// to maxDemo bytes, nil for none.
func New(st Store, orgID uuid.UUID, leases Leases, mapName func(string) string, extensions func(context.Context, string) []string,
	objects objectstore.Store, maxDemo int64, reg prometheus.Registerer, logger *slog.Logger) *Service {
	if extensions == nil {
		extensions = func(context.Context, string) []string { return nil }
	}
	s := &Service{st: st, orgID: orgID, leases: leases, mapName: mapName, extensions: extensions, objects: objects, maxDemo: maxDemo, logger: logger, now: time.Now,
		events: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gravel_match_events_total", Help: "Match plugin events by event name and result: recorded, ignored (not one the hub keeps), or error.",
		}, []string{"event", "result"}),
		demos: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gravel_match_demos_total", Help: "Demos a server's match plugin uploaded, by result: kept or error.",
		}, []string{"result"}),
	}
	if objects == nil {
		s.maxDemo = 0
	}
	reg.MustRegister(s.events, s.demos)
	return s
}

// WithClock replaces the clock (tests).
func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

// Events implements ingest.EventSink: one event, recorded as it says. A failure is logged and
// counted; the batch is stored already, so it can be read again.
func (s *Service) Events(ctx context.Context, server string, body []byte) {
	ev, v, err := matchzy.Parse(body)
	name := ev.Name
	if name == "" {
		name = "unknown"
	}
	if err == nil {
		err = s.record(ctx, server, ev, v)
	}
	switch {
	case errors.Is(err, errIgnored):
		s.events.WithLabelValues(name, "ignored").Inc()
	case err != nil:
		s.events.WithLabelValues(name, "error").Inc()
		s.logger.Warn("a match event was not recorded", "server", server, "event", name, "match", ev.MatchID, "error", err.Error())
	default:
		s.events.WithLabelValues(name, "recorded").Inc()
	}
}

var errIgnored = errors.New("results: an event the hub keeps nothing of")

func (s *Service) record(ctx context.Context, server string, ev matchzy.Event, v any) error {
	now := s.now().UTC()
	switch e := v.(type) {
	case matchzy.SeriesStart:
		_, err := s.start(ctx, server, ev.MatchID, e.Team1.Name, e.Team2.Name, e.NumMaps, now)
		return err
	case matchzy.GoingLive:
		r, err := s.result(ctx, server, ev.MatchID, now)
		if err != nil {
			return err
		}
		return s.st.SaveMatchResultMap(ctx, store.MatchResultMap{ResultID: r.ID, MapNumber: e.MapNumber, MapName: s.mapName(server), StartedAt: &now})
	case matchzy.RoundEnd:
		r, err := s.result(ctx, server, ev.MatchID, now)
		if err != nil {
			return err
		}
		if err := s.st.SaveMatchResultMap(ctx, store.MatchResultMap{ResultID: r.ID, MapNumber: e.MapNumber, MapName: s.mapName(server),
			Team1Score: e.Team1.Score, Team2Score: e.Team2.Score}); err != nil {
			return err
		}
		declared := s.extensions(ctx, server)
		return s.st.ReplaceMatchResultPlayers(ctx, r.ID, e.MapNumber, append(players(e.Team1, matchzy.Team1, declared), players(e.Team2, matchzy.Team2, declared)...))
	case matchzy.MapResult:
		r, err := s.result(ctx, server, ev.MatchID, now)
		if err != nil {
			return err
		}
		return s.st.SaveMatchResultMap(ctx, store.MatchResultMap{ResultID: r.ID, MapNumber: e.MapNumber, MapName: s.mapName(server),
			Team1Score: e.Team1.Score, Team2Score: e.Team2.Score, Winner: e.Winner.Team, EndedAt: &now})
	case matchzy.SeriesEnd:
		r, err := s.result(ctx, server, ev.MatchID, now)
		if err != nil {
			return err
		}
		if err := s.st.EndMatchResult(ctx, r.ID, e.Team1SeriesScore, e.Team2SeriesScore, winner(e.Winner.Team), now); err != nil {
			return err
		}
		if s.leases != nil {
			s.leases.MatchEnded(ctx, server)
		}
		s.logger.Info("a match ended", "server", server, "result", r.ID, "match", ev.MatchID, "score", fmt.Sprintf("%d-%d", e.Team1SeriesScore, e.Team2SeriesScore))
		return nil
	}
	return errIgnored
}

func winner(team string) string {
	switch team {
	case matchzy.Team1, matchzy.Team2, matchzy.TeamNone:
		return team
	}
	return ""
}

// players are a team's players' running totals on a map, keyed by SteamID64, with the plugin's
// values the game declares as extensions (one the plugin doesn't report stays absent).
func players(t matchzy.StatsTeam, team string, declared []string) []store.MatchResultPlayer {
	out := make([]store.MatchResultPlayer, 0, len(t.Players))
	for _, p := range t.Players {
		if !steamID.MatchString(p.SteamID) {
			continue // a bot
		}
		values := p.Stats.Values()
		ext := make(map[string]float64, len(declared))
		for _, name := range declared {
			if v, ok := values[name]; ok {
				ext[name] = v
			}
		}
		out = append(out, store.MatchResultPlayer{Provider: Provider, Subject: p.SteamID, Name: p.Name, Team: team,
			Kills: p.Stats.Kills, Deaths: p.Stats.Deaths, Extensions: ext})
	}
	return out
}

var steamID = regexp.MustCompile(`^7656119[0-9]{10}$`)

// start records a series, linked to the server's live lease.
func (s *Service) start(ctx context.Context, server string, matchID int64, team1, team2 string, maps int, at time.Time) (store.MatchResult, error) {
	r := store.MatchResult{ID: uuid.New(), OrganizationID: s.orgID, ServerID: server, Source: Source, ExternalID: matchID, Team1Name: team1,
		Team2Name: team2, NumMaps: maps, StartedAt: at}
	if s.leases != nil {
		if id, ok := s.leases.LiveLease(server); ok {
			r.LeaseID = &id
		}
	}
	return s.st.StartMatchResult(ctx, r)
}

// result is a series by match id, started now when its series_start never came (MatchZy doesn't
// send an event again).
func (s *Service) result(ctx context.Context, server string, matchID int64, now time.Time) (store.MatchResult, error) {
	r, err := s.st.MatchResultFor(ctx, s.orgID, server, Source, matchID)
	if errors.Is(err, store.ErrNotFound) {
		return s.start(ctx, server, matchID, "", "", 1, now)
	}
	return r, err
}

// MaxDemoBytes implements ingest.DemoSink.
func (s *Service) MaxDemoBytes() int64 { return s.maxDemo }

// Demo implements ingest.DemoSink: the demo goes to object storage under its result, and the map
// records it.
func (s *Service) Demo(ctx context.Context, server string, h http.Header, body io.Reader, size int64) error {
	if s.objects == nil {
		return ErrNoDemo
	}
	matchID, err := strconv.ParseInt(h.Get(matchzy.HeaderMatchID), 10, 64)
	if err != nil {
		s.demos.WithLabelValues("error").Inc()
		return fmt.Errorf("results: the demo's %s: %w", matchzy.HeaderMatchID, err)
	}
	mapNumber, err := strconv.Atoi(h.Get(matchzy.HeaderMapNumber))
	if err != nil || mapNumber < 0 {
		s.demos.WithLabelValues("error").Inc()
		return fmt.Errorf("results: the demo's %s is not a map number", matchzy.HeaderMapNumber)
	}
	r, err := s.result(ctx, server, matchID, s.now().UTC())
	if err != nil {
		s.demos.WithLabelValues("error").Inc()
		return err
	}
	name := demoName.ReplaceAllString(path.Base(h.Get(matchzy.HeaderFileName)), "_")
	if name == "" || name == "." || name == "_" {
		name = "demo.dem"
	}
	key := fmt.Sprintf("matches/%s/%d-%s", r.ID, mapNumber, name)
	counted := &countingReader{r: body}
	if err := s.objects.Put(ctx, key, counted, size, "application/octet-stream"); err != nil {
		s.demos.WithLabelValues("error").Inc()
		return err
	}
	if err := s.st.SetMatchResultDemo(ctx, r.ID, mapNumber, key, counted.n); err != nil {
		s.demos.WithLabelValues("error").Inc()
		return err
	}
	s.demos.WithLabelValues("kept").Inc()
	s.logger.Info("a demo was kept", "server", server, "result", r.ID, "map", mapNumber, "bytes", counted.n)
	return nil
}

var demoName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// Get is a result with its maps and players.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (store.MatchResult, error) {
	return s.st.GetMatchResult(ctx, s.orgID, id)
}

// List is results, newest first.
func (s *Service) List(ctx context.Context, f store.MatchResultFilter) ([]store.MatchResult, error) {
	return s.st.ListMatchResults(ctx, s.orgID, f)
}

// DemoURL is a short-lived link to download a map's demo.
func (s *Service) DemoURL(ctx context.Context, id uuid.UUID, mapNumber int) (string, error) {
	if s.objects == nil {
		return "", ErrNoDemo
	}
	r, err := s.st.GetMatchResult(ctx, s.orgID, id)
	if err != nil {
		return "", err
	}
	for _, m := range r.Maps {
		if m.MapNumber == mapNumber && m.DemoKey != "" {
			return s.objects.SignedURL(ctx, m.DemoKey, DemoTTL, path.Base(m.DemoKey))
		}
	}
	return "", ErrNoDemo
}
