package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// StatsEvent is a row of stats_events (ADR-0011 §4): one event a source reported, with where it
// came from. Player and Target are zero for an event of nobody; BatchID is 0 for an event no batch
// carried (a gap the hub found).
type StatsEvent struct {
	ID             int64
	OrganizationID uuid.UUID
	ServerID       string
	GameID         string
	Source         string
	SourceKey      string
	Kind           string
	ObservedAt     time.Time
	ReceivedAt     time.Time
	Trust          string
	BatchID        int64
	SchemaVersion  int
	Player         PlayerKey
	Target         PlayerKey
	// Extensions are the kind's own fields, a JSON object; empty is {}.
	Extensions json.RawMessage
}

// ParsedBatch is what a parser made of a stored batch: its new state, the parser's version, and
// the events to store.
type ParsedBatch struct {
	BatchID       int64
	State         string
	ParserVersion int
	Events        []StatsEvent
}

const statsEventColumns = `id, organization_id, server_id, game_id, source, source_key, kind, observed_at, received_at, trust,
	coalesce(batch_id, 0), schema_version, coalesce(provider, ''), coalesce(subject, ''), coalesce(target_provider, ''),
	coalesce(target_subject, ''), extensions`

func scanStatsEvent(row pgx.Row) (StatsEvent, error) {
	var e StatsEvent
	err := row.Scan(&e.ID, &e.OrganizationID, &e.ServerID, &e.GameID, &e.Source, &e.SourceKey, &e.Kind, &e.ObservedAt, &e.ReceivedAt,
		&e.Trust, &e.BatchID, &e.SchemaVersion, &e.Player.Provider, &e.Player.Subject, &e.Target.Provider, &e.Target.Subject, &e.Extensions)
	return e, err
}

// queueStatsEvents queues each event's insert; a source key the server's source has used already
// inserts nothing and returns no row.
func queueStatsEvents(batch *pgx.Batch, events []StatsEvent) {
	null := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	for _, e := range events {
		ext := e.Extensions
		if len(ext) == 0 {
			ext = json.RawMessage(`{}`)
		}
		var batchID *int64
		if e.BatchID != 0 {
			batchID = &e.BatchID
		}
		batch.Queue(`INSERT INTO stats_events (organization_id, server_id, game_id, source, source_key, kind, observed_at,
			  received_at, trust, batch_id, schema_version, provider, subject, target_provider, target_subject, extensions)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
			ON CONFLICT (organization_id, server_id, source, source_key) DO UPDATE SET kind = EXCLUDED.kind,
			  observed_at = EXCLUDED.observed_at, schema_version = EXCLUDED.schema_version, provider = EXCLUDED.provider,
			  subject = EXCLUDED.subject, target_provider = EXCLUDED.target_provider, target_subject = EXCLUDED.target_subject,
			  extensions = EXCLUDED.extensions
			WHERE stats_events.batch_id = EXCLUDED.batch_id
			RETURNING id, xmax = 0`,
			e.OrganizationID, e.ServerID, e.GameID, e.Source, e.SourceKey, e.Kind, e.ObservedAt, e.ReceivedAt, e.Trust, batchID,
			e.SchemaVersion, null(e.Player.Provider), null(e.Player.Subject), null(e.Target.Provider), null(e.Target.Subject), ext)
	}
}

// insertStatsEvents runs the queued inserts and reports, for each event in order, whether it was
// stored new: false is a duplicate, or an event its own batch carried before, read again by a newer
// parser, whose fields are refreshed (a field the older parser did not read; never its trust or when
// it was received). A key from another batch, or an event no batch carried, is never touched.
func insertStatsEvents(ctx context.Context, tx pgx.Tx, events []StatsEvent, then func(*pgx.Batch)) ([]bool, error) {
	batch := &pgx.Batch{}
	queueStatsEvents(batch, events)
	if then != nil {
		then(batch)
	}
	br := tx.SendBatch(ctx, batch)
	stored := make([]bool, len(events))
	for i := range events {
		var id int64
		var inserted bool
		switch err := br.QueryRow().Scan(&id, &inserted); {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			_ = br.Close()
			return nil, err
		default:
			stored[i] = inserted
		}
	}
	if then != nil {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return nil, err
		}
	}
	return stored, br.Close()
}

// RecordStatsEvents stores events no batch carried (a gap the hub found) in one transaction and
// reports, for each in order, whether it was stored: false is a duplicate.
func (s *Store) RecordStatsEvents(ctx context.Context, events []StatsEvent) ([]bool, error) {
	if len(events) == 0 {
		return nil, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: record stats events: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	stored, err := insertStatsEvents(ctx, tx, events, nil)
	if err != nil {
		return nil, fmt.Errorf("store: record stats events: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: record stats events: %w", err)
	}
	return stored, nil
}

// SaveParsedBatch stores a parsed batch's events and moves the batch to its new state, in one
// transaction, and reports for each event in order whether it was stored new. False is a duplicate
// (a source key the server's source had used: an earlier batch, or earlier in this one, which is
// dropped), or an event an earlier reading of this batch stored, which takes this reading's fields.
func (s *Store) SaveParsedBatch(ctx context.Context, p ParsedBatch) ([]bool, error) {
	if p.State != IngestParsed && p.State != IngestFailed {
		return nil, fmt.Errorf("store: save parsed batch: state %q", p.State)
	}
	// A key twice in one batch is the source repeating itself: the first stands, the rest are
	// duplicates and never reach the refresh an earlier reading's rows get.
	var first []StatsEvent
	at := make([]int, len(p.Events)) // each event's place in first, or -1
	seen := map[string]bool{}
	for i, e := range p.Events {
		k := e.ServerID + "\x00" + e.Source + "\x00" + e.SourceKey
		if seen[k] {
			at[i] = -1
			continue
		}
		seen[k] = true
		at[i] = len(first)
		first = append(first, e)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: save parsed batch: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	got, err := insertStatsEvents(ctx, tx, first, func(b *pgx.Batch) {
		b.Queue(`UPDATE ingest_batches SET state = $2, parser_version = $3 WHERE id = $1`, p.BatchID, p.State, p.ParserVersion)
	})
	if err != nil {
		return nil, fmt.Errorf("store: save parsed batch %d: %w", p.BatchID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: save parsed batch: %w", err)
	}
	stored := make([]bool, len(p.Events))
	for i, j := range at {
		stored[i] = j >= 0 && got[j]
	}
	return stored, nil
}

// PendingIngestBatches returns up to limit of a source's batches waiting for its parser, oldest
// first.
func (s *Store) PendingIngestBatches(ctx context.Context, orgID uuid.UUID, source string, limit int) ([]IngestBatch, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+ingestColumns+` FROM ingest_batches
		WHERE source = $2 AND state = 'pending' AND organization_id = $1 ORDER BY id LIMIT $3`, orgID, source, limit)
	if err != nil {
		return nil, fmt.Errorf("store: pending ingest batches: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (IngestBatch, error) { return scanIngestBatch(r) })
	if err != nil {
		return nil, fmt.Errorf("store: pending ingest batches: %w", err)
	}
	return out, nil
}

// RequeueIngestBatches puts back to pending every batch of a source that a parser older than
// version read, and reports how many.
func (s *Store) RequeueIngestBatches(ctx context.Context, orgID uuid.UUID, source string, version int) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE ingest_batches SET state = 'pending'
		WHERE organization_id = $1 AND source = $2 AND state <> 'pending' AND parser_version < $3`, orgID, source, version)
	if err != nil {
		return 0, fmt.Errorf("store: requeue ingest batches: %w", err)
	}
	return tag.RowsAffected(), nil
}

// LastIngestBatch is when the newest batch a server pushed before a time was received; ok is
// false when there is none.
func (s *Store) LastIngestBatch(ctx context.Context, orgID uuid.UUID, serverID string, before time.Time) (at time.Time, ok bool, err error) {
	err = s.pool.QueryRow(ctx, `SELECT received_at FROM ingest_batches
		WHERE organization_id = $1 AND server_id = $2 AND received_at < $3 ORDER BY id DESC LIMIT 1`, orgID, serverID, before).Scan(&at)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return time.Time{}, false, nil
	case err != nil:
		return time.Time{}, false, fmt.Errorf("store: last ingest batch: %w", err)
	}
	return at, true, nil
}

// HadPlayers reports whether the poll saw anyone on a server in the minute of at or the one
// before it (server_occupancy, ADR-0018, has a row only for a minute anyone was on).
func (s *Store) HadPlayers(ctx context.Context, orgID uuid.UUID, serverID string, at time.Time) (bool, error) {
	var had bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM server_occupancy
		WHERE organization_id = $1 AND server_id = $2 AND max_players > 0
		  AND minute BETWEEN date_trunc('minute', $3::timestamptz) - interval '1 minute' AND date_trunc('minute', $3::timestamptz))`,
		orgID, serverID, at).Scan(&had)
	if err != nil {
		return false, fmt.Errorf("store: had players: %w", err)
	}
	return had, nil
}

// StatsEvents returns up to limit of a server's events of a kind observed in [from, to), oldest
// first; kind "" is every kind.
func (s *Store) StatsEvents(ctx context.Context, orgID uuid.UUID, serverID, kind string, from, to time.Time, limit int) ([]StatsEvent, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+statsEventColumns+` FROM stats_events
		WHERE organization_id = $1 AND server_id = $2 AND ($3 = '' OR kind = $3) AND observed_at >= $4 AND observed_at < $5
		ORDER BY observed_at, id LIMIT $6`, orgID, serverID, kind, from, to, limit)
	if err != nil {
		return nil, fmt.Errorf("store: stats events: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (StatsEvent, error) { return scanStatsEvent(r) })
	if err != nil {
		return nil, fmt.Errorf("store: stats events: %w", err)
	}
	return out, nil
}

// RetireStatsEvents anonymises the players of every event observed before cutoff (ADR-0012 §6;
// README principle 10, as #216 does for match results): each identity on each event becomes
// (ErasedProvider, a random token of its own), so the events stay countable and name nobody.
// It reports how many events it changed; an event already anonymised is not touched again.
func (s *Store) RetireStatsEvents(ctx context.Context, orgID uuid.UUID, cutoff time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE stats_events SET
		  provider = CASE WHEN provider IS NULL THEN NULL ELSE $3 END,
		  subject = CASE WHEN provider IS NULL OR provider = $3 THEN subject ELSE replace(gen_random_uuid()::text, '-', '') END,
		  target_provider = CASE WHEN target_provider IS NULL THEN NULL ELSE $3 END,
		  target_subject = CASE WHEN target_provider IS NULL OR target_provider = $3 THEN target_subject ELSE replace(gen_random_uuid()::text, '-', '') END
		WHERE organization_id = $1 AND observed_at < $2
		  AND ((provider IS NOT NULL AND provider <> $3) OR (target_provider IS NOT NULL AND target_provider <> $3))`,
		orgID, cutoff, ErasedProvider)
	if err != nil {
		return 0, fmt.Errorf("store: retire stats events: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
