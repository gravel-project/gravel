package identity_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/authz"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/store"
)

// ADR-0027 §5: who may grant what.
func TestSetRoleRules(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.st.Games = []string{"cs2", "wardogs"}
	jo, sam := f.login(t, "jo").User, f.login(t, "sam").User
	owner := authz.ForMember(uuid.New(), true, nil)
	set := func(by authz.Principal, who uuid.UUID, role, game string, granted bool) error {
		_, err := f.svc.SetRole(ctx, by, who, role, game, granted)
		return err
	}
	refused := func(err error) bool { return errors.Is(err, identity.ErrGrantRefused) }

	if err := set(owner, jo.ID, "owner", "", true); !errors.Is(err, identity.ErrUnknownRole) {
		t.Errorf("owner is not a grant: %v", err)
	}
	if err := set(owner, jo.ID, "moderator", "halo", true); !errors.Is(err, identity.ErrUnknownGame) {
		t.Errorf("unknown game: %v", err)
	}
	if err := set(owner, uuid.New(), "moderator", "", true); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown member: %v", err)
	}
	if err := set(owner, jo.ID, "admin", "", true); err != nil {
		t.Fatal(err)
	}
	me, err := f.svc.Me(ctx, jo.ID)
	if err != nil || !slices.Equal(me.Roles, []string{"admin"}) || len(me.Grants) != 1 {
		t.Fatalf("admin: %v %+v", err, me)
	}
	admin := authz.ForMember(jo.ID, false, me.AuthzGrants())
	if err := set(admin, sam.ID, "admin", "", true); !refused(err) {
		t.Errorf("an admin granting admin: %v", err)
	}
	if err := set(admin, sam.ID, "operator", "", true); err != nil {
		t.Errorf("an admin granting operator: %v", err)
	}
	if err := set(admin, jo.ID, "moderator", "", true); !refused(err) {
		t.Errorf("self-grant: %v", err)
	}

	// A game-scoped admin grants within that game only.
	scoped := authz.ForMember(sam.ID, false, []authz.Grant{{Role: "admin", GameID: "cs2"}})
	tom := uuid.New()
	f.st.Users[tom] = store.User{ID: tom, DisplayName: "Tom"}
	if err := set(scoped, tom, "moderator", "cs2", true); err != nil {
		t.Errorf("scoped admin within the game: %v", err)
	}
	if err := set(scoped, tom, "moderator", "wardogs", true); !refused(err) {
		t.Errorf("scoped admin, another game: %v", err)
	}
	if err := set(scoped, tom, "moderator", "", true); !refused(err) {
		t.Errorf("scoped admin, instance-wide: %v", err)
	}

	// An app with roles:grant grants moderator only.
	bot := authz.ForApp(uuid.New(), "htg-bot", []string{"roles:grant"})
	if err := set(bot, tom, "operator", "", true); !refused(err) {
		t.Errorf("app granting operator: %v", err)
	}
	if err := set(bot, tom, "moderator", "", true); err != nil {
		t.Errorf("app granting moderator: %v", err)
	}
	if ch := f.st.RoleChanges[len(f.st.RoleChanges)-1]; ch.ByApp == nil || ch.By != nil || ch.Role != "moderator" {
		t.Errorf("an app's change: %+v", ch)
	}
	if err := set(authz.ForMember(uuid.New(), false, nil), tom, "moderator", "", false); !refused(err) {
		t.Errorf("a member revoking: %v", err)
	}

	// No-op changes record nothing; a revocation records one.
	n := len(f.st.RoleChanges)
	if err := set(owner, tom, "moderator", "", true); err != nil || len(f.st.RoleChanges) != n {
		t.Errorf("no-op grant: %v %d", err, len(f.st.RoleChanges)-n)
	}
	if err := set(owner, tom, "moderator", "cs2", false); err != nil || len(f.st.RoleChanges) != n+1 || f.st.RoleChanges[n].GameID != "cs2" {
		t.Errorf("scoped revoke: %v %+v", err, f.st.RoleChanges[n:])
	}
	grants, err := f.svc.RoleGrants(ctx, nil)
	if err != nil || len(grants) != 3 {
		t.Errorf("grants: %v %+v", err, grants)
	}
}
