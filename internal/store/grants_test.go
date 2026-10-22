package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

// Migration 00025 gives every member who held ADR-0013's moderator role a moderator grant and an
// operator grant (park and unpark moved to operator), recorded as the migration's; a member
// without it gets nothing (gravel#185).
func TestRoleGrantsMigration(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	now := time.Now().UTC().Truncate(time.Microsecond)
	mod, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: "Mod"}, ident("discord", "1", now))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: "Plain"}, ident("discord", "2", now))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MigrateDownTo(ctx, 24); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, storetest.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, `UPDATE users SET roles = ARRAY['moderator'] WHERE id = $1`, mod.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetUser(ctx, mod.ID)
	if err != nil || strings.Join(got.Roles, ",") != "moderator,operator" {
		t.Fatalf("the moderator after the migration: %v %v", got.Roles, err)
	}
	if p, _ := st.GetUser(ctx, plain.ID); len(p.Roles) != 0 {
		t.Errorf("a plain member got %v", p.Roles)
	}
	grants, err := st.RoleGrants(ctx, nil)
	if err != nil || len(grants) != 2 || grants[0].GrantedBy != nil || grants[0].GameID != "" {
		t.Errorf("grants: %+v %v", grants, err)
	}
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM role_changes WHERE user_id = $1 AND role = 'operator' AND by_user_id IS NULL`, mod.ID).Scan(&n); err != nil || n != 1 {
		t.Errorf("the operator grant recorded: %d %v", n, err)
	}
	if has, err := usersHasRoles(ctx, conn); err != nil || has {
		t.Errorf("users.roles at head: %t %v", has, err)
	}
}

// Migration 00039 drops users.roles (gravel#199); going down puts it back holding moderator for
// every instance-wide moderator grant, the one role ADR-0013's column could hold, and up again
// leaves the grants as they were.
func TestDropUsersRolesMigration(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := st.ApplyServers(ctx, o.ID, []store.Game{{ID: "wardogs"}}, nil, nil, now); err != nil {
		t.Fatal(err)
	}
	reg := func(name, subject string, grants ...[2]string) store.User {
		u, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: name}, ident("discord", subject, now))
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range grants {
			if _, err := st.SetRoleGrant(ctx, u.ID, g[0], g[1], true, store.RoleChange{At: now}); err != nil {
				t.Fatal(err)
			}
		}
		return u
	}
	mod := reg("Mod", "1", [2]string{store.RoleModerator, ""}, [2]string{store.RoleOperator, ""})
	gameMod := reg("Game mod", "2", [2]string{store.RoleModerator, "wardogs"})
	admin := reg("Admin", "3", [2]string{store.RoleAdmin, ""})
	plain := reg("Plain", "4")
	conn, err := pgx.Connect(ctx, storetest.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if has, err := usersHasRoles(ctx, conn); err != nil || has {
		t.Fatalf("users.roles at head: %t %v", has, err)
	}

	if err := st.MigrateDownTo(ctx, 38); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		who  store.User
		want string
	}{{mod, "moderator"}, {gameMod, ""}, {admin, ""}, {plain, ""}} {
		var roles []string
		if err := conn.QueryRow(ctx, `SELECT roles FROM users WHERE id = $1`, c.who.ID).Scan(&roles); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(roles, ","); got != c.want {
			t.Errorf("%s's users.roles at 00038: %q, want %q", c.who.DisplayName, got, c.want)
		}
	}

	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if has, err := usersHasRoles(ctx, conn); err != nil || has {
		t.Errorf("users.roles after going up again: %t %v", has, err)
	}
	if got, err := st.GetUser(ctx, mod.ID); err != nil || strings.Join(got.Roles, ",") != "moderator,operator" {
		t.Errorf("the moderator's roles after the round trip: %v %v", got.Roles, err)
	}
	if grants, err := st.RoleGrants(ctx, nil); err != nil || len(grants) != 4 {
		t.Errorf("grants after the round trip: %+v %v", grants, err)
	}
}

func usersHasRoles(ctx context.Context, conn *pgx.Conn) (bool, error) {
	var has bool
	err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'users' AND column_name = 'roles')`).Scan(&has)
	return has, err
}
