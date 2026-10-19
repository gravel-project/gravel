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
// reason and a window. Two of the detectors are the ingest route's:
//
//   - hub_down: at start, for every server whose feed has posted, from when the hub stopped (the
//     last minute its previous run was known to run, store.HubRun) to the start, when the poll saw
//     anyone on the server as the hub stopped. A source that never retries lost whatever it posted
//     while the hub was away; one with nobody on had nothing to post. Not the last batch: a feed
//     that posts only when it has events can be silent for hours before the hub stops (amended
//     2026-10-19, after 0.10.1 recorded a 1029-minute gap with nobody on). A hub with no record
//     of a previous run records no hub_down gap: it cannot say when it was away.
//   - silence: no batch for SilenceAfter while the poll sees at least SilencePlayers. War Dogs'
//     feed posts only when it has events (no batch at all for hours on an empty server, Season
//     02), so a few players idling below the match's minimum is quiet, not lost; with ten or more
//     on a match runs and kills come every few seconds, so ten minutes of nothing is a feed that
//     stopped (John, 2026-10-10, kept on 2026-10-19). The gap runs from the last batch to the
//     next one, and is written when that one comes.
//
// The third, reconcile (the feed's counts below the poll's), needs the kills, which come with the
// parser that reads them.

const (
	// SilenceAfter is how long without a batch, while SilencePlayers are on, is a silence gap.
	SilenceAfter = 10 * time.Minute
	// SilencePlayers is how many players the poll must see for a silence to count.
	SilencePlayers = 10
	// GapsEvery is the silence check's period.
	GapsEvery = time.Minute

	// Gap reasons.
	GapHubDown = "hub_down"
	GapSilence = "silence"
)

// GapStore is what the gap detectors need from the store.
type GapStore interface {
	LastIngestBatch(ctx context.Context, orgID uuid.UUID, serverID string, before time.Time) (time.Time, bool, error)
	HadPlayers(ctx context.Context, orgID uuid.UUID, serverID string, at time.Time) (bool, error)
	RecordStatsEvents(ctx context.Context, events []store.StatsEvent) ([]bool, error)
}

// FeedServer is a server whose feed the hub parses.
type FeedServer struct {
	ID, Game, Trust string
}

// FeedServers lists the servers with a feed and a parser for it.
type FeedServers func(ctx context.Context) ([]FeedServer, error)

// Occupancy is the poll's latest word on a server: how many players, and whether it answered.
type Occupancy func(server string) (players int, reachable bool)

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
	away      Away
	logger    *slog.Logger
	now       func() time.Time

	mu      sync.Mutex
	checked map[string]bool      // servers whose hub_down was decided
	last    map[string]time.Time // the newest batch each server pushed
	silent  map[string]time.Time // a silence in progress: from its last batch

	gaps *prometheus.CounterVec
}

// NewGaps builds the detectors for a hub that was away as away says (To is its start), and
// registers their metric.
func NewGaps(st GapStore, orgID uuid.UUID, servers FeedServers, occupancy Occupancy, away Away, reg prometheus.Registerer, logger *slog.Logger) *Gaps {
	g := &Gaps{st: st, orgID: orgID, servers: servers, occupancy: occupancy, away: away, logger: logger, now: time.Now,
		checked: map[string]bool{}, last: map[string]time.Time{}, silent: map[string]time.Time{},
		gaps: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gravel_ingest_gaps_total",
			Help: "Gaps in a server's feed the hub recorded (ADR-0011 §5), by reason: hub_down (the hub stopped while players were on), silence (no batch for ten minutes while ten or more players were on).",
		}, []string{"server", "reason"}),
	}
	reg.MustRegister(g.gaps)
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

// Run decides each new server's hub_down gap, then writes the silences that ended and starts the
// ones that began.
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
	if ok && g.away.Known {
		had, err := g.st.HadPlayers(ctx, g.orgID, s.ID, g.away.From)
		if err != nil {
			return fmt.Errorf("ingest: gaps: server %s: %w", s.ID, err)
		}
		if had {
			if err := g.record(ctx, s, GapHubDown, g.away.From, g.away.To); err != nil {
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
		if err := g.record(ctx, s, GapSilence, from, last); err != nil {
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

// record writes a gap. Its source key is its reason and start, so the same gap found twice (a
// restart while the hub was coming up) is stored once.
func (g *Gaps) record(ctx context.Context, s FeedServer, reason string, from, to time.Time) error {
	ext, err := json.Marshal(map[string]any{"reason": reason, "to": to.UTC().Format(time.RFC3339Nano)})
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
