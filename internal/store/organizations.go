package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	// Settings is the Organization settings document (org.Settings), "{}" until one is written.
	Settings          json.RawMessage
	SettingsUpdatedAt *time.Time
	// Pending is the member the owner nominated to take the hub over (#197), nil when none.
	Pending *PendingOwner
}

// PendingOwner is a nomination: who, their name, since when and until when they may accept.
type PendingOwner struct {
	UserID      uuid.UUID
	Name        string
	NominatedAt time.Time
	ExpiresAt   time.Time
}

// Owned reports whether ownership has been claimed.
func (o Organization) Owned() bool { return o.ClaimedAt != nil }

const organizationColumns = `id, name, builtin, created_at, claimed_at, owner_user_id, claim_token_hash, claim_token_expires_at, settings, settings_updated_at,
	pending_owner_user_id, coalesce((SELECT display_name FROM users WHERE users.id = organizations.pending_owner_user_id), ''),
	pending_owner_nominated_at, pending_owner_expires_at`

func scanOrganization(row pgx.Row) (Organization, error) {
	var o Organization
	var pending *uuid.UUID
	var pendingName string
	var nominated, expires *time.Time
	err := row.Scan(&o.ID, &o.Name, &o.Builtin, &o.CreatedAt, &o.ClaimedAt, &o.OwnerUserID, &o.ClaimTokenHash, &o.ClaimTokenExpiresAt, &o.Settings, &o.SettingsUpdatedAt,
		&pending, &pendingName, &nominated, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return Organization{}, ErrNotFound
	}
	if err == nil && pending != nil && nominated != nil && expires != nil {
		o.Pending = &PendingOwner{UserID: *pending, Name: pendingName, NominatedAt: *nominated, ExpiresAt: *expires}
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

// MintClaimToken replaces an unowned organization's claim token as SetClaimToken does, and
// records in the same transaction that the host's command line minted one
// (ownership.claim_token_minted, by by, ADR-0036). The event carries the expiry, nothing of the
// token. ErrOwned once the organization is owned.
func (s *Store) MintClaimToken(ctx context.Context, id uuid.UUID, hash []byte, expiresAt time.Time, by Actor, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: mint claim token: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx,
		`UPDATE organizations SET claim_token_hash = $2, claim_token_expires_at = $3 WHERE id = $1 AND claimed_at IS NULL`,
		id, hash, expiresAt)
	if err != nil {
		return fmt.Errorf("store: mint claim token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrOwned
	}
	if err := s.appendEvent(ctx, tx, by.event(Event{OrganizationID: id, At: now, Kind: "ownership.claim_token_minted",
		Summary: "the host minted an owner-claim token, valid until " + expiresAt.UTC().Format("2006-01-02 15:04 UTC"),
		Source:  "organizations.claim_token", // no source id: every mint is its own event
		Detail:  map[string]any{"expires_at": expiresAt.UTC().Format(time.RFC3339)}})); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: mint claim token: %w", err)
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
//
// The claim is an event (ownership.claimed) in the same transaction, by the new owner; it carries
// nothing of the token.
func (s *Store) ClaimOrganization(ctx context.Context, id uuid.UUID, hash []byte, by Actor, now time.Time) (Organization, error) {
	if by.UserID == nil {
		return Organization{}, errors.New("store: claim organization: no owner")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Organization{}, fmt.Errorf("store: claim organization: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	o, err := scanOrganization(tx.QueryRow(ctx,
		`UPDATE organizations
		    SET claimed_at = $3, owner_user_id = $4, claim_token_hash = NULL, claim_token_expires_at = NULL
		  WHERE id = $1 AND claimed_at IS NULL AND claim_token_hash = $2 AND claim_token_expires_at > $3
		 RETURNING `+organizationColumns, id, hash, now, *by.UserID))
	if err == nil {
		if err := s.appendEvent(ctx, tx, by.event(Event{OrganizationID: id, At: now, Kind: "ownership.claimed", SubjectUserID: by.UserID,
			Summary: "{user} claimed the hub and became its owner", Source: "organizations.claim", SourceID: id.String()})); err != nil {
			return Organization{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Organization{}, fmt.Errorf("store: claim organization: %w", err)
		}
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

// ErrSettingsConflict is a settings write whose caller read an older document (gravel#205).
var ErrSettingsConflict = errors.New("the settings changed since they were read")

// UpdateOrganizationSettings replaces the settings document and records it (settings.updated, by
// by, naming the top-level sections that changed) in one transaction. ErrNotFound for an unknown
// id.
func (s *Store) UpdateOrganizationSettings(ctx context.Context, id uuid.UUID, settings json.RawMessage, at time.Time, by Actor, sections []string) (Organization, error) {
	return s.UpdateOrganizationSettingsIf(ctx, id, settings, at, by, sections, nil)
}

// UpdateOrganizationSettingsIf is UpdateOrganizationSettings when the stored document was last
// written at ifUpdatedAt (nil: whenever, or never), checked in the same statement that writes, so
// two editors can't overwrite each other: ErrSettingsConflict otherwise.
func (s *Store) UpdateOrganizationSettingsIf(ctx context.Context, id uuid.UUID, settings json.RawMessage, at time.Time, by Actor, sections []string, ifUpdatedAt *time.Time) (Organization, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Organization{}, fmt.Errorf("store: update organization settings: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	check := ifUpdatedAt != nil
	var want *time.Time
	if check {
		w := ifUpdatedAt.UTC()
		want = &w
	}
	o, err := scanOrganization(tx.QueryRow(ctx,
		`UPDATE organizations SET settings = $2, settings_updated_at = $3
		  WHERE id = $1 AND (NOT $4 OR settings_updated_at IS NOT DISTINCT FROM $5::timestamptz) RETURNING `+organizationColumns,
		id, settings, at, check, want))
	if errors.Is(err, ErrNotFound) {
		if check {
			var exists bool
			if qerr := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM organizations WHERE id = $1)`, id).Scan(&exists); qerr == nil && exists {
				return Organization{}, ErrSettingsConflict
			}
		}
		return Organization{}, err
	}
	if err != nil {
		return Organization{}, fmt.Errorf("store: update organization settings: %w", err)
	}
	if sections == nil {
		sections = []string{}
	}
	summary := "The settings were saved unchanged"
	if len(sections) > 0 {
		summary = "The settings changed: " + strings.Join(sections, ", ")
	}
	if err := s.appendEvent(ctx, tx, by.event(Event{OrganizationID: id, At: at, Kind: "settings.updated", Summary: summary,
		Detail: map[string]any{"sections": sections, "settings_updated_at": at.UTC().Format(time.RFC3339Nano)}, Source: "organizations.settings"})); err != nil {
		return Organization{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Organization{}, fmt.Errorf("store: update organization settings: %w", err)
	}
	return o, nil
}
