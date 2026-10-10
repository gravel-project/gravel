package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Period is a rollup period (ADR-0012 §6): dates in the organization's timezone, From included,
// To excluded.
type Period struct {
	From, To time.Time // midnight dates; only the date counts
}

// OldestRawMatch is the start of the organization's oldest match that still has match_stats rows,
// or ErrNotFound when none has.
func (s *Store) OldestRawMatch(ctx context.Context, orgID uuid.UUID) (time.Time, error) {
	var at *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT min(m.started_at) FROM matches m
		WHERE m.organization_id = $1 AND EXISTS (SELECT 1 FROM match_stats ms WHERE ms.match_id = m.id)`, orgID).Scan(&at); err != nil {
		return time.Time{}, fmt.Errorf("store: oldest raw match: %w", err)
	}
	if at == nil {
		return time.Time{}, ErrNotFound
	}
	return *at, nil
}

// Rollup is what RollUp moved.
type Rollup struct {
	Matches int // matches whose rows rolled up
	Rows    int // match_stats rows deleted
}

// RollUp moves the match_stats rows of every match that started in one of the periods (its start
// read in timezone) into stats_monthly, adding to a period's row when one exists, records each
// match's player count and kills on the match, and deletes the raw rows, in one transaction.
// Periods must not overlap.
func (s *Store) RollUp(ctx context.Context, orgID uuid.UUID, timezone string, periods []Period) (Rollup, error) {
	if len(periods) == 0 {
		return Rollup{}, nil
	}
	froms, tos := make([]time.Time, len(periods)), make([]time.Time, len(periods))
	for i, p := range periods {
		froms[i], tos[i] = dateOnly(p.From), dateOnly(p.To)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Rollup{}, fmt.Errorf("store: roll up: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The matches to roll, each with its period, kept for the three statements below.
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE rolling ON COMMIT DROP AS
		SELECT m.id AS match_id, m.server_id, m.game_id, p.f AS period_from, p.t AS period_to
		  FROM matches m
		  JOIN unnest($2::date[], $3::date[]) AS p(f, t)
		    ON (m.started_at AT TIME ZONE $4)::date >= p.f AND (m.started_at AT TIME ZONE $4)::date < p.t
		 WHERE m.organization_id = $1 AND EXISTS (SELECT 1 FROM match_stats ms WHERE ms.match_id = m.id)`,
		orgID, froms, tos, timezone); err != nil {
		return Rollup{}, fmt.Errorf("store: roll up: select: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO stats_monthly
		  (organization_id, server_id, game_id, period_from, period_to, provider, subject, trust, kills, deaths, seconds_on, matches)
		SELECT $1, r.server_id, r.game_id, r.period_from, r.period_to, ms.provider, ms.subject, ms.trust,
		       sum(ms.kills), sum(ms.deaths), sum(ms.seconds_on), count(*)
		  FROM rolling r JOIN match_stats ms ON ms.match_id = r.match_id
		 GROUP BY r.server_id, r.game_id, r.period_from, r.period_to, ms.provider, ms.subject, ms.trust
		ON CONFLICT (organization_id, server_id, period_from, provider, subject, trust) DO UPDATE SET
		  kills = stats_monthly.kills + excluded.kills, deaths = stats_monthly.deaths + excluded.deaths,
		  seconds_on = stats_monthly.seconds_on + excluded.seconds_on, matches = stats_monthly.matches + excluded.matches`,
		orgID); err != nil {
		return Rollup{}, fmt.Errorf("store: roll up: aggregate: %w", err)
	}
	tag, err := tx.Exec(ctx, `UPDATE matches m SET players = coalesce(m.players, 0) + t.players, kills = coalesce(m.kills, 0) + t.kills
		  FROM (SELECT ms.match_id, count(*) AS players, sum(ms.kills) AS kills
		          FROM match_stats ms JOIN rolling r ON r.match_id = ms.match_id GROUP BY ms.match_id) t
		 WHERE m.id = t.match_id`)
	if err != nil {
		return Rollup{}, fmt.Errorf("store: roll up: matches: %w", err)
	}
	out := Rollup{Matches: int(tag.RowsAffected())}
	if tag, err = tx.Exec(ctx, `DELETE FROM match_stats ms USING rolling r WHERE ms.match_id = r.match_id`); err != nil {
		return Rollup{}, fmt.Errorf("store: roll up: delete: %w", err)
	}
	out.Rows = int(tag.RowsAffected())
	if err := tx.Commit(ctx); err != nil {
		return Rollup{}, fmt.Errorf("store: roll up: %w", err)
	}
	return out, nil
}

// ErasedProvider is the provider an erased identity's rows carry (ADR-0012 §7).
const ErasedProvider = "erased"

// Erasure is what EraseIdentity re-keyed.
type Erasure struct {
	MatchRows     int // match_stats
	MonthlyRows   int // stats_monthly
	IngestBatches int // ingest_batches whose body named the identity
	Pseudonym     bool
}

// EraseIdentity replaces an identity with (ErasedProvider, token) in match_stats, stats_monthly
// and the bodies of the stored ingest batches, and deletes its pseudonym, in one transaction. The
// token must be random (a hash of a SteamID can be reversed by trying them all); the counts stay.
// ingest_batches is re-keyed by replacing the subject wherever it appears in a body, which is how
// a feed writes it.
func (s *Store) EraseIdentity(ctx context.Context, orgID uuid.UUID, key PlayerKey, token string) (Erasure, error) {
	if key.Provider == "" || key.Subject == "" || key.Provider == ErasedProvider || len(token) < 16 {
		return Erasure{}, errors.New("store: erase identity: a provider, a subject and a random token of at least 16 characters are required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Erasure{}, fmt.Errorf("store: erase identity: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var out Erasure
	tag, err := tx.Exec(ctx, `UPDATE match_stats ms SET provider = $4, subject = $5
		  FROM matches m WHERE m.id = ms.match_id AND m.organization_id = $1 AND ms.provider = $2 AND ms.subject = $3`,
		orgID, key.Provider, key.Subject, ErasedProvider, token)
	if err != nil {
		return Erasure{}, fmt.Errorf("store: erase identity: match_stats: %w", err)
	}
	out.MatchRows = int(tag.RowsAffected())
	if tag, err = tx.Exec(ctx, `UPDATE stats_monthly SET provider = $4, subject = $5
		WHERE organization_id = $1 AND provider = $2 AND subject = $3`, orgID, key.Provider, key.Subject, ErasedProvider, token); err != nil {
		return Erasure{}, fmt.Errorf("store: erase identity: stats_monthly: %w", err)
	}
	out.MonthlyRows = int(tag.RowsAffected())
	if tag, err = tx.Exec(ctx, `UPDATE ingest_batches
		   SET body = convert_to(replace(convert_from(body, 'UTF8'), $2, $3), 'UTF8'),
		       body_sha256 = sha256(convert_to(replace(convert_from(body, 'UTF8'), $2, $3), 'UTF8'))
		 WHERE organization_id = $1 AND position(convert_to($2, 'UTF8') IN body) > 0`, orgID, key.Subject, token); err != nil {
		return Erasure{}, fmt.Errorf("store: erase identity: ingest_batches: %w", err)
	}
	out.IngestBatches = int(tag.RowsAffected())
	if tag, err = tx.Exec(ctx, `DELETE FROM pseudonyms WHERE organization_id = $1 AND provider = $2 AND subject = $3`,
		orgID, key.Provider, key.Subject); err != nil {
		return Erasure{}, fmt.Errorf("store: erase identity: pseudonyms: %w", err)
	}
	out.Pseudonym = tag.RowsAffected() > 0
	if err := tx.Commit(ctx); err != nil {
		return Erasure{}, fmt.Errorf("store: erase identity: %w", err)
	}
	return out, nil
}

func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
