package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrCodeReplayed is an authorization code presented after it was used: the tokens issued from it
// have been deleted (RFC 6749 §4.1.2).
var ErrCodeReplayed = errors.New("store: authorization code already used")

// AuthorizationCode is a row of authorization_codes (ADR-0015): hashed, single use, bound to the
// app, the redirect URI, the PKCE challenge and the member.
type AuthorizationCode struct {
	ID            uuid.UUID
	CodeHash      []byte
	AppID         uuid.UUID
	UserID        uuid.UUID
	RedirectURI   string
	CodeChallenge string
	Scopes        []string
	CreatedAt     time.Time
	ExpiresAt     time.Time
	UsedAt        *time.Time
}

// AppGrant is a row of app_grants: a member allowed an app these scopes. ClientID and AppName are
// read back with the app (not stored on the grant).
type AppGrant struct {
	UserID    uuid.UUID
	AppID     uuid.UUID
	Scopes    []string
	GrantedAt time.Time
	ClientID  string
	AppName   string
}

const codeColumns = `id, code_hash, app_id, user_id, redirect_uri, code_challenge, scopes, created_at, expires_at, used_at`

// CreateAuthorizationCode inserts a code.
func (s *Store) CreateAuthorizationCode(ctx context.Context, c AuthorizationCode) error {
	if c.Scopes == nil {
		c.Scopes = []string{}
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO authorization_codes (`+codeColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		c.ID, c.CodeHash, c.AppID, c.UserID, c.RedirectURI, c.CodeChallenge, c.Scopes, c.CreatedAt, c.ExpiresAt, c.UsedAt); err != nil {
		return fmt.Errorf("store: create authorization code: %w", err)
	}
	return nil
}

// RedeemAuthorizationCode spends a code, once, in one transaction that holds the code's row:
//   - an unknown code is ErrNotFound;
//   - a code already used deletes every token issued from it and answers ErrCodeReplayed;
//   - otherwise the code is marked used, and issue decides: a token it returns is stored with the
//     code's id; an error it returns is answered as is, and the code stays spent.
//
// Holding the row serialises two presentations of one code, so a replay racing the first exchange
// still finds and revokes the token that exchange issued.
func (s *Store) RedeemAuthorizationCode(ctx context.Context, hash []byte, now time.Time, issue func(AuthorizationCode) (AppToken, error)) (AuthorizationCode, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AuthorizationCode{}, fmt.Errorf("store: redeem authorization code: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var c AuthorizationCode
	err = tx.QueryRow(ctx, `SELECT `+codeColumns+` FROM authorization_codes WHERE code_hash = $1 FOR UPDATE`, hash).
		Scan(&c.ID, &c.CodeHash, &c.AppID, &c.UserID, &c.RedirectURI, &c.CodeChallenge, &c.Scopes, &c.CreatedAt, &c.ExpiresAt, &c.UsedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AuthorizationCode{}, ErrNotFound
	}
	if err != nil {
		return AuthorizationCode{}, fmt.Errorf("store: redeem authorization code: %w", err)
	}
	if c.UsedAt != nil {
		if _, err := tx.Exec(ctx, `DELETE FROM app_tokens WHERE code_id = $1`, c.ID); err != nil {
			return c, fmt.Errorf("store: redeem authorization code: revoke: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return c, fmt.Errorf("store: redeem authorization code: revoke: %w", err)
		}
		return c, ErrCodeReplayed
	}
	if _, err := tx.Exec(ctx, `UPDATE authorization_codes SET used_at = $2 WHERE id = $1`, c.ID, now); err != nil {
		return c, fmt.Errorf("store: redeem authorization code: %w", err)
	}
	used := now
	c.UsedAt = &used
	t, issueErr := issue(c)
	if issueErr == nil {
		t.CodeID = &c.ID
		if err := insertAppToken(ctx, tx, t); err != nil {
			return c, fmt.Errorf("store: redeem authorization code: token: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return c, fmt.Errorf("store: redeem authorization code: %w", err)
	}
	return c, issueErr
}

// DeleteExpiredAuthorizationCodes removes codes that expired before a time and reports how many.
// The caller passes a time far enough back that no token issued from them is still alive, so a
// replay is recognised for as long as it could matter.
func (s *Store) DeleteExpiredAuthorizationCodes(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM authorization_codes WHERE expires_at <= $1`, before)
	if err != nil {
		return 0, fmt.Errorf("store: delete expired authorization codes: %w", err)
	}
	return tag.RowsAffected(), nil
}

// GetAppGrant returns the member's grant to an app, or ErrNotFound.
func (s *Store) GetAppGrant(ctx context.Context, userID, appID uuid.UUID) (AppGrant, error) {
	var g AppGrant
	err := s.pool.QueryRow(ctx,
		`SELECT g.user_id, g.app_id, g.scopes, g.granted_at, a.client_id, a.name
		   FROM app_grants g JOIN apps a ON a.id = g.app_id
		  WHERE g.user_id = $1 AND g.app_id = $2`, userID, appID).
		Scan(&g.UserID, &g.AppID, &g.Scopes, &g.GrantedAt, &g.ClientID, &g.AppName)
	if errors.Is(err, pgx.ErrNoRows) {
		return AppGrant{}, ErrNotFound
	}
	if err != nil {
		return AppGrant{}, fmt.Errorf("store: get app grant: %w", err)
	}
	return g, nil
}

// SaveAppGrant inserts the member's grant to an app, or replaces its scopes and time.
func (s *Store) SaveAppGrant(ctx context.Context, g AppGrant) error {
	if g.Scopes == nil {
		g.Scopes = []string{}
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO app_grants (user_id, app_id, scopes, granted_at) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (user_id, app_id) DO UPDATE SET scopes = EXCLUDED.scopes, granted_at = EXCLUDED.granted_at`,
		g.UserID, g.AppID, g.Scopes, g.GrantedAt); err != nil {
		return fmt.Errorf("store: save app grant: %w", err)
	}
	return nil
}

// ListAppGrants returns a member's grants to apps that are not revoked, the oldest first.
func (s *Store) ListAppGrants(ctx context.Context, userID uuid.UUID) ([]AppGrant, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT g.user_id, g.app_id, g.scopes, g.granted_at, a.client_id, a.name
		   FROM app_grants g JOIN apps a ON a.id = g.app_id
		  WHERE g.user_id = $1 AND a.revoked_at IS NULL
		  ORDER BY g.granted_at, a.client_id`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: list app grants: %w", err)
	}
	defer rows.Close()
	var out []AppGrant
	for rows.Next() {
		var g AppGrant
		if err := rows.Scan(&g.UserID, &g.AppID, &g.Scopes, &g.GrantedAt, &g.ClientID, &g.AppName); err != nil {
			return nil, fmt.Errorf("store: list app grants: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list app grants: %w", err)
	}
	return out, nil
}

// DeleteAppGrant revokes a member's grant to an app with everything it gave the app: the
// member's tokens for it and the codes not yet exchanged, in one transaction. It reports how many
// tokens went; ErrNotFound when there was no grant.
func (s *Store) DeleteAppGrant(ctx context.Context, userID, appID uuid.UUID) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: delete app grant: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `DELETE FROM app_grants WHERE user_id = $1 AND app_id = $2`, userID, appID)
	if err != nil {
		return 0, fmt.Errorf("store: delete app grant: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return 0, ErrNotFound
	}
	tokens, err := tx.Exec(ctx, `DELETE FROM app_tokens WHERE user_id = $1 AND app_id = $2`, userID, appID)
	if err != nil {
		return 0, fmt.Errorf("store: delete app grant: tokens: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM authorization_codes WHERE user_id = $1 AND app_id = $2`, userID, appID); err != nil {
		return 0, fmt.Errorf("store: delete app grant: codes: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: delete app grant: %w", err)
	}
	return tokens.RowsAffected(), nil
}
