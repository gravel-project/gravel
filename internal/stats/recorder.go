// Package stats is the stats store's domain side (ADR-0012): the recorder that turns each good
// poll of a server into matches and each player's totals in them, and the boards read from them.
// A board counts match_stats, so it is right with or without a feed; the feed's events (#4) add
// detail later. Rows are keyed by provider identity and resolved to members only when a board is
// read (principle 3); the server's trust is copied into each row when it is written (principle 9).
package stats

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/store"
)

// SourceServerAdapter is the provenance of counts read from the server itself (ADR-0011).
const SourceServerAdapter = "server_adapter"

// maxGap is the longest time between two polls that still counts as time on: a longer gap (the
// hub was down, a poll failed) adds nothing rather than guessing.
const maxGap = time.Minute

// recordTimeout bounds one observation's writes; the poll is waiting.
const recordTimeout = 5 * time.Second

// Store is what the stats package needs from the store.
type Store interface {
	OpenMatch(ctx context.Context, orgID uuid.UUID, serverID string) (store.Match, error)
	StartMatch(ctx context.Context, m store.Match) (store.Match, error)
	EndMatch(ctx context.Context, matchID int64, at time.Time) error
	MatchPlayers(ctx context.Context, matchID int64) ([]store.MatchPlayer, error)
	SaveMatchPlayers(ctx context.Context, players []store.MatchPlayer) error
	Board(ctx context.Context, q store.BoardQuery) ([]store.BoardRow, error)
	ListMatches(ctx context.Context, orgID uuid.UUID, serverID string, beforeID int64, limit int) ([]store.MatchSummary, error)
	Pseudonyms(ctx context.Context, orgID uuid.UUID, keys []store.PlayerKey) (map[store.PlayerKey]string, error)
	AddPseudonym(ctx context.Context, orgID uuid.UUID, key store.PlayerKey, name string, at time.Time) (string, error)
	BoardMembers(ctx context.Context, keys []store.PlayerKey) (map[store.PlayerKey]store.BoardMember, error)
	SetShowNameOnBoards(ctx context.Context, userID uuid.UUID, show bool) error
	ShowNameOnBoards(ctx context.Context, userID uuid.UUID) (bool, error)
	PlayerTotals(ctx context.Context, q store.PlayerTotalsQuery) (store.BoardRow, error)
	PlayerMatches(ctx context.Context, orgID uuid.UUID, keys []store.PlayerKey, limit int) ([]store.PlayerMatch, error)
}

// Recorder records matches from the monitor's observations; it is the monitor's ObservationSink.
type Recorder struct {
	st      Store
	orgID   uuid.UUID
	changes *Changes
	logger  *slog.Logger

	mu      sync.Mutex
	servers map[string]*serverState

	matches *prometheus.CounterVec
	rows    *prometheus.CounterVec
	errs    *prometheus.CounterVec
}

// serverState is what the recorder holds between one server's polls.
type serverState struct {
	loaded   bool // the open match (if any) was read back from the store
	match    store.Match
	players  map[store.PlayerKey]*playerState
	lastSeen time.Time // the previous good poll
	failing  bool      // the last record failed (logged once)
}

// playerState is one player in the open match: the row as written, and how to read the game's
// counters, which count from the player's (re)join.
type playerState struct {
	row        store.MatchPlayer
	baseKills  int // counts from earlier segments of this match (a rejoin restarts the game's)
	baseDeaths int
	lastKills  int // the game's counters at the previous poll; -1 before the first
	lastDeaths int
	resumed    bool // read back from the store after a hub restart: the next poll decides the base
	wasPresent bool
}

// NewRecorder builds the recorder and registers its metrics.
// changes is bumped after every write, for the boards' cache (nil for none).
func NewRecorder(st Store, orgID uuid.UUID, changes *Changes, reg prometheus.Registerer, logger *slog.Logger) *Recorder {
	r := &Recorder{
		st: st, orgID: orgID, changes: changes, logger: logger, servers: map[string]*serverState{},
		matches: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gravel_stats_matches_total", Help: "Matches the stats store started, by server and game."}, []string{"server", "game"}),
		rows:    prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gravel_stats_rows_written_total", Help: "Player rows the stats store wrote from polls, by server."}, []string{"server"}),
		errs:    prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gravel_stats_record_errors_total", Help: "Polls the stats store could not record, by server."}, []string{"server"}),
	}
	reg.MustRegister(r.matches, r.rows, r.errs)
	return r
}

// Observe records one good poll. A failure is logged once while it lasts and counted; the next
// poll tries again, from the store's state.
func (r *Recorder) Observe(ctx context.Context, srv servers.Server, o servers.Observation) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.servers[srv.ID]
	if s == nil {
		s = &serverState{players: map[store.PlayerKey]*playerState{}}
		r.servers[srv.ID] = s
	}
	if err := r.record(ctx, s, srv, o); err != nil {
		r.errs.WithLabelValues(srv.ID).Inc()
		if !s.failing {
			r.logger.WarnContext(ctx, "stats: could not record a poll, will retry", "server", srv.ID, "error", err.Error())
		}
		// Read the store again at the next poll rather than trust what was half written.
		r.servers[srv.ID] = &serverState{players: map[store.PlayerKey]*playerState{}, failing: true}
		return
	}
	if s.failing {
		s.failing = false
		r.logger.InfoContext(ctx, "stats: recording again", "server", srv.ID)
	}
}

func (r *Recorder) record(ctx context.Context, s *serverState, srv servers.Server, o servers.Observation) error {
	now := o.ObservedAt
	if !s.loaded {
		if err := r.load(ctx, s, srv.ID); err != nil {
			return err
		}
	}
	present := map[store.PlayerKey]struct{}{}
	for _, p := range o.Players {
		if p.Identity.Subject != "" {
			present[store.PlayerKey{Provider: p.Identity.Provider, Subject: p.Identity.Subject}] = struct{}{}
		}
	}

	// The server emptied: its match is over.
	if len(present) == 0 {
		if s.match.ID != 0 {
			if err := r.st.EndMatch(ctx, s.match.ID, now); err != nil {
				return err
			}
			r.changes.Bump()
			s.match, s.players = store.Match{}, map[store.PlayerKey]*playerState{}
		}
		s.lastSeen = now
		return nil
	}
	if s.match.ID == 0 || r.newMatch(s, o) {
		m, err := r.st.StartMatch(ctx, store.Match{OrganizationID: r.orgID, ServerID: srv.ID, GameID: srv.Game, StartedAt: now,
			Map: o.Status.Map, RotationIndex: o.Status.RotationIndex})
		if err != nil {
			return err
		}
		s.match, s.players = m, map[store.PlayerKey]*playerState{}
		r.matches.WithLabelValues(srv.ID, srv.Game).Inc()
	}

	elapsed := now.Sub(s.lastSeen)
	rows := make([]store.MatchPlayer, 0, len(present))
	for _, p := range o.Players {
		key := store.PlayerKey{Provider: p.Identity.Provider, Subject: p.Identity.Subject}
		if key.Subject == "" {
			continue
		}
		ps := s.players[key]
		if ps == nil {
			ps = &playerState{lastKills: -1, lastDeaths: -1, row: store.MatchPlayer{MatchID: s.match.ID, Provider: key.Provider, Subject: key.Subject,
				FirstSeen: now, Source: SourceServerAdapter, Trust: srv.Trust}}
			s.players[key] = ps
		}
		ps.update(p.Kills, p.Deaths)
		if ps.wasPresent && !s.lastSeen.IsZero() && elapsed > 0 && elapsed <= maxGap {
			ps.row.SecondsOn += int(elapsed.Seconds())
		}
		ps.row.Team, ps.row.LastSeen, ps.wasPresent = p.Team, now, true
		rows = append(rows, ps.row)
	}
	for key, ps := range s.players {
		if _, on := present[key]; !on {
			ps.wasPresent = false
		}
	}
	s.lastSeen = now
	if err := r.st.SaveMatchPlayers(ctx, rows); err != nil {
		return err
	}
	r.rows.WithLabelValues(srv.ID).Add(float64(len(rows)))
	r.changes.Bump()
	return nil
}

// update takes the game's counters for a player and sets the row's totals. The game counts from
// the player's (re)join, so a counter that goes down means a new segment: what the row had is kept
// and the game's count is added to it.
func (ps *playerState) update(kills, deaths int) {
	switch {
	case ps.resumed:
		// After a hub restart the game's counters may have carried on (this segment) or restarted
		// (a rejoin while the hub was down): carry on when they are at least the row's.
		ps.resumed = false
		if kills < ps.row.Kills || deaths < ps.row.Deaths {
			ps.baseKills, ps.baseDeaths = ps.row.Kills, ps.row.Deaths
		}
	case ps.lastKills >= 0 && (kills < ps.lastKills || deaths < ps.lastDeaths):
		ps.baseKills += ps.lastKills
		ps.baseDeaths += ps.lastDeaths
	}
	ps.lastKills, ps.lastDeaths = kills, deaths
	ps.row.Kills, ps.row.Deaths = ps.baseKills+kills, ps.baseDeaths+deaths
}

// newMatch reports whether an observation starts a new match on the server: the rotation moved,
// or every player who was on at the last poll and still is has lower counters (the game reset
// the match, or the server restarted). One player's counters going down is that player rejoining.
func (r *Recorder) newMatch(s *serverState, o servers.Observation) bool {
	if ri := o.Status.RotationIndex; ri >= 0 && s.match.RotationIndex >= 0 && ri != s.match.RotationIndex {
		return true
	}
	stayed, dropped := 0, 0
	for _, p := range o.Players {
		ps := s.players[store.PlayerKey{Provider: p.Identity.Provider, Subject: p.Identity.Subject}]
		if ps == nil || !ps.wasPresent || ps.lastKills < 0 || ps.lastKills+ps.lastDeaths == 0 {
			continue
		}
		stayed++
		if p.Kills+p.Deaths < ps.lastKills+ps.lastDeaths {
			dropped++
		}
	}
	return stayed >= 2 && dropped == stayed
}

// load reads a server's open match back after a hub restart, so its players' totals carry on.
func (r *Recorder) load(ctx context.Context, s *serverState, serverID string) error {
	m, err := r.st.OpenMatch(ctx, r.orgID, serverID)
	if errors.Is(err, store.ErrNotFound) {
		s.loaded = true
		return nil
	}
	if err != nil {
		return err
	}
	rows, err := r.st.MatchPlayers(ctx, m.ID)
	if err != nil {
		return err
	}
	s.match = m
	for _, row := range rows {
		s.players[store.PlayerKey{Provider: row.Provider, Subject: row.Subject}] = &playerState{row: row, lastKills: -1, lastDeaths: -1, resumed: true}
	}
	s.loaded = true
	return nil
}
