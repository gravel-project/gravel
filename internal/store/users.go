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

// User is a row of users.
type User struct {
	ID             uuid.UUID
	OrganizationID uuid.UUID
	DisplayName    string
	CreatedAt      time.Time
	LastLoginAt    *time.Time
}

// Identity is a row of identities: one provider account linked to a user.
type Identity struct {
	Provider           string
	Subject            string
	UserID             uuid.UUID
	DisplayName        string
	AvatarURL          string
	VerificationMethod string
	VerifiedAt         time.Time
	LinkedAt           time.Time
	LastLoginAt        *time.Time
}

// Identity events, as written to identity_events.
const (
	IdentityEventRegistered = "registered"
	IdentityEventLogin      = "login"
	IdentityEventLinked     = "linked"
	IdentityEventUnlinked   = "unlinked"
)

// Errors of the identity tables.
var (
	// ErrIdentityExists is returned when a (provider, subject) pair is already linked.
	ErrIdentityExists = errors.New("identity already linked")
	// ErrLastIdentity is returned when an unlink would leave the user with no identity.
	ErrLastIdentity = errors.New("last identity")
)

const userColumns = `id, organization_id, display_name, created_at, last_login_at`

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.OrganizationID, &u.DisplayName, &u.CreatedAt, &u.LastLoginAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

const identityColumns = `provider, subject, user_id, display_name, avatar_url, verification_method, verified_at, linked_at, last_login_at`

func scanIdentity(row pgx.Row) (Identity, error) {
	var i Identity
	err := row.Scan(&i.Provider, &i.Subject, &i.UserID, &i.DisplayName, &i.AvatarURL, &i.VerificationMethod, &i.VerifiedAt, &i.LinkedAt, &i.LastLoginAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Identity{}, ErrNotFound
	}
	return i, err
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// GetUser returns a user or ErrNotFound.
func (s *Store) GetUser(ctx context.Context, id uuid.UUID) (User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return User{}, fmt.Errorf("store: get user: %w", err)
	}
	return u, err
}

// RegisterUser creates a user and their first identity in one transaction and logs the
// registration. ErrIdentityExists if the pair was linked meanwhile (a concurrent first login).
func (s *Store) RegisterUser(ctx context.Context, u User, ident Identity) (User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, fmt.Errorf("store: register user: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	created, err := scanUser(tx.QueryRow(ctx,
		`INSERT INTO users (id, organization_id, display_name, last_login_at) VALUES ($1, $2, $3, $4) RETURNING `+userColumns,
		u.ID, u.OrganizationID, u.DisplayName, ident.VerifiedAt))
	if err != nil {
		return User{}, fmt.Errorf("store: register user: %w", err)
	}
	ident.UserID = created.ID
	ident.LastLoginAt = &ident.VerifiedAt
	if err := insertIdentity(ctx, tx, ident, IdentityEventRegistered); err != nil {
		return User{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, fmt.Errorf("store: register user: %w", err)
	}
	return created, nil
}

// LinkIdentity adds a provider account to an existing user and logs the link.
// ErrIdentityExists if the pair is linked to any user already.
func (s *Store) LinkIdentity(ctx context.Context, ident Identity) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: link identity: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := insertIdentity(ctx, tx, ident, IdentityEventLinked); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: link identity: %w", err)
	}
	return nil
}

func insertIdentity(ctx context.Context, tx pgx.Tx, ident Identity, event string) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO identities (provider, subject, user_id, display_name, avatar_url, verification_method, verified_at, last_login_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		ident.Provider, ident.Subject, ident.UserID, ident.DisplayName, ident.AvatarURL, ident.VerificationMethod, ident.VerifiedAt, ident.LastLoginAt)
	if isUniqueViolation(err) {
		return ErrIdentityExists
	}
	if err != nil {
		return fmt.Errorf("store: insert identity: %w", err)
	}
	return appendIdentityEvent(ctx, tx, ident.UserID, ident.Provider, ident.Subject, event, ident.VerifiedAt)
}

func appendIdentityEvent(ctx context.Context, tx pgx.Tx, userID uuid.UUID, provider, subject, event string, at time.Time) error {
	if _, err := tx.Exec(ctx,
		`INSERT INTO identity_events (user_id, provider, subject, event, at) VALUES ($1, $2, $3, $4, $5)`,
		userID, provider, subject, event, at); err != nil {
		return fmt.Errorf("store: identity event: %w", err)
	}
	return nil
}

// GetIdentity returns the identity for a (provider, subject) pair or ErrNotFound.
func (s *Store) GetIdentity(ctx context.Context, provider, subject string) (Identity, error) {
	i, err := scanIdentity(s.pool.QueryRow(ctx,
		`SELECT `+identityColumns+` FROM identities WHERE provider = $1 AND subject = $2`, provider, subject))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Identity{}, fmt.Errorf("store: get identity: %w", err)
	}
	return i, err
}

// ListIdentities returns a user's identities, oldest link first.
func (s *Store) ListIdentities(ctx context.Context, userID uuid.UUID) ([]Identity, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+identityColumns+` FROM identities WHERE user_id = $1 ORDER BY linked_at, provider, subject`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: list identities: %w", err)
	}
	defer rows.Close()
	var out []Identity
	for rows.Next() {
		i, err := scanIdentity(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list identities: %w", err)
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list identities: %w", err)
	}
	return out, nil
}

// RecordLogin refreshes an identity's display attributes, stamps the login on the identity
// and its user, and logs it. ErrNotFound if the pair is not linked.
func (s *Store) RecordLogin(ctx context.Context, provider, subject, displayName, avatarURL string, at time.Time) (Identity, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Identity{}, fmt.Errorf("store: record login: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ident, err := scanIdentity(tx.QueryRow(ctx,
		`UPDATE identities SET display_name = $3, avatar_url = $4, last_login_at = $5
		  WHERE provider = $1 AND subject = $2 RETURNING `+identityColumns,
		provider, subject, displayName, avatarURL, at))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Identity{}, err
		}
		return Identity{}, fmt.Errorf("store: record login: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET last_login_at = $2 WHERE id = $1`, ident.UserID, at); err != nil {
		return Identity{}, fmt.Errorf("store: record login: %w", err)
	}
	if err := appendIdentityEvent(ctx, tx, ident.UserID, provider, subject, IdentityEventLogin, at); err != nil {
		return Identity{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Identity{}, fmt.Errorf("store: record login: %w", err)
	}
	return ident, nil
}

// RefreshIdentity updates an identity's display attributes without logging a login (a link
// of an account the user already has). ErrNotFound if the pair is not linked.
func (s *Store) RefreshIdentity(ctx context.Context, provider, subject, displayName, avatarURL string) (Identity, error) {
	ident, err := scanIdentity(s.pool.QueryRow(ctx,
		`UPDATE identities SET display_name = $3, avatar_url = $4 WHERE provider = $1 AND subject = $2 RETURNING `+identityColumns,
		provider, subject, displayName, avatarURL))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Identity{}, fmt.Errorf("store: refresh identity: %w", err)
	}
	return ident, err
}

// UnlinkIdentity removes one of a user's identities and logs it. The user's identities are
// locked for the check, so two concurrent unlinks cannot leave zero: ErrLastIdentity if this
// is the only one, ErrNotFound if the pair is not linked to this user.
func (s *Store) UnlinkIdentity(ctx context.Context, userID uuid.UUID, provider, subject string, at time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: unlink identity: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT provider, subject FROM identities WHERE user_id = $1 FOR UPDATE`, userID)
	if err != nil {
		return fmt.Errorf("store: unlink identity: %w", err)
	}
	count, found := 0, false
	for rows.Next() {
		var p, sub string
		if err := rows.Scan(&p, &sub); err != nil {
			rows.Close()
			return fmt.Errorf("store: unlink identity: %w", err)
		}
		count++
		if p == provider && sub == subject {
			found = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: unlink identity: %w", err)
	}
	if !found {
		return ErrNotFound
	}
	if count == 1 {
		return ErrLastIdentity
	}
	if _, err := tx.Exec(ctx, `DELETE FROM identities WHERE provider = $1 AND subject = $2 AND user_id = $3`, provider, subject, userID); err != nil {
		return fmt.Errorf("store: unlink identity: %w", err)
	}
	if err := appendIdentityEvent(ctx, tx, userID, provider, subject, IdentityEventUnlinked, at); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: unlink identity: %w", err)
	}
	return nil
}

// IdentityEvent is a row of identity_events. ID is the append-only log's position: a reader
// that remembers the last id it saw resumes from there (ListIdentityEventsAfter).
type IdentityEvent struct {
	ID       int64
	UserID   uuid.UUID
	Provider string
	Subject  string
	Event    string
	At       time.Time
}

const identityEventColumns = `id, user_id, provider, subject, event, at`

func scanIdentityEvents(rows pgx.Rows) ([]IdentityEvent, error) {
	defer rows.Close()
	var out []IdentityEvent
	for rows.Next() {
		var e IdentityEvent
		if err := rows.Scan(&e.ID, &e.UserID, &e.Provider, &e.Subject, &e.Event, &e.At); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListIdentityEvents returns a user's identity log, oldest first.
func (s *Store) ListIdentityEvents(ctx context.Context, userID uuid.UUID) ([]IdentityEvent, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+identityEventColumns+` FROM identity_events WHERE user_id = $1 ORDER BY id`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: list identity events: %w", err)
	}
	out, err := scanIdentityEvents(rows)
	if err != nil {
		return nil, fmt.Errorf("store: list identity events: %w", err)
	}
	return out, nil
}

// ListIdentityEventsAfter returns up to limit events with an id greater than afterID, oldest
// first: the incremental read a role-sync reconciler makes (ADR-0008).
func (s *Store) ListIdentityEventsAfter(ctx context.Context, afterID int64, limit int) ([]IdentityEvent, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+identityEventColumns+` FROM identity_events WHERE id > $1 ORDER BY id LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list identity events after: %w", err)
	}
	out, err := scanIdentityEvents(rows)
	if err != nil {
		return nil, fmt.Errorf("store: list identity events after: %w", err)
	}
	return out, nil
}

// UserCursor is a position in the users list: the (created_at, id) of the last user seen.
type UserCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// ListUsers pages through an organization's users, oldest first, by (created_at, id); after is
// the previous page's last user, nil for the first page.
func (s *Store) ListUsers(ctx context.Context, orgID uuid.UUID, after *UserCursor, limit int) ([]User, error) {
	var rows pgx.Rows
	var err error
	if after == nil {
		rows, err = s.pool.Query(ctx,
			`SELECT `+userColumns+` FROM users WHERE organization_id = $1 ORDER BY created_at, id LIMIT $2`, orgID, limit)
	} else {
		rows, err = s.pool.Query(ctx,
			`SELECT `+userColumns+` FROM users WHERE organization_id = $1 AND (created_at, id) > ($2, $3) ORDER BY created_at, id LIMIT $4`,
			orgID, after.CreatedAt, after.ID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("store: list users: %w", err)
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list users: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list users: %w", err)
	}
	return out, nil
}

// ListIdentitiesForUsers returns the identities of the given users, ordered by user then by
// when they were linked, so a page of users needs one query for its identities.
func (s *Store) ListIdentitiesForUsers(ctx context.Context, userIDs []uuid.UUID) ([]Identity, error) {
	if len(userIDs) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+identityColumns+` FROM identities WHERE user_id = ANY($1) ORDER BY user_id, linked_at, provider, subject`, userIDs)
	if err != nil {
		return nil, fmt.Errorf("store: list identities for users: %w", err)
	}
	defer rows.Close()
	var out []Identity
	for rows.Next() {
		i, err := scanIdentity(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list identities for users: %w", err)
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list identities for users: %w", err)
	}
	return out, nil
}
