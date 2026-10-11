package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TeamScore is one team's (faction's) score in a match.
type TeamScore struct {
	Name  string `json:"name"`
	Color string `json:"color,omitempty"`
	Score int64  `json:"score"`
}

// RecordOccupancy folds one minute's player counts into server_occupancy: the fewest and the most
// players seen in the minute, kept as the least and the greatest of every write (ADR-0018). minute
// is truncated to the minute.
func (s *Store) RecordOccupancy(ctx context.Context, orgID uuid.UUID, serverID string, minute time.Time, fewest, most int) error {
	if fewest < 0 || most < fewest {
		return fmt.Errorf("store: record occupancy: %d to %d players is not a range", fewest, most)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO server_occupancy (organization_id, server_id, minute, min_players, max_players)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (organization_id, server_id, minute) DO UPDATE SET
		  min_players = least(server_occupancy.min_players, excluded.min_players),
		  max_players = greatest(server_occupancy.max_players, excluded.max_players)`,
		orgID, serverID, minute.UTC().Truncate(time.Minute), fewest, most); err != nil {
		return fmt.Errorf("store: record occupancy: %w", err)
	}
	return nil
}

// SetMatchTeams records a match's team scores as last seen.
func (s *Store) SetMatchTeams(ctx context.Context, matchID int64, teams []TeamScore) error {
	if teams == nil {
		teams = []TeamScore{}
	}
	raw, err := json.Marshal(teams)
	if err != nil {
		return fmt.Errorf("store: set match teams: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE matches SET teams = $2 WHERE id = $1`, matchID, raw); err != nil {
		return fmt.Errorf("store: set match teams: %w", err)
	}
	return nil
}

// RecapQuery is a session recap's window on one server.
type RecapQuery struct {
	OrganizationID uuid.UUID
	ServerID       string
	From, To       time.Time
	// Floor is the player count the minutes "at or above" are counted against; 0 counts none.
	Floor int
	// Top is how many players to rank by kills.
	Top int
}

// Recap is what a server's window held (ADR-0018): its occupancy, the matches that overlap it,
// and the players with the most kills in those matches.
type Recap struct {
	PeakPlayers int
	// Minutes is the minutes anyone was on; AtFloor those whose fewest players reached the floor.
	Minutes, AtFloor int
	// Players is the distinct identities seen in the window.
	Players int
	Matches []RecapMatch
	Top     []BoardRow
}

// RecapMatch is one match that overlaps a recap's window.
type RecapMatch struct {
	ID        int64
	StartedAt time.Time
	EndedAt   *time.Time
	Map       string
	Teams     []TeamScore
	Players   int
}

// SessionRecap reads a recap. A match counts when it overlaps the window; its players count when
// they were seen inside it. A player's kills are their match totals, so a match that straddles an
// edge brings its kills from outside the window too. Top lists only players with a kill.
func (s *Store) SessionRecap(ctx context.Context, q RecapQuery) (Recap, error) {
	var out Recap
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return Recap{}, fmt.Errorf("store: session recap: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tx.QueryRow(ctx, `SELECT coalesce(max(max_players), 0)::int, count(*)::int, count(*) FILTER (WHERE $5 > 0 AND min_players >= $5)::int
		  FROM server_occupancy
		 WHERE organization_id = $1 AND server_id = $2 AND minute >= $3 AND minute < $4`,
		q.OrganizationID, q.ServerID, q.From, q.To, q.Floor).Scan(&out.PeakPlayers, &out.Minutes, &out.AtFloor); err != nil {
		return Recap{}, fmt.Errorf("store: session recap: occupancy: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT m.id, m.started_at, m.ended_at, m.map, m.teams,
		       (SELECT count(*) FROM match_stats ms WHERE ms.match_id = m.id AND ms.last_seen >= $3 AND ms.first_seen < $4)::int
		  FROM matches m
		 WHERE m.organization_id = $1 AND m.server_id = $2 AND m.started_at < $4 AND (m.ended_at IS NULL OR m.ended_at > $3)
		 ORDER BY m.started_at, m.id`, q.OrganizationID, q.ServerID, q.From, q.To)
	if err != nil {
		return Recap{}, fmt.Errorf("store: session recap: matches: %w", err)
	}
	out.Matches, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (RecapMatch, error) {
		var m RecapMatch
		var teams []byte
		if err := r.Scan(&m.ID, &m.StartedAt, &m.EndedAt, &m.Map, &teams, &m.Players); err != nil {
			return m, err
		}
		if err := json.Unmarshal(teams, &m.Teams); err != nil {
			return m, fmt.Errorf("teams of match %d: %w", m.ID, err)
		}
		return m, nil
	})
	if err != nil {
		return Recap{}, fmt.Errorf("store: session recap: matches: %w", err)
	}
	ids := make([]int64, len(out.Matches))
	for i, m := range out.Matches {
		ids[i] = m.ID
	}
	if err := tx.QueryRow(ctx, `SELECT count(DISTINCT (provider, subject))::int FROM match_stats
		 WHERE match_id = ANY($1) AND last_seen >= $2 AND first_seen < $3`, ids, q.From, q.To).Scan(&out.Players); err != nil {
		return Recap{}, fmt.Errorf("store: session recap: players: %w", err)
	}
	if q.Top > 0 {
		rows, err := tx.Query(ctx, `SELECT provider, subject, sum(kills)::int, sum(deaths)::int, sum(seconds_on)::int, count(*)::int
			  FROM match_stats
			 WHERE match_id = ANY($1) AND last_seen >= $2 AND first_seen < $3
			 GROUP BY provider, subject
			HAVING sum(kills) > 0
			 ORDER BY sum(kills) DESC, sum(deaths), provider, subject
			 LIMIT $4`, ids, q.From, q.To, q.Top)
		if err != nil {
			return Recap{}, fmt.Errorf("store: session recap: top: %w", err)
		}
		out.Top, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (BoardRow, error) {
			var b BoardRow
			err := r.Scan(&b.Provider, &b.Subject, &b.Kills, &b.Deaths, &b.SecondsOn, &b.Matches)
			return b, err
		})
		if err != nil {
			return Recap{}, fmt.Errorf("store: session recap: top: %w", err)
		}
	}
	return out, nil
}
