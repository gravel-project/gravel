package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/gravel-project/gravel/internal/store"
)

// Missing data is a row, not a shortfall (ADR-0011 §5): a gap is a stats event of kind gap with a
// reason and a window. The detectors are the ingest route's:
//
//   - hub_down: at start, for every server whose feed has posted and does not retry (its game's
//     spec, feed.retries: false), from when the hub stopped (the
//     last minute its previous run was known to run, store.HubRun) to the start, when the poll saw
//     anyone on the server as the hub stopped. A source that never retries lost whatever it posted
//     while the hub was away; one with nobody on had nothing to post. Not the last batch: a feed
//     that posts only when it has events can be silent for hours before the hub stops (amended
//     2026-10-19, after 0.10.1 recorded a 1029-minute gap with nobody on). A hub with no record
//     of a previous run records no hub_down gap: it cannot say when it was away. A feed that
//     retries lost nothing it still holds (War Dogs held nine minutes of 503s, deploy#49): what it
//     gave up on is the dropped gap's.
//   - silence: no batch for SilenceAfter while the poll sees at least SilencePlayers. War Dogs'
//     feed posts only when it has events (no batch at all for hours on an empty server, Season
//     02), so a few players idling below the match's minimum is quiet, not lost; with ten or more
//     on a match runs and kills come every few seconds, so ten minutes of nothing is a feed that
//     stopped (John, 2026-10-10, kept on 2026-10-19). The gap runs from the last batch to the
//     next one, and is written when that one comes.
//
//   - dropped: what the server says it gave up on (War Dogs: GET /v1/feed's droppedTotal), as the
//     poll reads it, against what it said last, saved (store.FeedDelivery) so a drop while the hub
//     was away is seen at its next start. The gap runs from the last reading to this one and
//     carries how many events. A count below the saved one is the server's restart: all it counts
//     since were lost since the last reading.
//   - reconcile: for a game whose feed posts every kill (feed.kills), ReconcileAfter past a poll
//     match's end, each player's kills and deaths from the feed's events in the match against the
//     poll's (ADR-0012's match_stats). Where the feed's fall short, one gap for the match, from its
//     start to its end, with how many kills and deaths are missing. The game's own match id is not
//     the poll's, so the feed's events are taken by time: the match's start to its end and
//     ReconcileAfter (a batch the server held while its sink was down arrives late), which can
//     count a kill of the next match and so hide a shortfall, never invent one.

const (
	// SilenceAfter is how long without a batch, while SilencePlayers are on, is a silence gap.
	SilenceAfter = 10 * time.Minute
	// SilencePlayers is how many players the poll must see for a silence to count.
	SilencePlayers = 10
	// GapsEvery is the silence check's period.
	GapsEvery = time.Minute

	// ReconcileAfter is how long after a match's end its kills are compared: long enough for what
	// the server held to post (19 s after nine minutes down, deploy#49) and for the parse job.
	ReconcileAfter = 5 * time.Minute
	// ReconcileWithin is how far back a match's end is looked for: a hub away longer reconciles
	// only what ended since.
	ReconcileWithin = 6 * time.Hour

	// Gap reasons.
	GapHubDown   = "hub_down"
	GapSilence   = "silence"
	GapDropped   = "dropped"
	GapReconcile = "reconcile"
)

// GapStore is what the gap detectors need from the store.
type GapStore interface {
	LastIngestBatch(ctx context.Context, orgID uuid.UUID, serverID string, before time.Time) (time.Time, bool, error)
	HadPlayers(ctx context.Context, orgID uuid.UUID, serverID string, at time.Time) (bool, error)
	RecordStatsEvents(ctx context.Context, events []store.StatsEvent) ([]bool, error)
	LastFeedDelivery(ctx context.Context, orgID uuid.UUID, serverID string) (store.FeedDelivery, bool, error)
	SaveFeedDelivery(ctx context.Context, orgID uuid.UUID, serverID string, d store.FeedDelivery) error
	EndedMatches(ctx context.Context, orgID uuid.UUID, serverID string, from, to time.Time) ([]store.Match, error)
	MatchPlayers(ctx context.Context, matchID int64) ([]store.MatchPlayer, error)
	KillCounts(ctx context.Context, orgID uuid.UUID, serverID, kind string, from, to time.Time) (map[store.PlayerKey]store.KillCount, error)
}

// FeedServer is a server whose feed the hub parses, and what its game's spec says of the feed.
type FeedServer struct {
	ID, Game, Trust string
	// Retries and Kills are the game's feed.retries and feed.kills.
	Retries, Kills bool
}

// FeedServers lists the servers with a feed and a parser for it.
type FeedServers func(ctx context.Context) ([]FeedServer, error)

// Occupancy is the poll's latest word on a server: how many players, and whether it answered.
type Occupancy func(server string) (players int, reachable bool)

// Delivery is what a server last said of its feed's delivery to the hub, as the poll read it; ok
// is false when it says nothing (a build that cannot tell, a feed that is off, no poll yet).
type Delivery func(server string) (d store.FeedDelivery, ok bool)

// Away is when the hub was last away: from the last time its previous run was known to run to
// this start. Known is false when the hub has no record of a previous run.
type Away struct {
	From, To time.Time
	Known    bool
}

// Gaps runs the hub_down and silence detectors.
type Gaps struct {
	st        GapStore
	orgID     uuid.UUID
	servers   FeedServers
	occupancy Occupancy
	delivery  Delivery
	away      Away
	logger    *slog.Logger
	now       func() time.Time

	mu      sync.Mutex
	checked map[string]bool      // servers whose hub_down was decided
	last    map[string]time.Time // the newest batch each server pushed
	silent  map[string]time.Time // a silence in progress: from its last batch
	// delivered is each server's saved delivery, read from the store once.
	delivered  map[string]store.FeedDelivery
	reconciled map[string]map[int64]bool // the matches each server's reconcile decided

	gaps    *prometheus.CounterVec
	dropped *prometheus.CounterVec
}

// NewGaps builds the detectors for a hub that was away as away says (To is its start), and
// registers their metric.
func NewGaps(st GapStore, orgID uuid.UUID, servers FeedServers, occupancy Occupancy, delivery Delivery, away Away, reg prometheus.Registerer, logger *slog.Logger) *Gaps {
	g := &Gaps{st: st, orgID: orgID, servers: servers, occupancy: occupancy, delivery: delivery, away: away, logger: logger, now: time.Now,
		checked: map[string]bool{}, last: map[string]time.Time{}, silent: map[string]time.Time{},
		delivered: map[string]store.FeedDelivery{}, reconciled: map[string]map[int64]bool{},
		gaps: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gravel_ingest_gaps_total",
			Help: "Gaps in a server's feed the hub recorded (ADR-0011 §5), by reason: hub_down (the hub stopped while players were on, for a feed that does not retry), silence (no batch for ten minutes while ten or more players were on), dropped (the server gave up on events), reconcile (a match's kills or deaths from the feed below the poll's).",
		}, []string{"server", "reason"}),
		dropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gravel_ingest_dropped_events_total",
			Help: "Events a server said it gave up on posting to the hub (ADR-0011 §5, the dropped gap), as the poll read its own count.",
		}, []string{"server"}),
	}
	reg.MustRegister(g.gaps, g.dropped)
	return g
}

// Stored is told of every batch the route stored; it is the ingest handler's hook.
func (g *Gaps) Stored(server string, at time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if at.After(g.last[server]) {
		g.last[server] = at
	}
}

// Run decides each new server's hub_down gap, writes the silences that ended and starts the ones
// that began, compares the server's dropped count with the last, and reconciles the matches that
// ended ReconcileAfter ago.
func (g *Gaps) Run(ctx context.Context) error {
	list, err := g.servers(ctx)
	if err != nil {
		return fmt.Errorf("ingest: gaps: %w", err)
	}
	var errs []error
	for _, s := range list {
		if err := g.hubDown(ctx, s); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := g.silence(ctx, s); err != nil {
			errs = append(errs, err)
		}
		if err := g.droppedEvents(ctx, s); err != nil {
			errs = append(errs, err)
		}
		if err := g.reconcile(ctx, s); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// hubDown runs once per server: a gap from the hub's stop to its start when anyone was on as it
// stopped. A server whose feed never posted before the start (new in servers.yaml, or a feed not
// on yet) lost nothing, so no gap; nor on a hub that does not know when it stopped.
func (g *Gaps) hubDown(ctx context.Context, s FeedServer) error {
	g.mu.Lock()
	done := g.checked[s.ID]
	g.mu.Unlock()
	if done {
		return nil
	}
	last, ok, err := g.st.LastIngestBatch(ctx, g.orgID, s.ID, g.away.To)
	if err != nil {
		return fmt.Errorf("ingest: gaps: server %s: %w", s.ID, err)
	}
	if ok && g.away.Known && !s.Retries {
		had, err := g.st.HadPlayers(ctx, g.orgID, s.ID, g.away.From)
		if err != nil {
			return fmt.Errorf("ingest: gaps: server %s: %w", s.ID, err)
		}
		if had {
			if err := g.record(ctx, s, GapHubDown, g.away.From, g.away.To, nil); err != nil {
				return err
			}
		}
	}
	g.mu.Lock()
	g.checked[s.ID] = true
	if ok && last.After(g.last[s.ID]) {
		g.last[s.ID] = last
	}
	g.mu.Unlock()
	return nil
}

// silence ends a silence when a batch came after it began, or begins one when the last batch is
// SilenceAfter old and the poll sees SilencePlayers on. A server that never posted has no feed on yet.
func (g *Gaps) silence(ctx context.Context, s FeedServer) error {
	g.mu.Lock()
	last, from, open := g.last[s.ID], g.silent[s.ID], false
	if !from.IsZero() {
		open = true
	}
	g.mu.Unlock()
	if open {
		if !last.After(from) {
			return nil
		}
		if err := g.record(ctx, s, GapSilence, from, last, nil); err != nil {
			return err
		}
		g.mu.Lock()
		delete(g.silent, s.ID)
		g.mu.Unlock()
		return nil
	}
	if last.IsZero() || g.now().Sub(last) < SilenceAfter {
		return nil
	}
	if players, reachable := g.occupancy(s.ID); !reachable || players < SilencePlayers {
		return nil
	}
	g.mu.Lock()
	g.silent[s.ID] = last
	g.mu.Unlock()
	g.logger.WarnContext(ctx, "ingest: a server's feed went quiet in a full match; a gap is recorded when it posts again",
		"server", s.ID, "last_batch", last.UTC().Format(time.RFC3339))
	return nil
}

// droppedEvents compares what the server says it gave up on with what it said last, and saves
// the reading. The first reading of a server the hub never read is only saved: it cannot say when
// those were dropped.
func (g *Gaps) droppedEvents(ctx context.Context, s FeedServer) error {
	if g.delivery == nil {
		return nil
	}
	cur, ok := g.delivery(s.ID)
	if !ok {
		return nil
	}
	g.mu.Lock()
	prev, have := g.delivered[s.ID]
	g.mu.Unlock()
	if !have {
		var err error
		if prev, have, err = g.st.LastFeedDelivery(ctx, g.orgID, s.ID); err != nil {
			return fmt.Errorf("ingest: gaps: server %s: %w", s.ID, err)
		}
	}
	if have && !cur.ObservedAt.After(prev.ObservedAt) {
		return nil // the same reading again
	}
	if have {
		lost := cur.Dropped - prev.Dropped
		if cur.Sent < prev.Sent || cur.Dropped < prev.Dropped {
			lost = cur.Dropped // the server restarted: everything it counts is since
		}
		if lost > 0 {
			if err := g.record(ctx, s, GapDropped, prev.ObservedAt, cur.ObservedAt, map[string]any{"events": lost}); err != nil {
				return err
			}
			g.dropped.WithLabelValues(s.ID).Add(float64(lost))
		}
	}
	if err := g.st.SaveFeedDelivery(ctx, g.orgID, s.ID, cur); err != nil {
		return fmt.Errorf("ingest: gaps: server %s: %w", s.ID, err)
	}
	g.mu.Lock()
	g.delivered[s.ID] = cur
	g.mu.Unlock()
	return nil
}

// reconcile compares, once each, the matches that ended between ReconcileWithin and
// ReconcileAfter ago: the feed's kills and deaths per player against the poll's.
func (g *Gaps) reconcile(ctx context.Context, s FeedServer) error {
	if !s.Kills {
		return nil
	}
	now := g.now()
	matches, err := g.st.EndedMatches(ctx, g.orgID, s.ID, now.Add(-ReconcileWithin), now.Add(-ReconcileAfter))
	if err != nil {
		return fmt.Errorf("ingest: gaps: server %s: %w", s.ID, err)
	}
	for _, m := range matches {
		g.mu.Lock()
		done := g.reconciled[s.ID][m.ID]
		g.mu.Unlock()
		if done || m.EndedAt == nil {
			continue
		}
		players, err := g.st.MatchPlayers(ctx, m.ID)
		if err != nil {
			return fmt.Errorf("ingest: gaps: server %s: match %d: %w", s.ID, m.ID, err)
		}
		feed, err := g.st.KillCounts(ctx, g.orgID, s.ID, KindKill, m.StartedAt, m.EndedAt.Add(ReconcileAfter))
		if err != nil {
			return fmt.Errorf("ingest: gaps: server %s: match %d: %w", s.ID, m.ID, err)
		}
		var kills, deaths, short int
		for _, p := range players {
			f := feed[store.PlayerKey{Provider: p.Provider, Subject: p.Subject}]
			k, d := max(p.Kills-f.Kills, 0), max(p.Deaths-f.Deaths, 0)
			kills, deaths = kills+k, deaths+d
			if k+d > 0 {
				short++
			}
		}
		if kills+deaths > 0 {
			if err := g.record(ctx, s, GapReconcile, m.StartedAt, *m.EndedAt,
				map[string]any{"match": m.ID, "kills": kills, "deaths": deaths, "players": short}); err != nil {
				return err
			}
		}
		g.mu.Lock()
		if g.reconciled[s.ID] == nil {
			g.reconciled[s.ID] = map[int64]bool{}
		}
		g.reconciled[s.ID][m.ID] = true
		g.mu.Unlock()
	}
	return nil
}

// record writes a gap with its reason, its end and the reason's own fields (extra). Its source key
// is its reason and start, so the same gap found twice (a restart while the hub was coming up) is
// stored once.
func (g *Gaps) record(ctx context.Context, s FeedServer, reason string, from, to time.Time, extra map[string]any) error {
	fields := map[string]any{"reason": reason, "to": to.UTC().Format(time.RFC3339Nano)}
	for k, v := range extra {
		fields[k] = v
	}
	ext, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	stored, err := g.st.RecordStatsEvents(ctx, []store.StatsEvent{{OrganizationID: g.orgID, ServerID: s.ID, GameID: s.Game,
		Source: SourceServerAdapter, SourceKey: fmt.Sprintf("gap:%s:%d", reason, from.UnixNano()), Kind: KindGap, ObservedAt: from,
		ReceivedAt: g.now(), Trust: s.Trust, SchemaVersion: StatsSchemaVersion, Extensions: ext}})
	if err != nil {
		return fmt.Errorf("ingest: gaps: server %s: %w", s.ID, err)
	}
	if len(stored) == 1 && stored[0] {
		g.gaps.WithLabelValues(s.ID, reason).Inc()
		g.logger.WarnContext(ctx, "ingest: a gap in a server's feed", "server", s.ID, "reason", reason,
			"from", from.UTC().Format(time.RFC3339), "to", to.UTC().Format(time.RFC3339), "minutes", int(to.Sub(from).Minutes()))
	}
	return nil
}
