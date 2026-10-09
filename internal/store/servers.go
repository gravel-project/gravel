package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Game is a row of games: a shipped Game spec an organization enabled, with the manifest's
// tightenings of its bands as JSON (ADR-0010).
type Game struct {
	OrganizationID uuid.UUID
	ID             string
	Bands          json.RawMessage
	CreatedAt      time.Time
	UpdatedAt      time.Time
	RemovedAt      *time.Time
}

// ManagedServer is a row of managed_servers: a server a driver controls. CredentialFile is where
// the credential is read from; the credential itself is never stored.
type ManagedServer struct {
	OrganizationID uuid.UUID
	ID             string
	GameID         string
	Name           string
	Driver         string
	Location       string
	Endpoint       string
	CredentialFile string
	PollInterval   time.Duration
	Trust          string
	// Seeding is the seeding section as JSON, nil when the server has none.
	Seeding   json.RawMessage
	CreatedAt time.Time
	UpdatedAt time.Time
	RemovedAt *time.Time
}

const (
	gameColumns   = `organization_id, id, bands, created_at, updated_at, removed_at`
	serverColumns = `organization_id, id, game_id, name, driver, location, endpoint, credential_file, poll_interval_ms, trust, seeding, created_at, updated_at, removed_at`
)

func scanGame(row pgx.Row) (Game, error) {
	var g Game
	err := row.Scan(&g.OrganizationID, &g.ID, &g.Bands, &g.CreatedAt, &g.UpdatedAt, &g.RemovedAt)
	return g, err
}

func scanServer(row pgx.Row) (ManagedServer, error) {
	var s ManagedServer
	var pollMS int64
	err := row.Scan(&s.OrganizationID, &s.ID, &s.GameID, &s.Name, &s.Driver, &s.Location, &s.Endpoint, &s.CredentialFile, &pollMS, &s.Trust, &s.Seeding, &s.CreatedAt, &s.UpdatedAt, &s.RemovedAt)
	s.PollInterval = time.Duration(pollMS) * time.Millisecond
	return s, err
}

// ListGames returns an organization's games by id; removed ones only when asked.
func (s *Store) ListGames(ctx context.Context, orgID uuid.UUID, includeRemoved bool) ([]Game, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+gameColumns+` FROM games WHERE organization_id = $1 AND ($2 OR removed_at IS NULL) ORDER BY id`, orgID, includeRemoved)
	if err != nil {
		return nil, fmt.Errorf("store: list games: %w", err)
	}
	defer rows.Close()
	var out []Game
	for rows.Next() {
		g, err := scanGame(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list games: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list games: %w", err)
	}
	return out, nil
}

// ListManagedServers returns an organization's servers by id; removed ones only when asked.
func (s *Store) ListManagedServers(ctx context.Context, orgID uuid.UUID, includeRemoved bool) ([]ManagedServer, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+serverColumns+` FROM managed_servers WHERE organization_id = $1 AND ($2 OR removed_at IS NULL) ORDER BY id`, orgID, includeRemoved)
	if err != nil {
		return nil, fmt.Errorf("store: list servers: %w", err)
	}
	defer rows.Close()
	var out []ManagedServer
	for rows.Next() {
		srv, err := scanServer(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list servers: %w", err)
		}
		out = append(out, srv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list servers: %w", err)
	}
	return out, nil
}

// ApplyServers makes an organization's games and servers the given ones, in one transaction:
// each is inserted, updated where it differs, or revived if it was removed; every other live one
// is marked removed. A row that is already as given keeps its updated_at.
func (s *Store) ApplyServers(ctx context.Context, orgID uuid.UUID, games []Game, servers []ManagedServer, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: apply servers: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	gameIDs := make([]string, 0, len(games))
	for _, g := range games {
		bands := g.Bands
		if len(bands) == 0 {
			bands = json.RawMessage(`[]`)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO games (organization_id, id, bands, created_at, updated_at) VALUES ($1, $2, $3, $4, $4)
			ON CONFLICT (organization_id, id) DO UPDATE SET bands = excluded.bands, updated_at = $4, removed_at = NULL
			 WHERE games.bands IS DISTINCT FROM excluded.bands OR games.removed_at IS NOT NULL`,
			orgID, g.ID, bands, now); err != nil {
			return fmt.Errorf("store: apply servers: game %s: %w", g.ID, err)
		}
		gameIDs = append(gameIDs, g.ID)
	}
	serverIDs := make([]string, 0, len(servers))
	for _, v := range servers {
		if _, err := tx.Exec(ctx, `
			INSERT INTO managed_servers (organization_id, id, game_id, name, driver, location, endpoint, credential_file, poll_interval_ms, trust, seeding, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12)
			ON CONFLICT (organization_id, id) DO UPDATE SET
			  game_id = excluded.game_id, name = excluded.name, driver = excluded.driver, location = excluded.location,
			  endpoint = excluded.endpoint, credential_file = excluded.credential_file, poll_interval_ms = excluded.poll_interval_ms,
			  trust = excluded.trust, seeding = excluded.seeding, updated_at = $12, removed_at = NULL
			 WHERE (managed_servers.game_id, managed_servers.name, managed_servers.driver, managed_servers.location, managed_servers.endpoint,
			        managed_servers.credential_file, managed_servers.poll_interval_ms, managed_servers.trust, managed_servers.seeding)
			       IS DISTINCT FROM
			       (excluded.game_id, excluded.name, excluded.driver, excluded.location, excluded.endpoint,
			        excluded.credential_file, excluded.poll_interval_ms, excluded.trust, excluded.seeding)
			    OR managed_servers.removed_at IS NOT NULL`,
			orgID, v.ID, v.GameID, v.Name, v.Driver, v.Location, v.Endpoint, v.CredentialFile, v.PollInterval.Milliseconds(), v.Trust, nullJSON(v.Seeding), now); err != nil {
			return fmt.Errorf("store: apply servers: server %s: %w", v.ID, err)
		}
		serverIDs = append(serverIDs, v.ID)
	}
	if _, err := tx.Exec(ctx, `UPDATE managed_servers SET removed_at = $3, updated_at = $3 WHERE organization_id = $1 AND removed_at IS NULL AND NOT (id = ANY($2))`, orgID, serverIDs, now); err != nil {
		return fmt.Errorf("store: apply servers: remove servers: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE games SET removed_at = $3, updated_at = $3 WHERE organization_id = $1 AND removed_at IS NULL AND NOT (id = ANY($2))`, orgID, gameIDs, now); err != nil {
		return fmt.Errorf("store: apply servers: remove games: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: apply servers: %w", err)
	}
	return nil
}

// nullJSON is SQL NULL for an empty document.
func nullJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
