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
	// ParticipationRows and ParticipationMonthlyRows are participation_events and
	// participation_monthly (ADR-0014).
	ParticipationRows        int
	ParticipationMonthlyRows int
	// ResultRows are match_result_players rows (ADR-0024), one per map the identity played, and
	// ResultMonthlyRows result_stats_monthly rows (ADR-0033), one per extension per period.
	ResultRows        int
	ResultMonthlyRows int
	// SessionRows and ActivityMonthlyRows are player_sessions and player_activity_monthly
	// (ADR-0035).
	SessionRows         int
	ActivityMonthlyRows int
	Pseudonym           bool
}

// EraseIdentity replaces an identity with (ErasedProvider, token) in match_stats, stats_monthly,
// participation_events, participation_monthly, match_result_players (whose in-game name it blanks),
// result_stats_monthly, player_sessions, player_activity_monthly and the bodies of the stored ingest batches, and deletes its pseudonym, in one transaction. The
// token must be random (a hash of a SteamID can be reversed by trying them all); the counts stay.
// ingest_batches is re-keyed by replacing the subject wherever it appears in a body, which is how
// a feed writes it.
func (s *Store) EraseIdentity(ctx context.Context, orgID uuid.UUID, key PlayerKey, token string, by Actor, at time.Time) (Erasure, error) {
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
	if tag, err = tx.Exec(ctx, `UPDATE participation_events SET provider = $4, subject = $5
		WHERE organization_id = $1 AND provider = $2 AND subject = $3`, orgID, key.Provider, key.Subject, ErasedProvider, token); err != nil {
		return Erasure{}, fmt.Errorf("store: erase identity: participation_events: %w", err)
	}
	out.ParticipationRows = int(tag.RowsAffected())
	if tag, err = tx.Exec(ctx, `UPDATE participation_monthly SET provider = $4, subject = $5
		WHERE organization_id = $1 AND provider = $2 AND subject = $3`, orgID, key.Provider, key.Subject, ErasedProvider, token); err != nil {
		return Erasure{}, fmt.Errorf("store: erase identity: participation_monthly: %w", err)
	}
	out.ParticipationMonthlyRows = int(tag.RowsAffected())
	if tag, err = tx.Exec(ctx, `UPDATE match_result_players p SET provider = $4, subject = $5, name = ''
		  FROM match_results r WHERE r.id = p.result_id AND r.organization_id = $1 AND p.provider = $2 AND p.subject = $3`,
		orgID, key.Provider, key.Subject, ErasedProvider, token); err != nil {
		return Erasure{}, fmt.Errorf("store: erase identity: match_result_players: %w", err)
	}
	out.ResultRows = int(tag.RowsAffected())
	if tag, err = tx.Exec(ctx, `UPDATE result_stats_monthly SET provider = $4, subject = $5
		WHERE organization_id = $1 AND provider = $2 AND subject = $3`, orgID, key.Provider, key.Subject, ErasedProvider, token); err != nil {
		return Erasure{}, fmt.Errorf("store: erase identity: result_stats_monthly: %w", err)
	}
	out.ResultMonthlyRows = int(tag.RowsAffected())
	if tag, err = tx.Exec(ctx, `UPDATE player_sessions SET provider = $4, subject = $5
		WHERE organization_id = $1 AND provider = $2 AND subject = $3`, orgID, key.Provider, key.Subject, ErasedProvider, token); err != nil {
		return Erasure{}, fmt.Errorf("store: erase identity: player_sessions: %w", err)
	}
	out.SessionRows = int(tag.RowsAffected())
	if tag, err = tx.Exec(ctx, `UPDATE player_activity_monthly SET provider = $4, subject = $5
		WHERE organization_id = $1 AND provider = $2 AND subject = $3`, orgID, key.Provider, key.Subject, ErasedProvider, token); err != nil {
		return Erasure{}, fmt.Errorf("store: erase identity: player_activity_monthly: %w", err)
	}
	out.ActivityMonthlyRows = int(tag.RowsAffected())
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
	// The event says an erasure happened and how much it touched; never whose (the subject is
	// what was erased).
	if err := s.appendEvent(ctx, tx, by.event(Event{OrganizationID: orgID, At: at, Kind: "stats.erased",
		Summary: "A player's identity was erased from the stats",
		Detail: map[string]any{"provider": key.Provider, "match_rows": out.MatchRows, "monthly_rows": out.MonthlyRows,
			"participation_rows": out.ParticipationRows, "result_rows": out.ResultRows, "result_monthly_rows": out.ResultMonthlyRows, "session_rows": out.SessionRows, "ingest_batches": out.IngestBatches},
		Source: "stats.erase", SourceID: fmt.Sprintf("%s:%d", orgID, at.UnixMicro())})); err != nil {
		return Erasure{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Erasure{}, fmt.Errorf("store: erase identity: %w", err)
	}
	return out, nil
}

// Retired is what RetireResults rolled up and anonymised.
type Retired struct {
	Results int // series whose players were re-keyed
	Rows    int // match_result_players rows
	// Rolled are the result_stats_monthly rows the players' extensions (ADR-0033) were added to.
	Rolled int
}

// RetireResults anonymises the players of every series that ended before cutoff (ADR-0024 as
// amended by #216): each player of a series gets (ErasedProvider, a random token of their own in
// that series), the same on each of its maps and unlinkable to any other series, and their in-game
// name is blanked. The scoreboard stays readable; nothing is deleted. A series that hasn't ended is
// never retired, and a retired row is not touched again.
//
// First, in the same transaction, each player's extensions (ADR-0033) are added to
// result_stats_monthly in the period holding the series' start (read in timezone), so the boards
// keep them. periods must hold the start of every series it retires (from OldestRetirableResult
// up to cutoff); one that doesn't fails the whole call before anything changes.
func (s *Store) RetireResults(ctx context.Context, orgID uuid.UUID, cutoff time.Time, timezone string, periods []Period) (Retired, error) {
	froms, tos := make([]time.Time, len(periods)), make([]time.Time, len(periods))
	for i, p := range periods {
		froms[i], tos[i] = dateOnly(p.From), dateOnly(p.To)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Retired{}, fmt.Errorf("store: retire results: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// One token per player per series, from Postgres' random UUIDs (gen_random_uuid, a CSPRNG),
	// with the period the series' start falls in.
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE retiring ON COMMIT DROP AS
		SELECT k.result_id, k.provider, k.subject, replace(gen_random_uuid()::text, '-', '') AS token, k.period_from, k.period_to
		  FROM (SELECT DISTINCT p.result_id, p.provider, p.subject, per.f AS period_from, per.t AS period_to
		          FROM match_result_players p JOIN match_results r ON r.id = p.result_id
		          LEFT JOIN unnest($4::date[], $5::date[]) AS per(f, t)
		            ON (r.started_at AT TIME ZONE $6)::date >= per.f AND (r.started_at AT TIME ZONE $6)::date < per.t
		         WHERE r.organization_id = $1 AND r.ended_at IS NOT NULL AND r.ended_at < $2 AND p.provider <> $3) k`,
		orgID, cutoff, ErasedProvider, froms, tos, timezone); err != nil {
		return Retired{}, fmt.Errorf("store: retire results: select: %w", err)
	}
	var outside int
	if err := tx.QueryRow(ctx, `SELECT count(DISTINCT result_id) FROM retiring WHERE period_from IS NULL`).Scan(&outside); err != nil {
		return Retired{}, fmt.Errorf("store: retire results: periods: %w", err)
	}
	if outside > 0 {
		return Retired{}, fmt.Errorf("store: retire results: %d series started outside the periods given; their extensions would be lost", outside)
	}
	tag, err := tx.Exec(ctx, `INSERT INTO result_stats_monthly
		  (organization_id, server_id, game_id, period_from, period_to, provider, subject, trust, name, total, maps, max)
		SELECT $1, r.server_id, ms.game_id, t.period_from, t.period_to, p.provider, p.subject, r.trust, e.key,
		       sum((e.value #>> '{}')::float8), count(*), max((e.value #>> '{}')::float8)
		  FROM retiring t
		  JOIN match_result_players p ON p.result_id = t.result_id AND p.provider = t.provider AND p.subject = t.subject
		  JOIN match_results r ON r.id = t.result_id
		  JOIN managed_servers ms ON ms.organization_id = r.organization_id AND ms.id = r.server_id
		  CROSS JOIN LATERAL jsonb_each(p.extensions) e
		 WHERE jsonb_typeof(e.value) = 'number'
		 GROUP BY r.server_id, ms.game_id, t.period_from, t.period_to, p.provider, p.subject, r.trust, e.key
		ON CONFLICT (organization_id, server_id, period_from, provider, subject, trust, name) DO UPDATE SET
		  total = result_stats_monthly.total + excluded.total, maps = result_stats_monthly.maps + excluded.maps,
		  max = greatest(result_stats_monthly.max, excluded.max)`, orgID)
	if err != nil {
		return Retired{}, fmt.Errorf("store: retire results: roll up: %w", err)
	}
	out := Retired{Rolled: int(tag.RowsAffected())}
	tag, err = tx.Exec(ctx, `UPDATE match_result_players p SET provider = $1, subject = t.token, name = ''
		  FROM retiring t WHERE p.result_id = t.result_id AND p.provider = t.provider AND p.subject = t.subject`, ErasedProvider)
	if err != nil {
		return Retired{}, fmt.Errorf("store: retire results: %w", err)
	}
	out.Rows = int(tag.RowsAffected())
	if err := tx.QueryRow(ctx, `SELECT count(DISTINCT result_id) FROM retiring`).Scan(&out.Results); err != nil {
		return Retired{}, fmt.Errorf("store: retire results: count: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Retired{}, fmt.Errorf("store: retire results: %w", err)
	}
	return out, nil
}

func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
