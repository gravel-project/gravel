package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/gravel-project/gravel/internal/store"
)

// Parsing reads the stored batches after the answer (ADR-0011 §1): a hub job hands each pending
// batch of a source to that source's Parser and stores the events it finds, deduplicated on their
// source keys, with the batch's provenance. A batch the parser cannot read is kept, marked failed
// and counted; raising the parser's version reads every stored batch of its source again, so a
// fixed parser, or one that learned a new kind, loses nothing the 30 days still hold.

const (
	// ParseEvery is the parse job's period: a batch waits at most this long for its events.
	ParseEvery = 10 * time.Second
	// SourceServerAdapter is the provenance of events a server reported itself (ADR-0011 §4).
	SourceServerAdapter = "server_adapter"
	// StatsSchemaVersion is stats_events' schema version.
	StatsSchemaVersion = 1
	// KindGap is a gap's kind (ADR-0011 §5).
	KindGap = "gap"
	// KindKill is a kill, whichever game's: its player the killer (none when the world killed),
	// its target the victim, the same player for a suicide. The reconcile gap counts it.
	KindKill = "kill"

	parsePage = 100
)

// Parser turns a source's stored batch into events (ADR-0011 §2). It is an adapter's: nothing in
// the core reads a game's wire format.
type Parser interface {
	// Version is the parser's version. Raising it reads every stored batch of its source again.
	Version() int
	Parse(b Batch) (Parsed, error)
}

// Batch is a stored batch as a parser reads it.
type Batch struct {
	ServerID   string
	ReceivedAt time.Time
	Body       []byte
	// Headers are the request headers the source defines, as stored.
	Headers json.RawMessage
}

// Event is one event a parser found.
type Event struct {
	// Key is the source's own identity for the event (War Dogs: its eventId); a second event with
	// the same key from the same server is a duplicate.
	Key  string
	Kind string
	// ObservedAt is the game's time of the event when it gives one as a wall-clock time; zero
	// means the batch's received time.
	ObservedAt time.Time
	// Player is who the event is about (a kill's killer), Target the other player (its victim);
	// zero for nobody.
	Player, Target store.PlayerKey
	// Extensions are the kind's own fields.
	Extensions map[string]any
}

// Parsed is what a parser found in a batch.
type Parsed struct {
	Events []Event
	// Unknown are the types, as the source names them, of the events of a kind the parser does
	// not know, one entry per event. They stay in the stored batch for a later parser.
	Unknown []string
	// Invalid counts the events of a known kind the parser could not read.
	Invalid int
}

// ParseStore is what the parse job needs from the store.
type ParseStore interface {
	PendingIngestBatches(ctx context.Context, orgID uuid.UUID, source string, limit int) ([]store.IngestBatch, error)
	SaveParsedBatch(ctx context.Context, p store.ParsedBatch) ([]bool, error)
	RequeueIngestBatches(ctx context.Context, orgID uuid.UUID, source string, version int) (int64, error)
}

// ServerInfo returns a server's game and its trust now: an event's trust is its server's when it
// is first stored.
type ServerInfo func(ctx context.Context, server string) (game, trust string, err error)

// Parsing is the parse job.
type Parsing struct {
	st      ParseStore
	orgID   uuid.UUID
	parsers map[string]Parser
	server  ServerInfo
	logger  *slog.Logger

	mu       sync.Mutex
	requeued map[string]bool // sources whose batches older parsers read were put back
	unknown  map[string]bool // server and type of an unknown kind, logged once

	parsed     *prometheus.CounterVec
	events     *prometheus.CounterVec
	duplicates *prometheus.CounterVec
}

// NewParsing builds the parse job for parsers, by source, and registers its metrics.
func NewParsing(st ParseStore, orgID uuid.UUID, parsers map[string]Parser, server ServerInfo, reg prometheus.Registerer, logger *slog.Logger) *Parsing {
	p := &Parsing{st: st, orgID: orgID, parsers: parsers, server: server, logger: logger,
		requeued: map[string]bool{}, unknown: map[string]bool{},
		parsed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gravel_ingest_parse_total",
			Help: "Stored batches the parse job read, by server, source and result (parsed, failed).",
		}, []string{"server", "source", "result"}),
		events: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gravel_ingest_events_total",
			Help: "Events parsed from the stored batches and stored, by server and kind; unknown is a kind the parser does not know (kept in the batch), invalid one it could not read.",
		}, []string{"server", "kind"}),
		duplicates: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gravel_ingest_duplicates_total",
			Help: "Events dropped because their server had sent the same source key before.",
		}, []string{"server"}),
	}
	reg.MustRegister(p.parsed, p.events, p.duplicates)
	return p
}

// Run reads every pending batch of every source with a parser. The first run of each source
// first puts back the batches an older parser read. A batch whose server cannot be read stays
// pending and fails the run after the others are read; the store failing stops it at once.
func (p *Parsing) Run(ctx context.Context) error {
	var skipped []error
	// A run asks for each server once, and reports one that cannot be read once.
	type info struct {
		game, trust string
		err         error
	}
	servers := map[string]info{}
	for _, source := range slices.Sorted(maps.Keys(p.parsers)) {
		if err := p.requeue(ctx, source); err != nil {
			return err
		}
		skip := map[int64]bool{}
		for {
			// Ask past the batches skipped already, so they cannot fill a page.
			limit := parsePage + len(skip)
			batches, err := p.st.PendingIngestBatches(ctx, p.orgID, source, limit)
			if err != nil {
				return fmt.Errorf("ingest: parse %s: %w", source, err)
			}
			progressed := 0
			for _, b := range batches {
				if skip[b.ID] {
					continue
				}
				in, known := servers[b.ServerID]
				if !known {
					in.game, in.trust, in.err = p.server(ctx, b.ServerID)
					servers[b.ServerID] = in
					if in.err != nil {
						skipped = append(skipped, fmt.Errorf("ingest: parse: the batches of server %s wait: %w", b.ServerID, in.err))
					}
				}
				if in.err != nil {
					skip[b.ID] = true
					progressed++ // the next page asks past it
					continue
				}
				if err := p.parse(ctx, p.parsers[source], b, in.game, in.trust); err != nil {
					return err
				}
				progressed++
			}
			if progressed == 0 || len(batches) < limit || ctx.Err() != nil {
				break
			}
		}
	}
	return errors.Join(skipped...)
}

func (p *Parsing) requeue(ctx context.Context, source string) error {
	p.mu.Lock()
	done := p.requeued[source]
	p.mu.Unlock()
	if done {
		return nil
	}
	version := p.parsers[source].Version()
	n, err := p.st.RequeueIngestBatches(ctx, p.orgID, source, version)
	if err != nil {
		return fmt.Errorf("ingest: requeue %s: %w", source, err)
	}
	if n > 0 {
		p.logger.InfoContext(ctx, "ingest: a newer parser reads the stored batches again", "source", source, "version", version, "batches", n)
	}
	p.mu.Lock()
	p.requeued[source] = true
	p.mu.Unlock()
	return nil
}

// parse reads one batch and saves what it found. Only the store failing is an error: a batch the
// parser cannot read is saved as failed.
func (p *Parsing) parse(ctx context.Context, parser Parser, b store.IngestBatch, game, trust string) error {
	version := parser.Version()
	got, perr := parser.Parse(Batch{ServerID: b.ServerID, ReceivedAt: b.ReceivedAt, Body: b.Body, Headers: b.Headers})
	save := store.ParsedBatch{BatchID: b.ID, State: store.IngestParsed, ParserVersion: version}
	if perr != nil {
		save.State = store.IngestFailed
		got = Parsed{}
	}
	for _, e := range got.Events {
		ext, err := json.Marshal(e.Extensions)
		if err != nil || e.Key == "" || e.Kind == "" {
			got.Invalid++
			continue
		}
		if e.Extensions == nil {
			ext = []byte(`{}`)
		}
		at := e.ObservedAt
		if at.IsZero() {
			at = b.ReceivedAt
		}
		save.Events = append(save.Events, store.StatsEvent{OrganizationID: p.orgID, ServerID: b.ServerID, GameID: game,
			Source: SourceServerAdapter, SourceKey: e.Key, Kind: e.Kind, ObservedAt: at, ReceivedAt: b.ReceivedAt, Trust: trust,
			BatchID: b.ID, SchemaVersion: StatsSchemaVersion, Player: e.Player, Target: e.Target, Extensions: ext})
	}
	stored, err := p.st.SaveParsedBatch(ctx, save)
	if err != nil {
		return fmt.Errorf("ingest: parse batch %d: %w", b.ID, err)
	}
	if perr != nil {
		p.parsed.WithLabelValues(b.ServerID, b.Source, store.IngestFailed).Inc()
		p.logger.WarnContext(ctx, "ingest: a batch the parser could not read; it is kept and read again by a newer parser",
			"server", b.ServerID, "source", b.Source, "batch", b.ID, "parser_version", version, "error", perr.Error())
		return nil
	}
	p.parsed.WithLabelValues(b.ServerID, b.Source, store.IngestParsed).Inc()
	for i, ok := range stored {
		switch {
		case ok:
			p.events.WithLabelValues(b.ServerID, save.Events[i].Kind).Inc()
		case b.ParserVersion == 0:
			// A batch read again (a newer parser) finds its own events stored: not a duplicate
			// the source sent.
			p.duplicates.WithLabelValues(b.ServerID).Inc()
		}
	}
	if got.Invalid > 0 {
		p.events.WithLabelValues(b.ServerID, "invalid").Add(float64(got.Invalid))
	}
	for _, kind := range got.Unknown {
		p.events.WithLabelValues(b.ServerID, "unknown").Inc()
		p.mu.Lock()
		first := !p.unknown[b.ServerID+"\x00"+kind]
		p.unknown[b.ServerID+"\x00"+kind] = true
		p.mu.Unlock()
		if first {
			p.logger.InfoContext(ctx, "ingest: an event of a kind the parser does not know; it stays in the stored batch for a parser that does",
				"server", b.ServerID, "source", b.Source, "type", kind, "parser_version", version)
		}
	}
	return nil
}
