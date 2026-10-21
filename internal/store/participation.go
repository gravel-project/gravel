package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ParticipationEvent is a row of participation_events (ADR-0014): one time a player took part in
// something a first-party app tracks, keyed by provider identity. GameID, ServerID and Session
// are empty when the app named none.
type ParticipationEvent struct {
	OrganizationID  uuid.UUID
	AppID           uuid.UUID
	Key             string
	Kind            string
	Provider        string
	Subject         string
	GameID          string
	ServerID        string
	Session         string
	OccurredAt      time.Time
	DurationSeconds int
	ReceivedAt      time.Time
	Source          string
	SchemaVersion   int
}

// RecordParticipation inserts events in one transaction and reports, for each in order, whether
// it was stored: false is a duplicate, a key its app had used already (in an earlier call or
// earlier in this one), which is left as first stored.
func (s *Store) RecordParticipation(ctx context.Context, events []ParticipationEvent) ([]bool, error) {
	if len(events) == 0 {
		return nil, nil
	}
	batch := &pgx.Batch{}
	for _, e := range events {
		batch.Queue(`INSERT INTO participation_events (organization_id, app_id, key, kind, provider, subject, game_id, server_id,
			  session, occurred_at, duration_seconds, received_at, source, schema_version)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
			ON CONFLICT (organization_id, app_id, key) DO NOTHING RETURNING id`,
			e.OrganizationID, e.AppID, e.Key, e.Kind, e.Provider, e.Subject, e.GameID, e.ServerID,
			e.Session, e.OccurredAt, e.DurationSeconds, e.ReceivedAt, e.Source, e.SchemaVersion)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: record participation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	br := tx.SendBatch(ctx, batch)
	stored := make([]bool, len(events))
	for i := range events {
		var id int64
		switch err := br.QueryRow().Scan(&id); {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			_ = br.Close()
			return nil, fmt.Errorf("store: record participation: %w", err)
		default:
			stored[i] = true
		}
	}
	if err := br.Close(); err != nil {
		return nil, fmt.Errorf("store: record participation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: record participation: %w", err)
	}
	return stored, nil
}

// ParticipationEvents are an organization's raw events, oldest first, for tests and checks.
func (s *Store) ParticipationEvents(ctx context.Context, orgID uuid.UUID) ([]ParticipationEvent, error) {
	rows, err := s.pool.Query(ctx, `SELECT organization_id, app_id, key, kind, provider, subject, game_id, server_id, session,
		  occurred_at, duration_seconds, received_at, source, schema_version
		  FROM participation_events WHERE organization_id = $1 ORDER BY occurred_at, id`, orgID)
	if err != nil {
		return nil, fmt.Errorf("store: participation events: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ParticipationEvent, error) {
		var e ParticipationEvent
		err := r.Scan(&e.OrganizationID, &e.AppID, &e.Key, &e.Kind, &e.Provider, &e.Subject, &e.GameID, &e.ServerID, &e.Session,
			&e.OccurredAt, &e.DurationSeconds, &e.ReceivedAt, &e.Source, &e.SchemaVersion)
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: participation events: %w", err)
	}
	return out, nil
}

// ParticipationBoardQuery selects and ranks the players who took part in one kind. An empty GameID
// is every event, with a game or without; a zero From or To is unbounded (events by when they
// happened). Rolled-up periods count by their first day, read in Timezone (UTC when empty), as on
// a combat board.
type ParticipationBoardQuery struct {
	OrganizationID uuid.UUID
	Kind           string
	GameID         string
	From, To       time.Time
	Timezone       string
	Limit, Offset  int
}

// ParticipationRow is one player's participation over a query's window.
type ParticipationRow struct {
	Provider string
	Subject  string
	Events   int64
	Seconds  int64
}

// participationRows is the raw events and the rolled-up periods of an organization over a window,
// as (provider, subject, kind, events, seconds); $1 organization, $2 kind (empty for every kind),
// $3 game, $4 from, $5 to, $6 timezone. Callers add their own identity filter in extra, with
// placeholders from $7.
func participationRows(extraRaw, extraMonthly string) string {
	return `
		    SELECT e.provider, e.subject, e.kind, 1::bigint AS events, e.duration_seconds::bigint AS seconds
		      FROM participation_events e ` + extraRaw + `
		     WHERE e.organization_id = $1 AND ($2 = '' OR e.kind = $2) AND ($3 = '' OR e.game_id = $3)
		       AND ($4::timestamptz IS NULL OR e.occurred_at >= $4) AND ($5::timestamptz IS NULL OR e.occurred_at < $5)
		    UNION ALL
		    SELECT pm.provider, pm.subject, pm.kind, pm.events, pm.seconds
		      FROM participation_monthly pm ` + extraMonthly + `
		     WHERE pm.organization_id = $1 AND ($2 = '' OR pm.kind = $2) AND ($3 = '' OR pm.game_id = $3)
		       AND ($4::timestamptz IS NULL OR pm.period_from >= ($4::timestamptz AT TIME ZONE $6)::date)
		       AND ($5::timestamptz IS NULL OR pm.period_from < ($5::timestamptz AT TIME ZONE $6)::date)`
}

func windowArgs(from, to time.Time, timezone string) (*time.Time, *time.Time, string) {
	var f, t *time.Time
	if !from.IsZero() {
		f = &from
	}
	if !to.IsZero() {
		t = &to
	}
	if timezone == "" {
		timezone = "UTC"
	}
	return f, t, timezone
}

// ParticipationBoard ranks the players by time taken part, then times, then identity, so the order
// is stable.
func (s *Store) ParticipationBoard(ctx context.Context, q ParticipationBoardQuery) ([]ParticipationRow, error) {
	if q.Kind == "" {
		return nil, errors.New("store: participation board: a kind is required")
	}
	from, to, tz := windowArgs(q.From, q.To, q.Timezone)
	rows, err := s.pool.Query(ctx, `
		SELECT x.provider, x.subject, sum(x.events)::bigint, sum(x.seconds)::bigint
		  FROM (`+participationRows("", "")+`) x
		 GROUP BY x.provider, x.subject
		 ORDER BY sum(x.seconds) DESC, sum(x.events) DESC, x.provider, x.subject
		 LIMIT $7 OFFSET $8`,
		q.OrganizationID, q.Kind, q.GameID, from, to, tz, q.Limit, q.Offset)
	if err != nil {
		return nil, fmt.Errorf("store: participation board: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ParticipationRow, error) {
		var p ParticipationRow
		err := r.Scan(&p.Provider, &p.Subject, &p.Events, &p.Seconds)
		return p, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: participation board: %w", err)
	}
	return out, nil
}

// ParticipationKinds are the kinds recorded in an organization, raw or rolled up, sorted: of one
// game's events, or of every event when game is "".
func (s *Store) ParticipationKinds(ctx context.Context, orgID uuid.UUID, game string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT kind FROM participation_events WHERE organization_id = $1 AND ($2 = '' OR game_id = $2)
		UNION SELECT kind FROM participation_monthly WHERE organization_id = $1 AND ($2 = '' OR game_id = $2) ORDER BY kind`, orgID, game)
	if err != nil {
		return nil, fmt.Errorf("store: participation kinds: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("store: participation kinds: %w", err)
	}
	return out, nil
}

// ParticipationTotalsQuery sums one player's participation (all their identities) over a window,
// per kind, like a participation board.
type ParticipationTotalsQuery struct {
	OrganizationID uuid.UUID
	Keys           []PlayerKey
	GameID         string // empty: every event
	From, To       time.Time
	Timezone       string
}

// KindTotals is a player's participation of one kind.
type KindTotals struct {
	Kind    string
	Events  int64
	Seconds int64
}

// ParticipationTotals is a player's participation per kind over the query's window, by kind; a
// kind they never took part in is absent.
func (s *Store) ParticipationTotals(ctx context.Context, q ParticipationTotalsQuery) ([]KindTotals, error) {
	if len(q.Keys) == 0 {
		return nil, nil
	}
	from, to, tz := windowArgs(q.From, q.To, q.Timezone)
	providers, subjects := splitKeys(q.Keys)
	const keyed = `JOIN unnest($7::text[], $8::text[]) AS k(provider, subject) ON k.provider = %[1]s.provider AND k.subject = %[1]s.subject`
	rows, err := s.pool.Query(ctx, `
		SELECT x.kind, sum(x.events)::bigint, sum(x.seconds)::bigint
		  FROM (`+participationRows(fmt.Sprintf(keyed, "e"), fmt.Sprintf(keyed, "pm"))+`) x
		 GROUP BY x.kind ORDER BY x.kind`,
		q.OrganizationID, "", q.GameID, from, to, tz, providers, subjects)
	if err != nil {
		return nil, fmt.Errorf("store: participation totals: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (KindTotals, error) {
		var k KindTotals
		err := r.Scan(&k.Kind, &k.Events, &k.Seconds)
		return k, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: participation totals: %w", err)
	}
	return out, nil
}

// OldestRawParticipation is when the organization's oldest raw participation event happened, or
// ErrNotFound when it has none.
func (s *Store) OldestRawParticipation(ctx context.Context, orgID uuid.UUID) (time.Time, error) {
	var at *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT min(occurred_at) FROM participation_events WHERE organization_id = $1`, orgID).Scan(&at); err != nil {
		return time.Time{}, fmt.Errorf("store: oldest raw participation: %w", err)
	}
	if at == nil {
		return time.Time{}, ErrNotFound
	}
	return *at, nil
}

// RollUpParticipation moves every participation event that happened in one of the periods (read
// in timezone) into participation_monthly, adding to a period's row when one exists, and deletes
// the raw rows, in one statement (so one transaction). It reports how many raw rows moved.
// Periods must not overlap.
func (s *Store) RollUpParticipation(ctx context.Context, orgID uuid.UUID, timezone string, periods []Period) (int, error) {
	if len(periods) == 0 {
		return 0, nil
	}
	froms, tos := make([]time.Time, len(periods)), make([]time.Time, len(periods))
	for i, p := range periods {
		froms[i], tos[i] = dateOnly(p.From), dateOnly(p.To)
	}
	// A data-modifying WITH runs to completion whether or not the outer query reads it.
	var moved int
	if err := s.pool.QueryRow(ctx, `
		WITH moved AS (
		  DELETE FROM participation_events e
		   USING unnest($2::date[], $3::date[]) AS p(f, t)
		   WHERE e.organization_id = $1
		     AND (e.occurred_at AT TIME ZONE $4)::date >= p.f AND (e.occurred_at AT TIME ZONE $4)::date < p.t
		  RETURNING e.kind, e.game_id, p.f AS period_from, p.t AS period_to, e.provider, e.subject, e.duration_seconds
		), added AS (
		  INSERT INTO participation_monthly (organization_id, kind, game_id, period_from, period_to, provider, subject, events, seconds)
		  SELECT $1, m.kind, m.game_id, m.period_from, m.period_to, m.provider, m.subject, count(*), sum(m.duration_seconds)
		    FROM moved m
		   GROUP BY m.kind, m.game_id, m.period_from, m.period_to, m.provider, m.subject
		  ON CONFLICT (organization_id, kind, game_id, period_from, provider, subject) DO UPDATE SET
		    events = participation_monthly.events + excluded.events, seconds = participation_monthly.seconds + excluded.seconds
		)
		SELECT count(*) FROM moved`, orgID, froms, tos, timezone).Scan(&moved); err != nil {
		return 0, fmt.Errorf("store: roll up participation: %w", err)
	}
	return moved, nil
}
