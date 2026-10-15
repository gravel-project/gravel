package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// MatchResult is a row of match_results: one series a match plugin ran (ADR-0024), with its maps
// and players when read whole.
type MatchResult struct {
	ID             uuid.UUID
	OrganizationID uuid.UUID
	ServerID       string
	LeaseID        *uuid.UUID
	Source         string // "matchzy"
	ExternalID     int64  // the plugin's match id
	Team1Name      string
	Team2Name      string
	NumMaps        int
	Team1Score     int
	Team2Score     int
	Winner         string // "", "team1", "team2", "none"
	StartedAt      time.Time
	EndedAt        *time.Time
	// Trust is the server's when the series started (README principle 11): "official" or
	// "community". StartMatchResult takes it from the server.
	Trust   string
	Maps    []MatchResultMap
	Players []MatchResultPlayer
}

// MatchResultMap is one map of a series.
type MatchResultMap struct {
	ResultID   uuid.UUID
	MapNumber  int
	MapName    string
	Team1Score int
	Team2Score int
	Winner     string
	StartedAt  *time.Time
	EndedAt    *time.Time
	DemoKey    string
	DemoBytes  int64
}

// MatchResultPlayer is one player's totals on one map, keyed by their provider identity.
type MatchResultPlayer struct {
	ResultID  uuid.UUID
	MapNumber int
	Provider  string
	Subject   string
	Name      string
	Team      string // "team1" or "team2"
	Kills     int
	Deaths    int
	// Extensions are the game's own stats on the map, by the names its spec declares
	// (stats.extensions, ADR-0033); one the source didn't report is absent, never 0.
	Extensions map[string]float64
}

const matchResultColumns = `id, organization_id, server_id, lease_id, source, external_id, team1_name, team2_name, num_maps, team1_score,
	team2_score, winner, started_at, ended_at, trust`

func scanMatchResult(row pgx.Row) (MatchResult, error) {
	var r MatchResult
	err := row.Scan(&r.ID, &r.OrganizationID, &r.ServerID, &r.LeaseID, &r.Source, &r.ExternalID, &r.Team1Name, &r.Team2Name, &r.NumMaps,
		&r.Team1Score, &r.Team2Score, &r.Winner, &r.StartedAt, &r.EndedAt, &r.Trust)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// StartMatchResult records a series as it starts, or finds it: a plugin's series_start for a
// match id the server already has (a repeated event) updates its teams and keeps the rest.
func (s *Store) StartMatchResult(ctx context.Context, r MatchResult) (MatchResult, error) {
	got, err := scanMatchResult(s.pool.QueryRow(ctx, `INSERT INTO match_results (id, organization_id, server_id, lease_id, source, external_id,
		team1_name, team2_name, num_maps, started_at, trust) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
		  (SELECT trust FROM managed_servers WHERE organization_id = $2 AND id = $3))
		ON CONFLICT (organization_id, server_id, source, external_id) DO UPDATE SET
		  team1_name = CASE WHEN excluded.team1_name = '' THEN match_results.team1_name ELSE excluded.team1_name END,
		  team2_name = CASE WHEN excluded.team2_name = '' THEN match_results.team2_name ELSE excluded.team2_name END,
		  num_maps = excluded.num_maps, lease_id = COALESCE(match_results.lease_id, excluded.lease_id)
		RETURNING `+matchResultColumns,
		r.ID, r.OrganizationID, r.ServerID, r.LeaseID, r.Source, r.ExternalID, r.Team1Name, r.Team2Name, max(r.NumMaps, 1), r.StartedAt))
	if err != nil {
		return MatchResult{}, fmt.Errorf("store: start match result: %w", err)
	}
	return got, nil
}

// MatchResultFor is a server's series by the plugin's match id, or ErrNotFound.
func (s *Store) MatchResultFor(ctx context.Context, orgID uuid.UUID, serverID, source string, externalID int64) (MatchResult, error) {
	r, err := scanMatchResult(s.pool.QueryRow(ctx, `SELECT `+matchResultColumns+` FROM match_results
		WHERE organization_id = $1 AND server_id = $2 AND source = $3 AND external_id = $4`, orgID, serverID, source, externalID))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return r, fmt.Errorf("store: match result: %w", err)
	}
	return r, err
}

// EndMatchResult records a series' end: its score and winner.
func (s *Store) EndMatchResult(ctx context.Context, id uuid.UUID, team1, team2 int, winner string, at time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: end match result: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var orgID uuid.UUID
	var server, name1, name2 string
	err = tx.QueryRow(ctx, `UPDATE match_results SET team1_score = $2, team2_score = $3, winner = $4, ended_at = $5 WHERE id = $1
		RETURNING organization_id, server_id, team1_name, team2_name`, id, team1, team2, winner, at).Scan(&orgID, &server, &name1, &name2)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: end match result: %w", err)
	}
	if err := s.appendEvent(ctx, tx, Event{OrganizationID: orgID, At: at, Kind: "match.ended", ServerID: server,
		Summary: fmt.Sprintf("%s %d–%d %s on %s", name1, team1, team2, name2, server), Detail: map[string]any{"result_id": id.String(), "winner": winner},
		Source: "match_results", SourceID: id.String()}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: end match result: %w", err)
	}
	return nil
}

// SaveMatchResultMap writes a map's state: a name, scores and a winner as they come, times only
// once (the first start, the first end).
func (s *Store) SaveMatchResultMap(ctx context.Context, m MatchResultMap) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO match_result_maps (result_id, map_number, map_name, team1_score, team2_score, winner, started_at, ended_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (result_id, map_number) DO UPDATE SET
		  map_name = CASE WHEN excluded.map_name = '' THEN match_result_maps.map_name ELSE excluded.map_name END,
		  team1_score = excluded.team1_score, team2_score = excluded.team2_score,
		  winner = CASE WHEN excluded.winner = '' THEN match_result_maps.winner ELSE excluded.winner END,
		  started_at = COALESCE(match_result_maps.started_at, excluded.started_at),
		  ended_at = COALESCE(match_result_maps.ended_at, excluded.ended_at)`,
		m.ResultID, m.MapNumber, m.MapName, m.Team1Score, m.Team2Score, m.Winner, m.StartedAt, m.EndedAt)
	if err != nil {
		return fmt.Errorf("store: save match map: %w", err)
	}
	return nil
}

// SetMatchResultDemo records a map's demo in object storage, making the map's row if it has none.
func (s *Store) SetMatchResultDemo(ctx context.Context, resultID uuid.UUID, mapNumber int, key string, bytes int64) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO match_result_maps (result_id, map_number, demo_key, demo_bytes) VALUES ($1, $2, $3, $4)
		ON CONFLICT (result_id, map_number) DO UPDATE SET demo_key = excluded.demo_key, demo_bytes = excluded.demo_bytes`,
		resultID, mapNumber, key, bytes)
	if err != nil {
		return fmt.Errorf("store: set match demo: %w", err)
	}
	return nil
}

// ReplaceMatchResultPlayers replaces a map's player totals: a plugin reports running totals, so the
// newest report is the whole truth.
func (s *Store) ReplaceMatchResultPlayers(ctx context.Context, resultID uuid.UUID, mapNumber int, players []MatchResultPlayer) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: match players: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM match_result_players WHERE result_id = $1 AND map_number = $2`, resultID, mapNumber); err != nil {
		return fmt.Errorf("store: match players: %w", err)
	}
	for _, p := range players {
		ext := p.Extensions
		if ext == nil {
			ext = map[string]float64{}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO match_result_players (result_id, map_number, provider, subject, name, team, kills, deaths, extensions)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (result_id, map_number, provider, subject) DO NOTHING`,
			resultID, mapNumber, p.Provider, p.Subject, p.Name, p.Team, p.Kills, p.Deaths, ext); err != nil {
			return fmt.Errorf("store: match players: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: match players: %w", err)
	}
	return nil
}

// GetMatchResult is a series with its maps and players, or ErrNotFound.
func (s *Store) GetMatchResult(ctx context.Context, orgID, id uuid.UUID) (MatchResult, error) {
	r, err := scanMatchResult(s.pool.QueryRow(ctx, `SELECT `+matchResultColumns+` FROM match_results WHERE organization_id = $1 AND id = $2`, orgID, id))
	if errors.Is(err, ErrNotFound) {
		return r, err
	}
	if err != nil {
		return r, fmt.Errorf("store: get match result: %w", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT result_id, map_number, map_name, team1_score, team2_score, winner, started_at, ended_at, demo_key, demo_bytes
		FROM match_result_maps WHERE result_id = $1 ORDER BY map_number`, id)
	if err != nil {
		return r, fmt.Errorf("store: get match result: %w", err)
	}
	for rows.Next() {
		var m MatchResultMap
		if err := rows.Scan(&m.ResultID, &m.MapNumber, &m.MapName, &m.Team1Score, &m.Team2Score, &m.Winner, &m.StartedAt, &m.EndedAt, &m.DemoKey, &m.DemoBytes); err != nil {
			rows.Close()
			return r, fmt.Errorf("store: get match result: %w", err)
		}
		r.Maps = append(r.Maps, m)
	}
	rows.Close()
	prows, err := s.pool.Query(ctx, `SELECT result_id, map_number, provider, subject, name, team, kills, deaths, extensions
		FROM match_result_players WHERE result_id = $1 ORDER BY map_number, team, kills DESC, subject`, id)
	if err != nil {
		return r, fmt.Errorf("store: get match result: %w", err)
	}
	defer prows.Close()
	for prows.Next() {
		var p MatchResultPlayer
		if err := prows.Scan(&p.ResultID, &p.MapNumber, &p.Provider, &p.Subject, &p.Name, &p.Team, &p.Kills, &p.Deaths, &p.Extensions); err != nil {
			return r, fmt.Errorf("store: get match result: %w", err)
		}
		r.Players = append(r.Players, p)
	}
	return r, prows.Err()
}

// MatchResultFilter narrows ListMatchResults; the zero value is every series, newest first.
type MatchResultFilter struct {
	ServerID string
	LeaseID  *uuid.UUID
	// EndedOnly keeps series that have ended; EndedAfter, those that ended after it.
	EndedOnly  bool
	EndedAfter *time.Time
	Limit      int
}

// ListMatchResults is series without their maps and players, newest start first.
func (s *Store) ListMatchResults(ctx context.Context, orgID uuid.UUID, f MatchResultFilter) ([]MatchResult, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `SELECT `+matchResultColumns+` FROM match_results WHERE organization_id = $1
		AND ($2 = '' OR server_id = $2) AND ($3::uuid IS NULL OR lease_id = $3) AND (NOT $4 OR ended_at IS NOT NULL)
		AND ($5::timestamptz IS NULL OR ended_at > $5) ORDER BY started_at DESC, id LIMIT $6`,
		orgID, f.ServerID, f.LeaseID, f.EndedOnly || f.EndedAfter != nil, f.EndedAfter, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list match results: %w", err)
	}
	defer rows.Close()
	var out []MatchResult
	for rows.Next() {
		r, err := scanMatchResult(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list match results: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
