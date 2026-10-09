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

// Organization is a row of organizations.
type Organization struct {
	ID        uuid.UUID
	Name      string
	Builtin   bool
	CreatedAt time.Time
	ClaimedAt *time.Time
	// OwnerUserID is the user who claimed ownership; set together with ClaimedAt.
	OwnerUserID         *uuid.UUID
	ClaimTokenHash      []byte
	ClaimTokenExpiresAt *time.Time
}

// Owned reports whether ownership has been claimed.
func (o Organization) Owned() bool { return o.ClaimedAt != nil }

const organizationColumns = `id, name, builtin, created_at, claimed_at, owner_user_id, claim_token_hash, claim_token_expires_at`

func scanOrganization(row pgx.Row) (Organization, error) {
	var o Organization
	err := row.Scan(&o.ID, &o.Name, &o.Builtin, &o.CreatedAt, &o.ClaimedAt, &o.OwnerUserID, &o.ClaimTokenHash, &o.ClaimTokenExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Organization{}, ErrNotFound
	}
	return o, err
}

// GetBuiltinOrganization returns the built-in organization or ErrNotFound.
func (s *Store) GetBuiltinOrganization(ctx context.Context) (Organization, error) {
	o, err := scanOrganization(s.pool.QueryRow(ctx, `SELECT `+organizationColumns+` FROM organizations WHERE builtin`))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Organization{}, fmt.Errorf("store: get builtin organization: %w", err)
	}
	return o, err
}

// CreateBuiltinOrganization inserts the built-in organization. If one already exists (a
// concurrent start won the race) the existing row is returned.
func (s *Store) CreateBuiltinOrganization(ctx context.Context, id uuid.UUID, name string) (Organization, error) {
	o, err := scanOrganization(s.pool.QueryRow(ctx,
		`INSERT INTO organizations (id, name, builtin) VALUES ($1, $2, true) RETURNING `+organizationColumns, id, name))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation: organizations_one_builtin
		return s.GetBuiltinOrganization(ctx)
	}
	if err != nil {
		return Organization{}, fmt.Errorf("store: create builtin organization: %w", err)
	}
	return o, nil
}

// UpdateOrganizationName renames an organization.
func (s *Store) UpdateOrganizationName(ctx context.Context, id uuid.UUID, name string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE organizations SET name = $2 WHERE id = $1`, id, name)
	if err != nil {
		return fmt.Errorf("store: rename organization: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetClaimToken stores a fresh owner-claim token hash on an unowned organization, replacing any
// earlier one. ErrOwned if the organization is already owned.
func (s *Store) SetClaimToken(ctx context.Context, id uuid.UUID, hash []byte, expiresAt time.Time) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE organizations SET claim_token_hash = $2, claim_token_expires_at = $3 WHERE id = $1 AND claimed_at IS NULL`,
		id, hash, expiresAt)
	if err != nil {
		return fmt.Errorf("store: set claim token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrOwned
	}
	return nil
}

// ErrOwned is returned when a claim-related write hits an organization that is already owned.
var ErrOwned = errors.New("organization is already owned")

// ErrClaimRejected is returned when the token hash does not match or has expired.
var ErrClaimRejected = errors.New("claim rejected")

// ClaimOrganization marks the organization owned by ownerUserID if, and only if, it is unowned
// and hash matches the unexpired stored token. The token is cleared in the same statement, so
// it can't be used twice. The caller compares hashes in constant time before calling; the
// predicate here is what makes the claim atomic under concurrent attempts.
func (s *Store) ClaimOrganization(ctx context.Context, id uuid.UUID, hash []byte, ownerUserID uuid.UUID, now time.Time) (Organization, error) {
	o, err := scanOrganization(s.pool.QueryRow(ctx,
		`UPDATE organizations
		    SET claimed_at = $3, owner_user_id = $4, claim_token_hash = NULL, claim_token_expires_at = NULL
		  WHERE id = $1 AND claimed_at IS NULL AND claim_token_hash = $2 AND claim_token_expires_at > $3
		 RETURNING `+organizationColumns, id, hash, now, ownerUserID))
	if err == nil {
		return o, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Organization{}, fmt.Errorf("store: claim organization: %w", err)
	}
	current, err := scanOrganization(s.pool.QueryRow(ctx, `SELECT `+organizationColumns+` FROM organizations WHERE id = $1`, id))
	if err != nil {
		return Organization{}, fmt.Errorf("store: claim organization: %w", err)
	}
	if current.Owned() {
		return Organization{}, ErrOwned
	}
	return Organization{}, ErrClaimRejected
}
