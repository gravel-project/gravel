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

// RoleGrant is a role a member holds (ADR-0027 §4): instance-wide when GameID is empty.
type RoleGrant struct {
	UserID       uuid.UUID
	Role         string
	GameID       string
	GrantedBy    *uuid.UUID // a member, or nil
	GrantedByApp *uuid.UUID // an app, or nil; both nil: a migration or the host
	GrantedAt    time.Time
}

// ErrUnknownGame is a grant for a game the hub doesn't have.
var ErrUnknownGame = errors.New("unknown game")

// RoleChange is who changes a grant and when.
type RoleChange struct {
	By    *uuid.UUID
	ByApp *uuid.UUID
	At    time.Time
}

// SetRoleGrant grants or revokes a role for a member, instance-wide (gameID "") or for one game,
// and appends it to role_changes, in one transaction. A change that changes nothing records
// nothing and reports false. ErrNotFound for an unknown member, ErrUnknownGame for a game the
// hub doesn't have.
func (s *Store) SetRoleGrant(ctx context.Context, userID uuid.UUID, role, gameID string, granted bool, ch RoleChange) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("store: set role grant: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var orgID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT organization_id FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&orgID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, fmt.Errorf("store: set role grant: %w", err)
	}
	game := nullString(gameID)
	var n int64
	if granted {
		tag, err := tx.Exec(ctx, `INSERT INTO role_grants (organization_id, user_id, role, game_id, granted_by, granted_by_app, granted_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (user_id, role, game_id) DO NOTHING`, orgID, userID, role, game, ch.By, ch.ByApp, ch.At)
		if isForeignKeyViolation(err) {
			return false, ErrUnknownGame
		}
		if err != nil {
			return false, fmt.Errorf("store: set role grant: %w", err)
		}
		n = tag.RowsAffected()
	} else {
		tag, err := tx.Exec(ctx, `DELETE FROM role_grants WHERE user_id = $1 AND role = $2 AND game_id IS NOT DISTINCT FROM $3`, userID, role, game)
		if err != nil {
			return false, fmt.Errorf("store: set role grant: %w", err)
		}
		n = tag.RowsAffected()
	}
	if n == 0 {
		return false, nil
	}
	var changeID int64
	if err := tx.QueryRow(ctx, `INSERT INTO role_changes (user_id, role, granted, by_user_id, by_app_id, game_id, at) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		userID, role, granted, ch.By, ch.ByApp, game, ch.At).Scan(&changeID); err != nil {
		return false, fmt.Errorf("store: set role grant: record: %w", err)
	}
	kind, verb := "role.revoked", " lost "
	if granted {
		kind, verb = "role.granted", " was granted "
	}
	scope := ""
	if gameID != "" {
		scope = " for " + gameID
	}
	if err := s.appendEvent(ctx, tx, Event{OrganizationID: orgID, At: ch.At, Kind: kind, ActorUserID: ch.By, ActorAppID: ch.ByApp,
		SubjectUserID: &userID, GameID: gameID, Summary: "{user}" + verb + role + scope, Detail: map[string]any{"role": role, "game_id": gameID},
		Source: "role_changes", SourceID: fmt.Sprint(changeID)}); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: set role grant: %w", err)
	}
	return true, nil
}

// RoleGrants returns the grants of the given members (all members when userIDs is empty), by
// member, then role, then game.
func (s *Store) RoleGrants(ctx context.Context, userIDs []uuid.UUID) ([]RoleGrant, error) {
	q := `SELECT user_id, role, coalesce(game_id, ''), granted_by, granted_by_app, granted_at FROM role_grants`
	args := []any{}
	if len(userIDs) > 0 {
		q += ` WHERE user_id = ANY($1)`
		args = append(args, userIDs)
	}
	rows, err := s.pool.Query(ctx, q+` ORDER BY user_id, role, game_id NULLS FIRST`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: role grants: %w", err)
	}
	defer rows.Close()
	var out []RoleGrant
	for rows.Next() {
		var g RoleGrant
		if err := rows.Scan(&g.UserID, &g.Role, &g.GameID, &g.GrantedBy, &g.GrantedByApp, &g.GrantedAt); err != nil {
			return nil, fmt.Errorf("store: role grants: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: role grants: %w", err)
	}
	return out, nil
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
