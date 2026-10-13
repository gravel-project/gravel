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
}
