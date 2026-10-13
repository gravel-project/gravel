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

// App is a row of apps: a registered first-party service (ADR-0008) that authenticates with
// client credentials. SecretHash is the SHA-256 of the secret, which is shown once at creation.
type App struct {
	ID             uuid.UUID
	OrganizationID uuid.UUID
	Name           string
	ClientID       string
	SecretHash     []byte
	Scopes         []string
	// RedirectURIs are where the authorization endpoint may send a member back (ADR-0015):
	// exact strings, compared as strings.
	RedirectURIs []string
	CreatedAt    time.Time
	RevokedAt    *time.Time
}

// AppToken is a row of app_tokens: a short-lived bearer token an app holds, stored hashed. A
// member's token (ADR-0015) carries the member's UserID and the authorization code it was issued
// from; an app's own token has neither.
type AppToken struct {
	ID        uuid.UUID
	AppID     uuid.UUID
	TokenHash []byte
	Scopes    []string
	CreatedAt time.Time
	ExpiresAt time.Time
	UserID    *uuid.UUID
	CodeID    *uuid.UUID
}

const appColumns = `id, organization_id, name, client_id, secret_hash, scopes, redirect_uris, created_at, revoked_at`

func scanApp(row pgx.Row) (App, error) {
	var a App
	err := row.Scan(&a.ID, &a.OrganizationID, &a.Name, &a.ClientID, &a.SecretHash, &a.Scopes, &a.RedirectURIs, &a.CreatedAt, &a.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return App{}, ErrNotFound
	}
	return a, err
}

// CreateApp inserts an app and records it (app.created, by by) in one transaction.
func (s *Store) CreateApp(ctx context.Context, a App, by Actor) error {
	if a.Scopes == nil {
		a.Scopes = []string{}
	}
	if a.RedirectURIs == nil {
		a.RedirectURIs = []string{}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: create app: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`INSERT INTO apps (`+appColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		a.ID, a.OrganizationID, a.Name, a.ClientID, a.SecretHash, a.Scopes, a.RedirectURIs, a.CreatedAt, a.RevokedAt); err != nil {
		return fmt.Errorf("store: create app: %w", err)
	}
	if err := s.appendEvent(ctx, tx, by.event(Event{OrganizationID: a.OrganizationID, At: a.CreatedAt, Kind: "app.created",
		Summary: fmt.Sprintf("The app %q was registered", a.Name), Detail: appDetail(a), Source: "apps.created", SourceID: a.ID.String()})); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: create app: %w", err)
	}
	return nil
}

// appDetail is what an app's events say about it: never its secret.
func appDetail(a App) map[string]any {
	return map[string]any{"app_id": a.ID.String(), "client_id": a.ClientID, "name": a.Name, "scopes": nonNilStrings(a.Scopes), "redirect_uris": nonNilStrings(a.RedirectURIs)}
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

// RevokeApp marks an app revoked and deletes its tokens (its own and its members'), the grants
// members gave it and its authorization codes, and records it (app.revoked, by by), in one
// transaction. ErrNotFound when the app does not exist or is already revoked.
func (s *Store) RevokeApp(ctx context.Context, id uuid.UUID, at time.Time, by Actor) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: revoke app: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	a, err := scanApp(tx.QueryRow(ctx, `UPDATE apps SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL RETURNING `+appColumns, id, at))
	if errors.Is(err, ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: revoke app: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM app_tokens WHERE app_id = $1`, id); err != nil {
		return fmt.Errorf("store: revoke app: delete tokens: %w", err)
	}
	grants, err := tx.Exec(ctx, `DELETE FROM app_grants WHERE app_id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: revoke app: delete grants: %w", err)
	}
	detail := appDetail(a)
	detail["grants_removed"] = grants.RowsAffected()
	if err := s.appendEvent(ctx, tx, by.event(Event{OrganizationID: a.OrganizationID, At: at, Kind: "app.revoked",
		Summary: fmt.Sprintf("The app %q was revoked", a.Name), Detail: detail, Source: "apps.revoked", SourceID: a.ID.String()})); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM authorization_codes WHERE app_id = $1`, id); err != nil {
		return fmt.Errorf("store: revoke app: delete codes: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: revoke app: %w", err)
	}
	return nil
}

// CreateAppToken inserts a token.
func (s *Store) CreateAppToken(ctx context.Context, t AppToken) error {
	if err := insertAppToken(ctx, s.pool, t); err != nil {
		return fmt.Errorf("store: create app token: %w", err)
	}
	return nil
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func insertAppToken(ctx context.Context, db execer, t AppToken) error {
	if t.Scopes == nil {
		t.Scopes = []string{}
	}
	_, err := db.Exec(ctx,
		`INSERT INTO app_tokens (id, app_id, token_hash, scopes, created_at, expires_at, user_id, code_id) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		t.ID, t.AppID, t.TokenHash, t.Scopes, t.CreatedAt, t.ExpiresAt, t.UserID, t.CodeID)
	return err
}

// GetAppTokenByHash returns an unexpired token of an unrevoked app, with the app, or ErrNotFound.
func (s *Store) GetAppTokenByHash(ctx context.Context, hash []byte, now time.Time) (AppToken, App, error) {
	var t AppToken
	var a App
	err := s.pool.QueryRow(ctx,
		`SELECT t.id, t.app_id, t.token_hash, t.scopes, t.created_at, t.expires_at, t.user_id, t.code_id,
		        a.id, a.organization_id, a.name, a.client_id, a.secret_hash, a.scopes, a.redirect_uris, a.created_at, a.revoked_at
		   FROM app_tokens t JOIN apps a ON a.id = t.app_id
		  WHERE t.token_hash = $1 AND t.expires_at > $2 AND a.revoked_at IS NULL`, hash, now).
		Scan(&t.ID, &t.AppID, &t.TokenHash, &t.Scopes, &t.CreatedAt, &t.ExpiresAt, &t.UserID, &t.CodeID,
			&a.ID, &a.OrganizationID, &a.Name, &a.ClientID, &a.SecretHash, &a.Scopes, &a.RedirectURIs, &a.CreatedAt, &a.RevokedAt)
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
