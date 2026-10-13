package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/authz"
	"github.com/gravel-project/gravel/internal/store"
)

// Role grants (ADR-0027 §3 to §5): who may grant what, in one place.

// Errors of role grants.
var (
	ErrUnknownRole  = errors.New("no such role: admin, operator or moderator")
	ErrUnknownGame  = errors.New("no such game on this hub")
	ErrGrantRefused = errors.New("not allowed to change that grant")
)

// SetRole grants or revokes a member's role, instance-wide (gameID "") or for one game, on behalf
// of by, and returns the member. The rules (ADR-0027 §5):
//
//   - by holds roles:grant for the grant's scope: instance-wide for an instance-wide grant, or for
//     the game; so a game-scoped admin changes grants within that game only;
//   - only the owner changes an admin grant;
//   - an app changes moderator grants only (Discord role sync maps onto moderator, never higher);
//   - nobody changes their own grants.
//
// ErrUnknownRole, ErrUnknownGame, ErrGrantRefused (with why), store.ErrNotFound for an unknown
// member. A change that changes nothing is not an error.
func (s *Service) SetRole(ctx context.Context, by authz.Principal, userID uuid.UUID, role, gameID string, granted bool) (User, error) {
	if !authz.IsGrantable(role) {
		return User{}, ErrUnknownRole
	}
	refuse := func(why string) (User, error) { return User{}, fmt.Errorf("%w: %s", ErrGrantRefused, why) }
	if !by.Has(authz.RolesGrant, authz.Target{GameID: gameID}) {
		if gameID == "" {
			return refuse("an instance-wide grant needs roles:grant for every game")
		}
		return refuse("needs roles:grant for " + gameID)
	}
	ch := store.RoleChange{At: s.now()}
	switch by.Kind {
	case authz.App:
		if role != store.RoleModerator {
			return refuse("an app grants moderator only")
		}
		ch.ByApp = &by.AppID
	case authz.InPerson, authz.MemberToken:
		if by.UserID == userID {
			return refuse("nobody changes their own grants")
		}
		if role == store.RoleAdmin && !by.Owner {
			return refuse("only the owner changes an admin grant")
		}
		ch.By = &by.UserID
	default:
		return refuse("nobody is calling")
	}
	changed, err := s.st.SetRoleGrant(ctx, userID, role, gameID, granted, ch)
	switch {
	case errors.Is(err, store.ErrUnknownGame):
		return User{}, ErrUnknownGame
	case err != nil:
		return User{}, err
	}
	if changed {
		scope := gameID
		if scope == "" {
			scope = "instance"
		}
		s.logger.InfoContext(ctx, "role grant changed", "user_id", userID, "role", role, "scope", scope, "granted", granted, "by", ch.By, "by_app", ch.ByApp)
	}
	return s.Me(ctx, userID)
}

// RoleGrants are every grant on the hub, or the given members' (ListRoleGrants).
func (s *Service) RoleGrants(ctx context.Context, userIDs []uuid.UUID) ([]store.RoleGrant, error) {
	return s.st.RoleGrants(ctx, userIDs)
}
