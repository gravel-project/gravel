package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Password link purposes and the identity events they write (gravel#187).
const (
	PasswordLinkSet   = "set"   // a new account's first password
	PasswordLinkReset = "reset" // a forgotten one

	IdentityEventPasswordLinkIssued = "password_reset_issued"
	IdentityEventPasswordSet        = "password_set"
	IdentityEventPasswordReset      = "password_reset"
)

// PasswordLink is a one-time link that sets a local account's password. TokenHash is the SHA-256
// of the token in the link; the token itself is never stored.
type PasswordLink struct {
	ID        uuid.UUID
	TokenHash []byte
	Subject   string
	Purpose   string
	IssuedBy  *uuid.UUID // nil: issued from the host
	IssuedAt  time.Time
	ExpiresAt time.Time
	UsedAt    *time.Time
}

const passwordLinkColumns = `id, token_hash, subject, purpose, issued_by, issued_at, expires_at, used_at`

func scanPasswordLink(row pgx.Row) (PasswordLink, error) {
	var l PasswordLink
	err := row.Scan(&l.ID, &l.TokenHash, &l.Subject, &l.Purpose, &l.IssuedBy, &l.IssuedAt, &l.ExpiresAt, &l.UsedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PasswordLink{}, ErrNotFound
	}
	return l, err
}

// CreatePasswordLink stores a link for a local account, deletes the account's unused links, and logs
// the issue on the account's identity log, in one transaction. ErrNotFound for an unknown account.
func (s *Store) CreatePasswordLink(ctx context.Context, l PasswordLink) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: create password link: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var userID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT user_id FROM identities WHERE provider = $1 AND subject = $2 FOR UPDATE`, ProviderLocal, l.Subject).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: create password link: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM password_links WHERE provider = $1 AND subject = $2 AND used_at IS NULL`, ProviderLocal, l.Subject); err != nil {
		return fmt.Errorf("store: create password link: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO password_links (id, token_hash, provider, subject, purpose, issued_by, issued_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, l.ID, l.TokenHash, ProviderLocal, l.Subject, l.Purpose, l.IssuedBy, l.IssuedAt, l.ExpiresAt); err != nil {
		return fmt.Errorf("store: create password link: %w", err)
	}
	if err := s.appendIdentityEvent(ctx, tx, userID, ProviderLocal, l.Subject, IdentityEventPasswordLinkIssued, l.IssuedAt); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: create password link: %w", err)
	}
	return nil
}

// GetPasswordLink returns the open link a token hash names: unused and not expired at now.
// ErrNotFound otherwise, whatever the reason.
func (s *Store) GetPasswordLink(ctx context.Context, tokenHash []byte, now time.Time) (PasswordLink, error) {
	l, err := scanPasswordLink(s.pool.QueryRow(ctx, `SELECT `+passwordLinkColumns+` FROM password_links
		WHERE token_hash = $1 AND used_at IS NULL AND expires_at > $2`, tokenHash, now))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return PasswordLink{}, fmt.Errorf("store: get password link: %w", err)
	}
	return l, err
}

// UsePasswordLink spends an open link and sets its account's password, in one transaction: the
// link is marked used, the credential gets passwordHash with its failures and lockout cleared,
// and the identity log says password_set or password_reset. It returns the link and the member.
// ErrNotFound when the link is unknown, used or expired.
func (s *Store) UsePasswordLink(ctx context.Context, tokenHash []byte, now time.Time, passwordHash string) (PasswordLink, uuid.UUID, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PasswordLink{}, uuid.Nil, fmt.Errorf("store: use password link: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	l, err := scanPasswordLink(tx.QueryRow(ctx, `UPDATE password_links SET used_at = $2
		WHERE token_hash = $1 AND used_at IS NULL AND expires_at > $2 RETURNING `+passwordLinkColumns, tokenHash, now))
	if errors.Is(err, ErrNotFound) {
		return PasswordLink{}, uuid.Nil, err
	}
	if err != nil {
		return PasswordLink{}, uuid.Nil, fmt.Errorf("store: use password link: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE local_credentials SET password_hash = $3, password_updated_at = $4,
		failed_attempts = 0, lockouts = 0, locked_until = NULL WHERE provider = $1 AND subject = $2`,
		ProviderLocal, l.Subject, passwordHash, now); err != nil {
		return PasswordLink{}, uuid.Nil, fmt.Errorf("store: use password link: %w", err)
	}
	var userID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT user_id FROM identities WHERE provider = $1 AND subject = $2`, ProviderLocal, l.Subject).Scan(&userID); err != nil {
		return PasswordLink{}, uuid.Nil, fmt.Errorf("store: use password link: %w", err)
	}
	event := IdentityEventPasswordReset
	if l.Purpose == PasswordLinkSet {
		event = IdentityEventPasswordSet
	}
	if err := s.appendIdentityEvent(ctx, tx, userID, ProviderLocal, l.Subject, event, now); err != nil {
		return PasswordLink{}, uuid.Nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PasswordLink{}, uuid.Nil, fmt.Errorf("store: use password link: %w", err)
	}
	return l, userID, nil
}

// DeleteExpiredPasswordLinks removes links that expired before cutoff, used or not.
func (s *Store) DeleteExpiredPasswordLinks(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM password_links WHERE expires_at < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: delete expired password links: %w", err)
	}
	return tag.RowsAffected(), nil
}
