package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// FeedDelivery is what a pushing server last said of its delivery to the hub: events sent and
// dropped since it started, and when the poll read them.
type FeedDelivery struct {
	Sent, Dropped int64
	ObservedAt    time.Time
}

// LastFeedDelivery is a server's saved delivery; ok is false when none was saved.
func (s *Store) LastFeedDelivery(ctx context.Context, orgID uuid.UUID, serverID string) (d FeedDelivery, ok bool, err error) {
	err = s.pool.QueryRow(ctx, `SELECT sent_total, dropped_total, observed_at FROM feed_delivery
		WHERE organization_id = $1 AND server_id = $2`, orgID, serverID).Scan(&d.Sent, &d.Dropped, &d.ObservedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return FeedDelivery{}, false, nil
	case err != nil:
		return FeedDelivery{}, false, fmt.Errorf("store: last feed delivery: %w", err)
	}
	return d, true, nil
}

// SaveFeedDelivery writes a server's delivery in place of the last.
func (s *Store) SaveFeedDelivery(ctx context.Context, orgID uuid.UUID, serverID string, d FeedDelivery) error {
	if _, err := s.pool.Exec(ctx, `INSERT INTO feed_delivery (organization_id, server_id, sent_total, dropped_total, observed_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (organization_id, server_id) DO UPDATE SET
		  sent_total = excluded.sent_total, dropped_total = excluded.dropped_total, observed_at = excluded.observed_at`,
		orgID, serverID, d.Sent, d.Dropped, d.ObservedAt); err != nil {
		return fmt.Errorf("store: save feed delivery: %w", err)
	}
	return nil
}

// EndedMatches are a server's matches that ended in [from, to), oldest first.
func (s *Store) EndedMatches(ctx context.Context, orgID uuid.UUID, serverID string, from, to time.Time) ([]Match, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+matchColumns+` FROM matches
		WHERE organization_id = $1 AND server_id = $2 AND ended_at >= $3 AND ended_at < $4
		ORDER BY ended_at, id`, orgID, serverID, from, to)
	if err != nil {
		return nil, fmt.Errorf("store: ended matches: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Match, error) { return scanMatch(r) })
	if err != nil {
		return nil, fmt.Errorf("store: ended matches: %w", err)
	}
	return out, nil
}

// KillCount is one player's kills and deaths as a server's pushed events tell them.
type KillCount struct {
	Kills, Deaths int
}

// KillCounts counts a server's events of a kill kind observed in [from, to), per player: a kill
// for its player when that is not its target (a suicide is a death only, and the world, no
// player, kills nobody's count), a death for its target.
func (s *Store) KillCounts(ctx context.Context, orgID uuid.UUID, serverID, kind string, from, to time.Time) (map[PlayerKey]KillCount, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT provider, subject, sum(kills)::int, sum(deaths)::int FROM (
		  SELECT provider, subject, 1 AS kills, 0 AS deaths FROM stats_events
		  WHERE organization_id = $1 AND server_id = $2 AND kind = $3 AND observed_at >= $4 AND observed_at < $5
		    AND provider IS NOT NULL
		    AND (target_provider IS NULL OR (provider, subject) IS DISTINCT FROM (target_provider, target_subject))
		  UNION ALL
		  SELECT target_provider, target_subject, 0, 1 FROM stats_events
		  WHERE organization_id = $1 AND server_id = $2 AND kind = $3 AND observed_at >= $4 AND observed_at < $5
		    AND target_provider IS NOT NULL
		) c GROUP BY provider, subject`, orgID, serverID, kind, from, to)
	if err != nil {
		return nil, fmt.Errorf("store: kill counts: %w", err)
	}
	defer rows.Close()
	out := map[PlayerKey]KillCount{}
	for rows.Next() {
		var k PlayerKey
		var c KillCount
		if err := rows.Scan(&k.Provider, &k.Subject, &c.Kills, &c.Deaths); err != nil {
			return nil, fmt.Errorf("store: kill counts: %w", err)
		}
		out[k] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: kill counts: %w", err)
	}
	return out, nil
}
