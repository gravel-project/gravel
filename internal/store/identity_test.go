package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

func newOrg(t *testing.T, st *store.Store) store.Organization {
	t.Helper()
	o, err := st.CreateBuiltinOrganization(context.Background(), uuid.New(), "HTG")
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func ident(provider, subject string, at time.Time) store.Identity {
	return store.Identity{Provider: provider, Subject: subject, DisplayName: subject + "-name", AvatarURL: "https://a/" + subject, VerificationMethod: "oauth2", VerifiedAt: at}
}

func TestIdentityLifecycle(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	now := time.Now().UTC().Truncate(time.Microsecond)

	u, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: "Jo"}, ident("discord", "1", now))
	if err != nil || u.DisplayName != "Jo" || u.LastLoginAt == nil || !u.LastLoginAt.Equal(now) {
		t.Fatalf("register: %v %+v", err, u)
	}
	got, err := st.GetUser(ctx, u.ID)
	if err != nil || got.ID != u.ID || got.OrganizationID != o.ID {
		t.Fatalf("get user: %v %+v", err, got)
	}
	if _, err := st.GetUser(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown user: %v", err)
	}

	// The pair is unique across users: a second registration with it fails and leaves no user.
	if _, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: "Dup"}, ident("discord", "1", now)); !errors.Is(err, store.ErrIdentityExists) {
		t.Errorf("duplicate registration: %v", err)
	}
	if err := st.LinkIdentity(ctx, store.Identity{UserID: u.ID, Provider: "discord", Subject: "1", VerificationMethod: "oauth2", VerifiedAt: now}); !errors.Is(err, store.ErrIdentityExists) {
		t.Errorf("duplicate link: %v", err)
	}

	i, err := st.GetIdentity(ctx, "discord", "1")
	if err != nil || i.UserID != u.ID || i.DisplayName != "1-name" || i.LastLoginAt == nil {
		t.Fatalf("get identity: %v %+v", err, i)
	}
	if _, err := st.GetIdentity(ctx, "discord", "2"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown identity: %v", err)
	}

	later := now.Add(time.Minute)
	s := ident("steam", "7656", later)
	s.UserID = u.ID
	if err := st.LinkIdentity(ctx, s); err != nil {
		t.Fatal(err)
	}
	list, err := st.ListIdentities(ctx, u.ID)
	if err != nil || len(list) != 2 || list[0].Provider != "discord" || list[1].Provider != "steam" || list[1].LastLoginAt != nil {
		t.Fatalf("list: %v %+v", err, list)
	}

	logged, err := st.RecordLogin(ctx, "discord", "1", "Jo!", "https://a/new", later)
	if err != nil || logged.DisplayName != "Jo!" || logged.AvatarURL != "https://a/new" || !logged.LastLoginAt.Equal(later) {
		t.Fatalf("record login: %v %+v", err, logged)
	}
	if got, _ := st.GetUser(ctx, u.ID); !got.LastLoginAt.Equal(later) {
		t.Errorf("user last login not stamped: %+v", got)
	}
	if _, err := st.RecordLogin(ctx, "discord", "nope", "", "", later); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("record login unknown: %v", err)
	}
	refreshed, err := st.RefreshIdentity(ctx, "steam", "7656", "Persona", "https://a/p")
	if err != nil || refreshed.DisplayName != "Persona" || refreshed.LastLoginAt != nil {
		t.Errorf("refresh: %v %+v", err, refreshed)
	}
	if _, err := st.RefreshIdentity(ctx, "steam", "nope", "", ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("refresh unknown: %v", err)
	}

	// Unlink: not mine, then one of two, then the last one is refused.
	other, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: "Other"}, ident("discord", "2", now))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UnlinkIdentity(ctx, other.ID, "steam", "7656", later); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unlink someone else's: %v", err)
	}
	if err := st.UnlinkIdentity(ctx, u.ID, "steam", "7656", later); err != nil {
		t.Errorf("unlink: %v", err)
	}
	if err := st.UnlinkIdentity(ctx, u.ID, "discord", "1", later); !errors.Is(err, store.ErrLastIdentity) {
		t.Errorf("unlink last: %v", err)
	}
	if list, _ := st.ListIdentities(ctx, u.ID); len(list) != 1 {
		t.Errorf("after unlink: %+v", list)
	}

	events, err := st.ListIdentityEvents(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Provider+":"+e.Event)
	}
	want := []string{"discord:registered", "steam:linked", "discord:login", "steam:unlinked"}
	if len(kinds) != len(want) {
		t.Fatalf("events: %v", kinds)
	}
	for k := range want {
		if kinds[k] != want[k] {
			t.Errorf("event %d: got %s want %s", k, kinds[k], want[k])
		}
	}
}

func TestSessionsAndAttempts(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	now := time.Now().UTC().Truncate(time.Microsecond)
	u, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: "Jo"}, ident("discord", "1", now))
	if err != nil {
		t.Fatal(err)
	}

	hash := []byte("0123456789abcdef0123456789abcdef")
	sess := store.Session{ID: uuid.New(), UserID: u.ID, TokenHash: hash, CSRFToken: "c", CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now}
	if err := st.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, store.Session{ID: uuid.New(), UserID: u.ID, TokenHash: hash, CSRFToken: "c", CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now}); err == nil {
		t.Error("token hashes must be unique")
	}
	got, err := st.GetSessionByTokenHash(ctx, hash, now)
	if err != nil || got.ID != sess.ID || got.UserID != u.ID || got.CSRFToken != "c" {
		t.Fatalf("get session: %v %+v", err, got)
	}
	if _, err := st.GetSessionByTokenHash(ctx, hash, now.Add(2*time.Hour)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expired session must not load: %v", err)
	}
	if _, err := st.GetSessionByTokenHash(ctx, []byte("other"), now); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown session: %v", err)
	}
	if err := st.TouchSession(ctx, sess.ID, now.Add(time.Minute)); err != nil {
		t.Error(err)
	}
	if got, _ := st.GetSessionByTokenHash(ctx, hash, now); !got.LastSeenAt.Equal(now.Add(time.Minute)) {
		t.Errorf("touch: %+v", got)
	}
	second := store.Session{ID: uuid.New(), UserID: u.ID, TokenHash: []byte("second"), CSRFToken: "d", CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now}
	if err := st.CreateSession(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSession(ctx, sess.ID); err != nil {
		t.Error(err)
	}
	if err := st.DeleteSession(ctx, sess.ID); err != nil {
		t.Errorf("deleting twice is fine: %v", err)
	}
	if n, err := st.DeleteUserSessions(ctx, u.ID); err != nil || n != 1 {
		t.Errorf("delete user sessions: %v %d", err, n)
	}
	if err := st.CreateSession(ctx, store.Session{ID: uuid.New(), UserID: u.ID, TokenHash: []byte("old"), CSRFToken: "e", CreatedAt: now, ExpiresAt: now.Add(-time.Second), LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	if n, err := st.DeleteExpiredSessions(ctx, now); err != nil || n != 1 {
		t.Errorf("prune sessions: %v %d", err, n)
	}

	a := store.AuthAttempt{ID: uuid.New(), TokenHash: []byte("attempt"), Provider: "discord", Intent: "login", State: "s", ProviderSession: "{}", CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute)}
	if err := st.CreateAuthAttempt(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateAuthAttempt(ctx, store.AuthAttempt{ID: uuid.New(), TokenHash: []byte("bad"), Provider: "steam", Intent: "link", State: "s", ProviderSession: "{}", CreatedAt: now, ExpiresAt: now.Add(time.Minute)}); err == nil {
		t.Error("a link attempt without a user must be refused by the schema")
	}
	roles := store.AuthAttempt{ID: uuid.New(), TokenHash: []byte("roles"), Provider: "discord", Intent: "roles", State: "sr", ProviderSession: "{}", CreatedAt: now, ExpiresAt: now.Add(time.Minute)}
	if err := st.CreateAuthAttempt(ctx, roles); err != nil {
		t.Fatalf("a Linked Roles attempt (migration 5): %v", err)
	}
	if got, err := st.ConsumeAuthAttempt(ctx, []byte("roles"), now); err != nil || got.Intent != "roles" {
		t.Errorf("consume roles: %v %+v", err, got)
	}
	if err := st.CreateAuthAttempt(ctx, store.AuthAttempt{ID: uuid.New(), TokenHash: []byte("odd"), Provider: "discord", Intent: "sudo", State: "s", ProviderSession: "{}", CreatedAt: now, ExpiresAt: now.Add(time.Minute)}); err == nil {
		t.Error("an unknown intent must still be refused by the schema")
	}
	consumed, err := st.ConsumeAuthAttempt(ctx, []byte("attempt"), now)
	if err != nil || consumed.ID != a.ID || consumed.State != "s" || consumed.UserID != nil {
		t.Fatalf("consume: %v %+v", err, consumed)
	}
	if _, err := st.ConsumeAuthAttempt(ctx, []byte("attempt"), now); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an attempt is consumed once: %v", err)
	}
	link := store.AuthAttempt{ID: uuid.New(), TokenHash: []byte("link"), Provider: "steam", Intent: "link", UserID: &u.ID, State: "s2", ProviderSession: "{}", CreatedAt: now, ExpiresAt: now.Add(time.Minute)}
	if err := st.CreateAuthAttempt(ctx, link); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ConsumeAuthAttempt(ctx, []byte("link"), now.Add(2*time.Minute)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an expired attempt must not complete: %v", err)
	}
	if _, err := st.ConsumeAuthAttempt(ctx, []byte("link"), now); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an expired attempt is deleted when consumed: %v", err)
	}
	if err := st.CreateAuthAttempt(ctx, store.AuthAttempt{ID: uuid.New(), TokenHash: []byte("stale"), Provider: "discord", Intent: "login", State: "s3", ProviderSession: "{}", CreatedAt: now, ExpiresAt: now.Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	if n, err := st.DeleteExpiredAuthAttempts(ctx, now); err != nil || n != 1 {
		t.Errorf("prune attempts: %v %d", err, n)
	}
}

func TestClaimBindsOwnerAndReopensOwnerless(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	now := time.Now().UTC().Truncate(time.Microsecond)
	u, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: "Jo"}, ident("discord", "1", now))
	if err != nil {
		t.Fatal(err)
	}
	hash := []byte("0123456789abcdef0123456789abcdef")
	if err := st.SetClaimToken(ctx, o.ID, hash, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOrganization(ctx, o.ID, hash, uuid.New(), now); err == nil {
		t.Error("the owner must be an existing user")
	}
	claimed, err := st.ClaimOrganization(ctx, o.ID, hash, u.ID, now)
	if err != nil || claimed.OwnerUserID == nil || *claimed.OwnerUserID != u.ID || !claimed.Owned() {
		t.Fatalf("claim: %v %+v", err, claimed)
	}

	// The 0.1.0 shape (claimed, no owner) is what the migration reopens.
	if err := st.MigrateDownTo(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetBuiltinOrganization(ctx); got.Owned() {
		t.Errorf("an ownerless claim must be reopened by the migration: %+v", got)
	}
}

func TestStats(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	now := time.Now().UTC().Truncate(time.Microsecond)

	got, err := st.Stats(ctx)
	if err != nil || got.Users != 0 || len(got.Identities) != 0 || got.DatabaseBytes <= 0 {
		t.Fatalf("empty: %v %+v", err, got)
	}
	jo, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: "Jo"}, ident("discord", "1", now))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: "Sam"}, ident("discord", "2", now)); err != nil {
		t.Fatal(err)
	}
	steam := ident("steam", "7656", now)
	steam.UserID = jo.ID
	if err := st.LinkIdentity(ctx, steam); err != nil {
		t.Fatal(err)
	}
	got, err = st.Stats(ctx)
	if err != nil || got.Users != 2 || got.Identities["discord"] != 2 || got.Identities["steam"] != 1 || len(got.Identities) != 2 {
		t.Fatalf("after register and link: %v %+v", err, got)
	}
	if err := st.UnlinkIdentity(ctx, jo.ID, "steam", "7656", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, err = st.Stats(ctx)
	if err != nil || got.Users != 2 || got.Identities["steam"] != 0 || len(got.Identities) != 1 {
		t.Fatalf("after unlink: %v %+v", err, got)
	}

	// A context that is already done fails rather than reporting stale numbers.
	done, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := st.Stats(done); err == nil {
		t.Error("stats with a cancelled context: want error")
	}
}

// A role is granted and revoked with a record of each change; the column takes only known roles,
// and role_changes is append-only (ADR-0013).
func TestSetUserRole(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	now := time.Now().UTC().Truncate(time.Microsecond)
	reg := func(name, subject string) store.User {
		u, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: name},
			store.Identity{Provider: "discord", Subject: subject, VerificationMethod: "oauth2", VerifiedAt: now})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	owner, sam := reg("Owner", "1"), reg("Sam", "2")
	if sam.Roles != nil || sam.HasRole(store.RoleModerator) {
		t.Fatalf("a new member has roles: %v", sam.Roles)
	}
	u, err := st.SetUserRole(ctx, sam.ID, store.RoleModerator, true, &owner.ID, now)
	if err != nil || !u.HasRole(store.RoleModerator) {
		t.Fatalf("grant: %+v %v", u, err)
	}
	if u, err = st.SetUserRole(ctx, sam.ID, store.RoleModerator, true, &owner.ID, now); err != nil || len(u.Roles) != 1 {
		t.Errorf("a repeated grant: %+v %v", u.Roles, err)
	}
	if got, _ := st.GetUser(ctx, sam.ID); !got.HasRole(store.RoleModerator) {
		t.Error("GetUser lost the role")
	}
	if u, err = st.SetUserRole(ctx, sam.ID, store.RoleModerator, false, &owner.ID, now.Add(time.Minute)); err != nil || u.Roles != nil {
		t.Errorf("revoke: %+v %v", u.Roles, err)
	}
	conn, err := pgx.Connect(ctx, storetest.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM role_changes WHERE user_id = $1 AND by_user_id = $2`, sam.ID, owner.ID).Scan(&n); err != nil || n != 2 {
		t.Errorf("role changes recorded: %d %v (the repeated grant records nothing)", n, err)
	}
	if _, err := st.SetUserRole(ctx, sam.ID, "admin", true, &owner.ID, now); err == nil {
		t.Error("an unknown role was stored")
	}
	for _, q := range []string{`DELETE FROM role_changes`, `UPDATE role_changes SET granted = NOT granted`} {
		if _, err := conn.Exec(ctx, q); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: %v", q, err)
		}
	}
}
