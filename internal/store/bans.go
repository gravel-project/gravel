package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrBanExists is a ban of an identity that already has an active ban on the server.
var ErrBanExists = errors.New("store: the identity already has an active ban on the server")

// Ban is a row of bans: one ban of a provider identity on a server, active until LiftedAt.
type Ban struct {
	ID             int64
	OrganizationID uuid.UUID
	ServerID       string
	Provider       string
	Subject        string
	Reason         string
	BannedAt       time.Time
	// BanAuditID is nil for a ban imported when the hub adopted the server's list.
	BanAuditID  *int64
	LiftedAt    *time.Time
	LiftAuditID *int64
}

const banColumns = `id, organization_id, server_id, provider, subject, reason, banned_at, ban_audit_id, lifted_at, lift_audit_id`

func scanBan(row pgx.Row) (Ban, error) {
	var b Ban
	err := row.Scan(&b.ID, &b.OrganizationID, &b.ServerID, &b.Provider, &b.Subject, &b.Reason, &b.BannedAt, &b.BanAuditID, &b.LiftedAt, &b.LiftAuditID)
	return b, err
}

// BanListAdopted reports whether the hub has adopted a server's ban list.
func (s *Store) BanListAdopted(ctx context.Context, orgID uuid.UUID, serverID string) (bool, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM ban_lists WHERE organization_id = $1 AND server_id = $2`, orgID, serverID).Scan(&n); err != nil {
		return false, fmt.Errorf("store: ban list adopted: %w", err)
	}
	return n > 0, nil
}

// AdoptBanList records the adoption of a server's ban list and imports its entries (provider,
// subject) as bans with no audit entry, in one transaction. A list already adopted is left alone
// and reports false.
func (s *Store) AdoptBanList(ctx context.Context, orgID uuid.UUID, serverID string, imported []Ban, at time.Time) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("store: adopt ban list: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `INSERT INTO ban_lists (organization_id, server_id, adopted_at) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, orgID, serverID, at)
	if err != nil {
		return false, fmt.Errorf("store: adopt ban list: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	for _, b := range imported {
		if _, err := tx.Exec(ctx, `
			INSERT INTO bans (organization_id, server_id, provider, subject, reason, banned_at) VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (organization_id, server_id, provider, subject) WHERE lifted_at IS NULL DO NOTHING`,
			orgID, serverID, b.Provider, b.Subject, b.Reason, at); err != nil {
			return false, fmt.Errorf("store: adopt ban list: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: adopt ban list: %w", err)
	}
	return true, nil
}

// ActiveBans are a server's active bans, oldest first.
func (s *Store) ActiveBans(ctx context.Context, orgID uuid.UUID, serverID string) ([]Ban, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+banColumns+` FROM bans WHERE organization_id = $1 AND server_id = $2 AND lifted_at IS NULL ORDER BY id`, orgID, serverID)
	if err != nil {
		return nil, fmt.Errorf("store: active bans: %w", err)
	}
	defer rows.Close()
	var out []Ban
	for rows.Next() {
		b, err := scanBan(rows)
		if err != nil {
			return nil, fmt.Errorf("store: active bans: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: active bans: %w", err)
	}
	return out, nil
}

// AddBan records an active ban and returns its id; ErrBanExists when the identity has one.
func (s *Store) AddBan(ctx context.Context, b Ban) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO bans (organization_id, server_id, provider, subject, reason, banned_at, ban_audit_id) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		b.OrganizationID, b.ServerID, b.Provider, b.Subject, b.Reason, b.BannedAt, b.BanAuditID).Scan(&id)
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return 0, ErrBanExists
	}
	if err != nil {
		return 0, fmt.Errorf("store: add ban: %w", err)
	}
	return id, nil
}

// LiftBan marks an identity's active ban on a server lifted; ErrNotFound when it has none.
// liftAuditID may be nil (a ban rolled back because the server refused it).
func (s *Store) LiftBan(ctx context.Context, orgID uuid.UUID, serverID, provider, subject string, liftAuditID *int64, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `UPDATE bans SET lifted_at = $5, lift_audit_id = $6
		WHERE organization_id = $1 AND server_id = $2 AND provider = $3 AND subject = $4 AND lifted_at IS NULL`,
		orgID, serverID, provider, subject, at, liftAuditID)
	if err != nil {
		return fmt.Errorf("store: lift ban: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
