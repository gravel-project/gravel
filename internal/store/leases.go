package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ServerLease is a row of server_leases (gravel#20).
type ServerLease struct {
	ID              uuid.UUID
	OrganizationID  uuid.UUID
	ServerID        string
	Pool            string
	HolderKind      string // LeaseHolderApp or LeaseHolderUser
	Holder          string
	HolderRef       string
	State           string
	Reason          string
	Settings        map[string]string
	Plugins         []string
	MatchConfig     []byte
	ExpectedPlayers int
	PluginState     string
	PluginReason    string
	MatchLoaded     bool
	AuditID         *int64
	CreatedAt       time.Time
	ReadyAt         *time.Time
	EndedAt         *time.Time
	ReleasedAt      *time.Time
	MaxUntil        time.Time
}

// Lease holders and states.
const (
	LeaseHolderApp  = "app"
	LeaseHolderUser = "user"

	LeaseStarting = "starting"
	LeaseReady    = "ready"
	LeaseEnding   = "ending"
	LeaseReleased = "released"
	LeaseFailed   = "failed"
)

// Live reports whether a lease still holds its server.
func (l ServerLease) Live() bool {
	return l.State == LeaseStarting || l.State == LeaseReady || l.State == LeaseEnding
}

// Errors of server_leases.
var (
	// ErrLeaseServerTaken is a server with a live lease already.
	ErrLeaseServerTaken = errors.New("store: the server has a live lease")
	// ErrLeaseRefTaken is a holder's reference with a live lease already.
	ErrLeaseRefTaken = errors.New("store: the holder has a live lease for that reference")
)

const leaseColumns = `id, organization_id, server_id, pool, holder_kind, holder, holder_ref, state, reason, settings, plugins, match_config,
	expected_players, plugin_state, plugin_reason, match_loaded, audit_id, created_at, ready_at, ended_at, released_at, max_until`

func scanLease(row pgx.Row) (ServerLease, error) {
	var l ServerLease
	var settings, plugins []byte
	err := row.Scan(&l.ID, &l.OrganizationID, &l.ServerID, &l.Pool, &l.HolderKind, &l.Holder, &l.HolderRef, &l.State, &l.Reason, &settings,
		&plugins, &l.MatchConfig, &l.ExpectedPlayers, &l.PluginState, &l.PluginReason, &l.MatchLoaded, &l.AuditID, &l.CreatedAt, &l.ReadyAt,
		&l.EndedAt, &l.ReleasedAt, &l.MaxUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return l, ErrNotFound
	}
	if err != nil {
		return l, err
	}
	if err := json.Unmarshal(settings, &l.Settings); err != nil {
		return l, err
	}
	if err := json.Unmarshal(plugins, &l.Plugins); err != nil {
		return l, err
	}
	return l, nil
}

// CreateServerLease stores a new lease. ErrLeaseServerTaken when its server has a live lease,
// ErrLeaseRefTaken when its holder has one for the same reference.
func (s *Store) CreateServerLease(ctx context.Context, l ServerLease) error {
	settings, err := json.Marshal(nonNilMap(l.Settings))
	if err != nil {
		return err
	}
	plugins, err := json.Marshal(nonNilStrings(l.Plugins))
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: create lease: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `INSERT INTO server_leases (`+leaseColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)`,
		l.ID, l.OrganizationID, l.ServerID, l.Pool, l.HolderKind, l.Holder, l.HolderRef, l.State, l.Reason, settings, plugins, l.MatchConfig,
		l.ExpectedPlayers, l.PluginState, l.PluginReason, l.MatchLoaded, l.AuditID, l.CreatedAt, l.ReadyAt, l.EndedAt, l.ReleasedAt, l.MaxUntil)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		if pgErr.ConstraintName == "server_leases_live_ref" {
			return ErrLeaseRefTaken
		}
		return ErrLeaseServerTaken
	}
	if err != nil {
		return fmt.Errorf("store: create lease: %w", err)
	}
	if err := s.appendEvent(ctx, tx, leaseEvent(l, l.CreatedAt)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: create lease: %w", err)
	}
	return nil
}

// leaseEvent is the event of a lease reaching its state.
func leaseEvent(l ServerLease, at time.Time) Event {
	return Event{OrganizationID: l.OrganizationID, At: at, Kind: "lease." + l.State, ServerID: l.ServerID,
		Summary: "lease of " + l.ServerID + ": " + l.State, Detail: map[string]any{"lease_id": l.ID.String(), "pool": l.Pool, "holder_kind": l.HolderKind, "reason": l.Reason},
		Source: "server_leases", SourceID: l.ID.String() + ":" + l.State}
}

// UpdateServerLease writes a lease's state, reasons and times (what the hub's lease job changes).
func (s *Store) UpdateServerLease(ctx context.Context, l ServerLease) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: update lease: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var before string
	if err := tx.QueryRow(ctx, `SELECT state FROM server_leases WHERE organization_id = $1 AND id = $2 FOR UPDATE`, l.OrganizationID, l.ID).Scan(&before); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("store: update lease: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE server_leases SET state = $3, reason = $4, plugin_state = $5, plugin_reason = $6, match_loaded = $7,
		ready_at = $8, ended_at = $9, released_at = $10 WHERE organization_id = $1 AND id = $2`,
		l.OrganizationID, l.ID, l.State, l.Reason, l.PluginState, l.PluginReason, l.MatchLoaded, l.ReadyAt, l.EndedAt, l.ReleasedAt); err != nil {
		return fmt.Errorf("store: update lease: %w", err)
	}
	if l.State != before {
		at := time.Now().UTC()
		for _, t := range []*time.Time{l.ReleasedAt, l.EndedAt, l.ReadyAt} {
			if t != nil && l.State != LeaseStarting {
				at = *t
				break
			}
		}
		if err := s.appendEvent(ctx, tx, leaseEvent(l, at)); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: update lease: %w", err)
	}
	return nil
}

// GetServerLease is a lease by id, or ErrNotFound.
func (s *Store) GetServerLease(ctx context.Context, orgID, id uuid.UUID) (ServerLease, error) {
	l, err := scanLease(s.pool.QueryRow(ctx, `SELECT `+leaseColumns+` FROM server_leases WHERE organization_id = $1 AND id = $2`, orgID, id))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return l, fmt.Errorf("store: get lease: %w", err)
	}
	return l, err
}

// LiveLeaseForRef is the holder's live lease for a reference, or ErrNotFound.
func (s *Store) LiveLeaseForRef(ctx context.Context, orgID uuid.UUID, holderKind, holder, ref string) (ServerLease, error) {
	l, err := scanLease(s.pool.QueryRow(ctx, `SELECT `+leaseColumns+` FROM server_leases WHERE organization_id = $1 AND holder_kind = $2
		AND holder = $3 AND holder_ref = $4 AND state IN ('starting', 'ready', 'ending')`, orgID, holderKind, holder, ref))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return l, fmt.Errorf("store: lease for a reference: %w", err)
	}
	return l, err
}

// ListServerLeases is an organization's leases, newest first: every live one, or a server's
// ("" for all), at most limit.
func (s *Store) ListServerLeases(ctx context.Context, orgID uuid.UUID, serverID string, liveOnly bool, limit int) ([]ServerLease, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+leaseColumns+` FROM server_leases WHERE organization_id = $1
		AND ($2 = '' OR server_id = $2) AND (NOT $3 OR state IN ('starting', 'ready', 'ending'))
		ORDER BY created_at DESC, id LIMIT $4`, orgID, serverID, liveOnly, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list leases: %w", err)
	}
	defer rows.Close()
	var out []ServerLease
	for rows.Next() {
		l, err := scanLease(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list leases: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list leases: %w", err)
	}
	return out, nil
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
