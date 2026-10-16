package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Identity events of a recovery address and a mailed reset (gravel#234).
const (
	IdentityEventEmailVerified       = "email_verified"
	IdentityEventPasswordResetMailed = "password_reset_mailed"
)

// EmailVerification is an open link that verifies a local account's recovery address. TokenHash is
// the SHA-256 of the token in the link; Email is the address it was mailed to.
type EmailVerification struct {
	TokenHash []byte
	Subject   string
	Email     string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// SetRecoveryEmail sets a local account's recovery address, or removes it (email nil), in one
// transaction: a changed address loses its verification, every open verification link of the
// account ends, and v, when set, is stored as the new one (v.Email is email). The same address
// again keeps its verification. ErrNotFound for an unknown account.
func (s *Store) SetRecoveryEmail(ctx context.Context, subject string, email *string, v *EmailVerification) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: set recovery email: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE local_credentials SET email = $3,
		email_verified_at = CASE WHEN email IS NOT DISTINCT FROM $3 THEN email_verified_at END
		WHERE provider = $1 AND subject = $2`, ProviderLocal, subject, email)
	if err != nil {
		return fmt.Errorf("store: set recovery email: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `DELETE FROM email_verifications WHERE provider = $1 AND subject = $2`, ProviderLocal, subject); err != nil {
		return fmt.Errorf("store: set recovery email: %w", err)
	}
	if v != nil {
		if email == nil || v.Email != *email {
			return errors.New("store: set recovery email: the link is for another address")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO email_verifications (token_hash, provider, subject, email, issued_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6)`, v.TokenHash, ProviderLocal, subject, v.Email, v.IssuedAt, v.ExpiresAt); err != nil {
			return fmt.Errorf("store: set recovery email: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: set recovery email: %w", err)
	}
	return nil
}

const emailVerificationColumns = `v.token_hash, v.subject, v.email, v.issued_at, v.expires_at`

func scanEmailVerification(row pgx.Row) (EmailVerification, error) {
	var v EmailVerification
	err := row.Scan(&v.TokenHash, &v.Subject, &v.Email, &v.IssuedAt, &v.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return EmailVerification{}, ErrNotFound
	}
	return v, err
}

// GetEmailVerification returns the open link a token hash names: not expired at now, for the
// address the account still has. ErrNotFound otherwise, whatever the reason.
func (s *Store) GetEmailVerification(ctx context.Context, tokenHash []byte, now time.Time) (EmailVerification, error) {
	v, err := scanEmailVerification(s.pool.QueryRow(ctx, `SELECT `+emailVerificationColumns+`
		  FROM email_verifications v JOIN local_credentials c ON c.provider = v.provider AND c.subject = v.subject
		 WHERE v.token_hash = $1 AND v.expires_at > $2 AND c.email = v.email`, tokenHash, now))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return EmailVerification{}, fmt.Errorf("store: get email verification: %w", err)
	}
	return v, err
}

// VerifyRecoveryEmail spends an open link and marks its address verified, in one transaction, with
// email_verified on the identity log. It returns the link and the member. ErrNotFound when the
// link is unknown, expired, or for an address the account no longer has.
func (s *Store) VerifyRecoveryEmail(ctx context.Context, tokenHash []byte, now time.Time) (EmailVerification, uuid.UUID, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return EmailVerification{}, uuid.Nil, fmt.Errorf("store: verify recovery email: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	v, err := scanEmailVerification(tx.QueryRow(ctx, `DELETE FROM email_verifications v WHERE v.token_hash = $1 AND v.expires_at > $2
		RETURNING `+emailVerificationColumns, tokenHash, now))
	if errors.Is(err, ErrNotFound) {
		return EmailVerification{}, uuid.Nil, err
	}
	if err != nil {
		return EmailVerification{}, uuid.Nil, fmt.Errorf("store: verify recovery email: %w", err)
	}
	tag, err := tx.Exec(ctx, `UPDATE local_credentials SET email_verified_at = $4 WHERE provider = $1 AND subject = $2 AND email = $3`,
		ProviderLocal, v.Subject, v.Email, now)
	if err != nil {
		return EmailVerification{}, uuid.Nil, fmt.Errorf("store: verify recovery email: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return EmailVerification{}, uuid.Nil, ErrNotFound
	}
	var userID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT user_id FROM identities WHERE provider = $1 AND subject = $2`, ProviderLocal, v.Subject).Scan(&userID); err != nil {
		return EmailVerification{}, uuid.Nil, fmt.Errorf("store: verify recovery email: %w", err)
	}
	if err := s.appendIdentityEvent(ctx, tx, userID, ProviderLocal, v.Subject, IdentityEventEmailVerified, now); err != nil {
		return EmailVerification{}, uuid.Nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return EmailVerification{}, uuid.Nil, fmt.Errorf("store: verify recovery email: %w", err)
	}
	return v, userID, nil
}

// DeleteExpiredEmailVerifications removes links that expired before cutoff.
func (s *Store) DeleteExpiredEmailVerifications(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM email_verifications WHERE expires_at < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: delete expired email verifications: %w", err)
	}
	return tag.RowsAffected(), nil
}
