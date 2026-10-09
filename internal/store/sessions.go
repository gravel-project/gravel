package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Session is a row of sessions: one logged-in browser.
type Session struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	TokenHash  []byte
	CSRFToken  string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
}

const sessionColumns = `id, user_id, token_hash, csrf_token, created_at, expires_at, last_seen_at`

func scanSession(row pgx.Row) (Session, error) {
	var s Session
	err := row.Scan(&s.ID, &s.UserID, &s.TokenHash, &s.CSRFToken, &s.CreatedAt, &s.ExpiresAt, &s.LastSeenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	return s, err
}

// CreateSession inserts a session.
func (s *Store) CreateSession(ctx context.Context, sess Session) error {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO sessions (id, user_id, token_hash, csrf_token, created_at, expires_at, last_seen_at) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		sess.ID, sess.UserID, sess.TokenHash, sess.CSRFToken, sess.CreatedAt, sess.ExpiresAt, sess.LastSeenAt); err != nil {
		return fmt.Errorf("store: create session: %w", err)
	}
	return nil
}

// GetSessionByTokenHash returns the unexpired session for a cookie token's hash, or ErrNotFound.
func (s *Store) GetSessionByTokenHash(ctx context.Context, hash []byte, now time.Time) (Session, error) {
	sess, err := scanSession(s.pool.QueryRow(ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE token_hash = $1 AND expires_at > $2`, hash, now))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Session{}, fmt.Errorf("store: get session: %w", err)
	}
	return sess, err
}

// TouchSession records when a session was last used.
func (s *Store) TouchSession(ctx context.Context, id uuid.UUID, lastSeen time.Time) error {
	if _, err := s.pool.Exec(ctx, `UPDATE sessions SET last_seen_at = $2 WHERE id = $1`, id, lastSeen); err != nil {
		return fmt.Errorf("store: touch session: %w", err)
	}
	return nil
}

// DeleteSession ends one session. Deleting a session that is gone is not an error.
func (s *Store) DeleteSession(ctx context.Context, id uuid.UUID) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, id); err != nil {
		return fmt.Errorf("store: delete session: %w", err)
	}
	return nil
}

// DeleteUserSessions ends every session of a user ("log out everywhere") and reports how many.
func (s *Store) DeleteUserSessions(ctx context.Context, userID uuid.UUID) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, userID)
	if err != nil {
		return 0, fmt.Errorf("store: delete user sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

// DeleteExpiredSessions removes sessions past their expiry and reports how many.
func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= $1`, now)
	if err != nil {
		return 0, fmt.Errorf("store: delete expired sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

// AuthAttempt is a row of auth_attempts: an authentication in progress.
type AuthAttempt struct {
	ID              uuid.UUID
	TokenHash       []byte
	Provider        string
	Intent          string
	UserID          *uuid.UUID
	State           string
	ProviderSession string
	CreatedAt       time.Time
	ExpiresAt       time.Time
}

const attemptColumns = `id, token_hash, provider, intent, user_id, state, provider_session, created_at, expires_at`

// CreateAuthAttempt inserts an attempt.
func (s *Store) CreateAuthAttempt(ctx context.Context, a AuthAttempt) error {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO auth_attempts (`+attemptColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		a.ID, a.TokenHash, a.Provider, a.Intent, a.UserID, a.State, a.ProviderSession, a.CreatedAt, a.ExpiresAt); err != nil {
		return fmt.Errorf("store: create auth attempt: %w", err)
	}
	return nil
}

// ConsumeAuthAttempt deletes and returns the attempt for a cookie token's hash, so it can be
// completed exactly once. ErrNotFound if there is none or it has expired (an expired row is
// deleted as well).
func (s *Store) ConsumeAuthAttempt(ctx context.Context, hash []byte, now time.Time) (AuthAttempt, error) {
	var a AuthAttempt
	err := s.pool.QueryRow(ctx, `DELETE FROM auth_attempts WHERE token_hash = $1 RETURNING `+attemptColumns, hash).
		Scan(&a.ID, &a.TokenHash, &a.Provider, &a.Intent, &a.UserID, &a.State, &a.ProviderSession, &a.CreatedAt, &a.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AuthAttempt{}, ErrNotFound
	}
	if err != nil {
		return AuthAttempt{}, fmt.Errorf("store: consume auth attempt: %w", err)
	}
	if !a.ExpiresAt.After(now) {
		return AuthAttempt{}, ErrNotFound
	}
	return a, nil
}

// DeleteExpiredAuthAttempts removes attempts past their expiry and reports how many.
func (s *Store) DeleteExpiredAuthAttempts(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM auth_attempts WHERE expires_at <= $1`, now)
	if err != nil {
		return 0, fmt.Errorf("store: delete expired auth attempts: %w", err)
	}
	return tag.RowsAffected(), nil
}
