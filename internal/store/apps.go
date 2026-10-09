package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// App is a row of apps: a registered first-party service (ADR-0008) that authenticates with
// client credentials. SecretHash is the SHA-256 of the secret, which is shown once at creation.
type App struct {
	ID             uuid.UUID
	OrganizationID uuid.UUID
	Name           string
	ClientID       string
	SecretHash     []byte
	Scopes         []string
	CreatedAt      time.Time
	RevokedAt      *time.Time
}

// AppToken is a row of app_tokens: a short-lived bearer token an app holds, stored hashed.
type AppToken struct {
	ID        uuid.UUID
	AppID     uuid.UUID
	TokenHash []byte
	Scopes    []string
	CreatedAt time.Time
	ExpiresAt time.Time
}

const appColumns = `id, organization_id, name, client_id, secret_hash, scopes, created_at, revoked_at`

func scanApp(row pgx.Row) (App, error) {
	var a App
	err := row.Scan(&a.ID, &a.OrganizationID, &a.Name, &a.ClientID, &a.SecretHash, &a.Scopes, &a.CreatedAt, &a.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return App{}, ErrNotFound
	}
	return a, err
}

// CreateApp inserts an app.
func (s *Store) CreateApp(ctx context.Context, a App) error {
	if a.Scopes == nil {
		a.Scopes = []string{}
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO apps (`+appColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		a.ID, a.OrganizationID, a.Name, a.ClientID, a.SecretHash, a.Scopes, a.CreatedAt, a.RevokedAt); err != nil {
		return fmt.Errorf("store: create app: %w", err)
	}
	return nil
}

// GetApp returns an app, revoked or not, or ErrNotFound.
func (s *Store) GetApp(ctx context.Context, id uuid.UUID) (App, error) {
	a, err := scanApp(s.pool.QueryRow(ctx, `SELECT `+appColumns+` FROM apps WHERE id = $1`, id))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return App{}, fmt.Errorf("store: get app: %w", err)
	}
	return a, err
}

// GetAppByClientID returns an app, revoked or not, or ErrNotFound.
func (s *Store) GetAppByClientID(ctx context.Context, clientID string) (App, error) {
	a, err := scanApp(s.pool.QueryRow(ctx, `SELECT `+appColumns+` FROM apps WHERE client_id = $1`, clientID))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return App{}, fmt.Errorf("store: get app by client id: %w", err)
	}
	return a, err
}

// ListApps returns every app of an organization, oldest first, revoked ones included.
func (s *Store) ListApps(ctx context.Context, orgID uuid.UUID) ([]App, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+appColumns+` FROM apps WHERE organization_id = $1 ORDER BY created_at, id`, orgID)
	if err != nil {
		return nil, fmt.Errorf("store: list apps: %w", err)
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list apps: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list apps: %w", err)
	}
	return out, nil
}

// RevokeApp marks an app revoked and deletes its tokens, in one transaction. ErrNotFound when
// the app does not exist or is already revoked.
func (s *Store) RevokeApp(ctx context.Context, id uuid.UUID, at time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: revoke app: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE apps SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL`, id, at)
	if err != nil {
		return fmt.Errorf("store: revoke app: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `DELETE FROM app_tokens WHERE app_id = $1`, id); err != nil {
		return fmt.Errorf("store: revoke app: delete tokens: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: revoke app: %w", err)
	}
	return nil
}

// CreateAppToken inserts a token.
func (s *Store) CreateAppToken(ctx context.Context, t AppToken) error {
	if t.Scopes == nil {
		t.Scopes = []string{}
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO app_tokens (id, app_id, token_hash, scopes, created_at, expires_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		t.ID, t.AppID, t.TokenHash, t.Scopes, t.CreatedAt, t.ExpiresAt); err != nil {
		return fmt.Errorf("store: create app token: %w", err)
	}
	return nil
}

// GetAppTokenByHash returns an unexpired token of an unrevoked app, with the app, or ErrNotFound.
func (s *Store) GetAppTokenByHash(ctx context.Context, hash []byte, now time.Time) (AppToken, App, error) {
	var t AppToken
	var a App
	err := s.pool.QueryRow(ctx,
		`SELECT t.id, t.app_id, t.token_hash, t.scopes, t.created_at, t.expires_at,
		        a.id, a.organization_id, a.name, a.client_id, a.secret_hash, a.scopes, a.created_at, a.revoked_at
		   FROM app_tokens t JOIN apps a ON a.id = t.app_id
		  WHERE t.token_hash = $1 AND t.expires_at > $2 AND a.revoked_at IS NULL`, hash, now).
		Scan(&t.ID, &t.AppID, &t.TokenHash, &t.Scopes, &t.CreatedAt, &t.ExpiresAt,
			&a.ID, &a.OrganizationID, &a.Name, &a.ClientID, &a.SecretHash, &a.Scopes, &a.CreatedAt, &a.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AppToken{}, App{}, ErrNotFound
	}
	if err != nil {
		return AppToken{}, App{}, fmt.Errorf("store: get app token: %w", err)
	}
	return t, a, nil
}

// DeleteExpiredAppTokens removes tokens past their expiry and reports how many.
func (s *Store) DeleteExpiredAppTokens(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM app_tokens WHERE expires_at <= $1`, now)
	if err != nil {
		return 0, fmt.Errorf("store: delete expired app tokens: %w", err)
	}
	return tag.RowsAffected(), nil
}
