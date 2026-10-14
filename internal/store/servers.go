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
	// Feed is where the server pushes events and the token's file (ADR-0011), as JSON; nil for
	// none.
	Feed json.RawMessage
	// VM and Settings are a cloud-VM server's machine and game settings (ADR-0003), as JSON; nil
	// for any other driver.
	VM        json.RawMessage
	Settings  json.RawMessage
	CreatedAt time.Time
	UpdatedAt time.Time
	RemovedAt *time.Time
}

const (
	gameColumns     = `organization_id, id, bands, created_at, updated_at, removed_at`
	serverColumns   = `organization_id, id, game_id, name, driver, location, endpoint, credential_file, poll_interval_ms, trust, feed, vm, settings, created_at, updated_at, removed_at`
	locationColumns = `organization_id, id, provider, region, project, token_file, ssh_key_file, image, extra_ssh_keys, ssh_from, created_at, updated_at, removed_at`
)

// Location is a row of locations: a cloud account and region the hub makes VMs in (ADR-0003).
// TokenFile and SSHKeyFile are where the secrets are read from; the secrets are never stored.
type Location struct {
	OrganizationID uuid.UUID
	ID             string
	Provider       string
	Region         string
	Project        string
	TokenFile      string
	SSHKeyFile     string
	Image          string
	ExtraSSHKeys   []string
	SSHFrom        []string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	RemovedAt      *time.Time
}

func scanLocation(row pgx.Row) (Location, error) {
	var l Location
	var keys, from json.RawMessage
	err := row.Scan(&l.OrganizationID, &l.ID, &l.Provider, &l.Region, &l.Project, &l.TokenFile, &l.SSHKeyFile, &l.Image, &keys, &from, &l.CreatedAt, &l.UpdatedAt, &l.RemovedAt)
	if err != nil {
		return l, err
	}
	if err := json.Unmarshal(keys, &l.ExtraSSHKeys); err != nil {
		return l, err
	}
	if err := json.Unmarshal(from, &l.SSHFrom); err != nil {
		return l, err
	}
	if len(l.ExtraSSHKeys) == 0 {
		l.ExtraSSHKeys = nil
	}
	if len(l.SSHFrom) == 0 {
		l.SSHFrom = nil
	}
	return l, nil
}

// ListLocations returns an organization's live locations by id.
func (s *Store) ListLocations(ctx context.Context, orgID uuid.UUID) ([]Location, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+locationColumns+` FROM locations WHERE organization_id = $1 AND removed_at IS NULL ORDER BY id`, orgID)
	if err != nil {
		return nil, fmt.Errorf("store: list locations: %w", err)
	}
	defer rows.Close()
	var out []Location
	for rows.Next() {
		l, err := scanLocation(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list locations: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list locations: %w", err)
	}
	return out, nil
}

func scanGame(row pgx.Row) (Game, error) {
	var g Game
	err := row.Scan(&g.OrganizationID, &g.ID, &g.Bands, &g.CreatedAt, &g.UpdatedAt, &g.RemovedAt)
	return g, err
}

func scanServer(row pgx.Row) (ManagedServer, error) {
	var s ManagedServer
	var pollMS int64
	err := row.Scan(&s.OrganizationID, &s.ID, &s.GameID, &s.Name, &s.Driver, &s.Location, &s.Endpoint, &s.CredentialFile, &pollMS, &s.Trust, &s.Feed, &s.VM, &s.Settings, &s.CreatedAt, &s.UpdatedAt, &s.RemovedAt)
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

// ApplyServers makes an organization's games, locations and servers the given ones, in one
// transaction: each is inserted, updated where it differs, or revived if it was removed; every
// other live one is marked removed. A row that is already as given keeps its updated_at.
func (s *Store) ApplyServers(ctx context.Context, orgID uuid.UUID, games []Game, locations []Location, servers []ManagedServer, now time.Time) error {
	return s.ApplyServersRecorded(ctx, orgID, games, locations, servers, now, Applied{})
}

// Applied is who applied a servers manifest and what it changed, for the events log
// (servers.applied, #195). With no changes nothing is recorded.
type Applied struct {
	By Actor
	// Changes are the changes in words, as servers.Change prints them; they name ids, never a
	// secret.
	Changes []string
	// Servers are the ids of the servers the changes touch.
	Servers []string
}

// ApplyServersRecorded is ApplyServers that also records rec in the events log, in the same
// transaction.
func (s *Store) ApplyServersRecorded(ctx context.Context, orgID uuid.UUID, games []Game, locations []Location, servers []ManagedServer, now time.Time, rec Applied) error {
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
	locationIDs := make([]string, 0, len(locations))
	for _, l := range locations {
		keys, err := json.Marshal(nonNilStrings(l.ExtraSSHKeys))
		if err != nil {
			return fmt.Errorf("store: apply servers: location %s: %w", l.ID, err)
		}
		from, err := json.Marshal(nonNilStrings(l.SSHFrom))
		if err != nil {
			return fmt.Errorf("store: apply servers: location %s: %w", l.ID, err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO locations (organization_id, id, provider, region, project, token_file, ssh_key_file, image, extra_ssh_keys, ssh_from, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $11)
			ON CONFLICT (organization_id, id) DO UPDATE SET
			  provider = excluded.provider, region = excluded.region, project = excluded.project, token_file = excluded.token_file,
			  ssh_key_file = excluded.ssh_key_file, image = excluded.image, extra_ssh_keys = excluded.extra_ssh_keys,
			  ssh_from = excluded.ssh_from, updated_at = $11, removed_at = NULL
			 WHERE (locations.provider, locations.region, locations.project, locations.token_file, locations.ssh_key_file,
			        locations.image, locations.extra_ssh_keys, locations.ssh_from)
			       IS DISTINCT FROM
			       (excluded.provider, excluded.region, excluded.project, excluded.token_file, excluded.ssh_key_file,
			        excluded.image, excluded.extra_ssh_keys, excluded.ssh_from)
			    OR locations.removed_at IS NOT NULL`,
			orgID, l.ID, l.Provider, l.Region, l.Project, l.TokenFile, l.SSHKeyFile, l.Image, keys, from, now); err != nil {
			return fmt.Errorf("store: apply servers: location %s: %w", l.ID, err)
		}
		locationIDs = append(locationIDs, l.ID)
	}
	serverIDs := make([]string, 0, len(servers))
	for _, v := range servers {
		if _, err := tx.Exec(ctx, `
			INSERT INTO managed_servers (organization_id, id, game_id, name, driver, location, endpoint, credential_file, poll_interval_ms, trust, feed, vm, settings, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $14)
			ON CONFLICT (organization_id, id) DO UPDATE SET
			  game_id = excluded.game_id, name = excluded.name, driver = excluded.driver, location = excluded.location,
			  endpoint = excluded.endpoint, credential_file = excluded.credential_file, poll_interval_ms = excluded.poll_interval_ms,
			  trust = excluded.trust, feed = excluded.feed, vm = excluded.vm, settings = excluded.settings, updated_at = $14, removed_at = NULL
			 WHERE (managed_servers.game_id, managed_servers.name, managed_servers.driver, managed_servers.location, managed_servers.endpoint,
			        managed_servers.credential_file, managed_servers.poll_interval_ms, managed_servers.trust, managed_servers.feed,
			        managed_servers.vm, managed_servers.settings)
			       IS DISTINCT FROM
			       (excluded.game_id, excluded.name, excluded.driver, excluded.location, excluded.endpoint,
			        excluded.credential_file, excluded.poll_interval_ms, excluded.trust, excluded.feed, excluded.vm, excluded.settings)
			    OR managed_servers.removed_at IS NOT NULL`,
			orgID, v.ID, v.GameID, v.Name, v.Driver, v.Location, v.Endpoint, v.CredentialFile, v.PollInterval.Milliseconds(), v.Trust,
			nullJSON(v.Feed), nullJSON(v.VM), nullJSON(v.Settings), now); err != nil {
			return fmt.Errorf("store: apply servers: server %s: %w", v.ID, err)
		}
		serverIDs = append(serverIDs, v.ID)
	}
	if _, err := tx.Exec(ctx, `UPDATE managed_servers SET removed_at = $3, updated_at = $3 WHERE organization_id = $1 AND removed_at IS NULL AND NOT (id = ANY($2))`, orgID, serverIDs, now); err != nil {
		return fmt.Errorf("store: apply servers: remove servers: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE locations SET removed_at = $3, updated_at = $3 WHERE organization_id = $1 AND removed_at IS NULL AND NOT (id = ANY($2))`, orgID, locationIDs, now); err != nil {
		return fmt.Errorf("store: apply servers: remove locations: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE games SET removed_at = $3, updated_at = $3 WHERE organization_id = $1 AND removed_at IS NULL AND NOT (id = ANY($2))`, orgID, gameIDs, now); err != nil {
		return fmt.Errorf("store: apply servers: remove games: %w", err)
	}
	if len(rec.Changes) > 0 {
		if err := s.appendEvent(ctx, tx, rec.By.event(Event{OrganizationID: orgID, At: now, Kind: "servers.applied",
			Summary: fmt.Sprintf("The servers manifest was applied: %d changes", len(rec.Changes)),
			Detail:  map[string]any{"changes": rec.Changes, "servers": nonNilStrings(rec.Servers)},
			Source:  "servers.apply", SourceID: fmt.Sprintf("%s:%d", orgID, now.UnixMicro())})); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: apply servers: %w", err)
	}
	return nil
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// nullJSON is SQL NULL for an empty document.
func nullJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
