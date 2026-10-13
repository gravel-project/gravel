package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ProviderLocal is the provider of a local account: a username and a password (ADR-0026).
const ProviderLocal = "local"

// LocalCredential is a local account's password and its lockout state; the identity it belongs to
// is ('local', Subject).
type LocalCredential struct {
	Subject           string
	PasswordHash      string // a PHC string, never a password
	Email             *string
	EmailVerifiedAt   *time.Time
	PasswordUpdatedAt time.Time
	FailedAttempts    int
	Lockouts          int
	LockedUntil       *time.Time
}

// Lockout is when failed logins lock a local account: after Threshold failures in a row, for
// Base, doubled for each lockout since the last success, at most Max.
type Lockout struct {
	Threshold int
	Base, Max time.Duration
}

const localColumns = `subject, password_hash, email, email_verified_at, password_updated_at, failed_attempts, lockouts, locked_until`

func scanLocal(row pgx.Row) (LocalCredential, error) {
	var c LocalCredential
	err := row.Scan(&c.Subject, &c.PasswordHash, &c.Email, &c.EmailVerifiedAt, &c.PasswordUpdatedAt, &c.FailedAttempts, &c.Lockouts, &c.LockedUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return LocalCredential{}, ErrNotFound
	}
	return c, err
}

func insertLocalCredential(ctx context.Context, tx pgx.Tx, subject string, c LocalCredential) error {
	if _, err := tx.Exec(ctx,
		`INSERT INTO local_credentials (provider, subject, password_hash, email, password_updated_at) VALUES ($1, $2, $3, $4, $5)`,
		ProviderLocal, subject, c.PasswordHash, c.Email, c.PasswordUpdatedAt); err != nil {
		return fmt.Errorf("store: insert local credential: %w", err)
	}
	return nil
}

// GetLocalCredential returns a local account's credential, or ErrNotFound.
func (s *Store) GetLocalCredential(ctx context.Context, subject string) (LocalCredential, error) {
	c, err := scanLocal(s.pool.QueryRow(ctx, `SELECT `+localColumns+` FROM local_credentials WHERE provider = $1 AND subject = $2`, ProviderLocal, subject))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return LocalCredential{}, fmt.Errorf("store: get local credential: %w", err)
	}
	return c, err
}

// RecordLocalFailure counts a failed login, in one statement so concurrent failures all count.
// The failure that reaches the threshold locks the account until now + Base·2^lockouts (at most
// Max) and starts the count again. ErrNotFound for an unknown account.
func (s *Store) RecordLocalFailure(ctx context.Context, subject string, now time.Time, l Lockout) (LocalCredential, error) {
	c, err := scanLocal(s.pool.QueryRow(ctx, `
		UPDATE local_credentials SET
		  failed_attempts = CASE WHEN failed_attempts + 1 >= $3 THEN 0 ELSE failed_attempts + 1 END,
		  lockouts        = CASE WHEN failed_attempts + 1 >= $3 THEN lockouts + 1 ELSE lockouts END,
		  locked_until    = CASE WHEN failed_attempts + 1 >= $3
		                         THEN $2::timestamptz + make_interval(secs => LEAST($4::double precision * power(2, LEAST(lockouts, 30)), $5::double precision))
		                         ELSE locked_until END
		WHERE provider = $6 AND subject = $1
		RETURNING `+localColumns,
		subject, now, l.Threshold, l.Base.Seconds(), l.Max.Seconds(), ProviderLocal))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return LocalCredential{}, fmt.Errorf("store: record local failure: %w", err)
	}
	return c, err
}

// RecordLocalSuccess clears a local account's failures and lockouts after a good login, and
// replaces its hash when rehash is set (the hashing parameters changed; the password did not).
// ErrNotFound for an unknown account.
func (s *Store) RecordLocalSuccess(ctx context.Context, subject, rehash string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE local_credentials SET failed_attempts = 0, lockouts = 0, locked_until = NULL,
		  password_hash = CASE WHEN $2 = '' THEN password_hash ELSE $2 END
		WHERE provider = $3 AND subject = $1`,
		subject, rehash, ProviderLocal)
	if err != nil {
		return fmt.Errorf("store: record local success: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
