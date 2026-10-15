package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ExtensionTotal is one of a game's own stats (ADR-0033) over a player's maps: the sum, how many
// maps reported it and the largest value on one map, so a board can show a sum, an average per map
// or a best.
type ExtensionTotal struct {
	Total float64
	Maps  int
	Max   float64
}

// BoardExtensions are the extension totals of the given players over the query's window, server or
// game and trusts (Metric, MinMatches, Limit and Offset are not used): their match results (by a
// series' start) and the periods rolled up past retention (result_stats_monthly, by their first
// day in Timezone), keyed by player, then extension name. A player with none is absent.
func (s *Store) BoardExtensions(ctx context.Context, q BoardQuery, keys []PlayerKey) (map[PlayerKey]map[string]ExtensionTotal, error) {
	out := map[PlayerKey]map[string]ExtensionTotal{}
	if len(keys) == 0 {
		return out, nil
	}
	var from, to *time.Time
	if !q.From.IsZero() {
		from = &q.From
	}
	if !q.To.IsZero() {
		to = &q.To
	}
	tz := q.Timezone
	if tz == "" {
		tz = "UTC"
	}
	providers, subjects := splitKeys(keys)
	rows, err := s.pool.Query(ctx, `
		WITH k AS (SELECT * FROM unnest($7::text[], $8::text[]) AS k(provider, subject))
		SELECT x.provider, x.subject, x.name, sum(x.total), sum(x.maps)::int, max(x.max)
		  FROM (
		    SELECT p.provider, p.subject, e.key AS name, (e.value #>> '{}')::float8 AS total, 1 AS maps, (e.value #>> '{}')::float8 AS max
		      FROM match_result_players p
		      JOIN k ON k.provider = p.provider AND k.subject = p.subject
		      JOIN match_results r ON r.id = p.result_id
		      JOIN managed_servers ms ON ms.organization_id = r.organization_id AND ms.id = r.server_id
		      CROSS JOIN LATERAL jsonb_each(p.extensions) e
		     WHERE r.organization_id = $1 AND jsonb_typeof(e.value) = 'number'
		       AND ($2 = '' OR r.server_id = $2) AND ($3 = '' OR ms.game_id = $3)
		       AND ($4::timestamptz IS NULL OR r.started_at >= $4) AND ($5::timestamptz IS NULL OR r.started_at < $5)
		       AND r.trust = ANY($6)
		    UNION ALL
		    SELECT rs.provider, rs.subject, rs.name, rs.total, rs.maps, rs.max
		      FROM result_stats_monthly rs
		      JOIN k ON k.provider = rs.provider AND k.subject = rs.subject
		     WHERE rs.organization_id = $1
		       AND ($2 = '' OR rs.server_id = $2) AND ($3 = '' OR rs.game_id = $3)
		       AND ($4::timestamptz IS NULL OR rs.period_from >= ($4::timestamptz AT TIME ZONE $9)::date)
		       AND ($5::timestamptz IS NULL OR rs.period_from < ($5::timestamptz AT TIME ZONE $9)::date)
		       AND rs.trust = ANY($6)
		  ) x
		 GROUP BY x.provider, x.subject, x.name`,
		q.OrganizationID, q.ServerID, q.GameID, from, to, q.Trusts, providers, subjects, tz)
	if err != nil {
		return nil, fmt.Errorf("store: board extensions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k PlayerKey
		var name string
		var t ExtensionTotal
		if err := rows.Scan(&k.Provider, &k.Subject, &name, &t.Total, &t.Maps, &t.Max); err != nil {
			return nil, fmt.Errorf("store: board extensions: %w", err)
		}
		if out[k] == nil {
			out[k] = map[string]ExtensionTotal{}
		}
		out[k][name] = t
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: board extensions: %w", err)
	}
	return out, nil
}

// OldestRetirableResult is the start of the organization's oldest series that ended before cutoff
// and still has a player RetireResults has not anonymised, or ErrNotFound when none has: where the
// periods RetireResults rolls up into must begin.
func (s *Store) OldestRetirableResult(ctx context.Context, orgID uuid.UUID, cutoff time.Time) (time.Time, error) {
	var at *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT min(r.started_at) FROM match_results r
		WHERE r.organization_id = $1 AND r.ended_at IS NOT NULL AND r.ended_at < $2
		  AND EXISTS (SELECT 1 FROM match_result_players p WHERE p.result_id = r.id AND p.provider <> $3)`,
		orgID, cutoff, ErasedProvider).Scan(&at); err != nil {
		return time.Time{}, fmt.Errorf("store: oldest retirable result: %w", err)
	}
	if at == nil {
		return time.Time{}, ErrNotFound
	}
	return *at, nil
}

// ServerGame is the game a server runs (a removed one included), or ErrNotFound.
func (s *Store) ServerGame(ctx context.Context, orgID uuid.UUID, serverID string) (string, error) {
	var game string
	err := s.pool.QueryRow(ctx, `SELECT game_id FROM managed_servers WHERE organization_id = $1 AND id = $2`, orgID, serverID).Scan(&game)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: server game: %w", err)
	}
	return game, nil
}
