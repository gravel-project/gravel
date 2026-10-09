package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

func TestAppsAndTokens(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	now := time.Now().UTC().Truncate(time.Microsecond)

	a := store.App{ID: uuid.New(), OrganizationID: o.ID, Name: "bot", ClientID: "gravel_abc", SecretHash: []byte("hash-of-secret"), Scopes: []string{"identity:read"}, CreatedAt: now}
	if err := st.CreateApp(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateApp(ctx, store.App{ID: uuid.New(), OrganizationID: o.ID, Name: "dup", ClientID: "gravel_abc", SecretHash: []byte("x"), CreatedAt: now}); err == nil {
		t.Error("a duplicate client id must be refused")
	}
	none := store.App{ID: uuid.New(), OrganizationID: o.ID, Name: "none", ClientID: "gravel_none", SecretHash: []byte("x"), CreatedAt: now.Add(time.Second)}
	if err := st.CreateApp(ctx, none); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetAppByClientID(ctx, "gravel_abc")
	if err != nil || got.ID != a.ID || got.Name != "bot" || len(got.Scopes) != 1 || got.Scopes[0] != "identity:read" || got.RevokedAt != nil || string(got.SecretHash) != "hash-of-secret" {
		t.Fatalf("get by client id: %v %+v", err, got)
	}
	if byID, err := st.GetApp(ctx, none.ID); err != nil || byID.ClientID != "gravel_none" || len(byID.Scopes) != 0 {
		t.Errorf("get by id (no scopes stored as an empty array): %v %+v", err, byID)
	}
	if _, err := st.GetAppByClientID(ctx, "gravel_unknown"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown client id: %v", err)
	}
	list, err := st.ListApps(ctx, o.ID)
	if err != nil || len(list) != 2 || list[0].ClientID != "gravel_abc" || list[1].ClientID != "gravel_none" {
		t.Errorf("list: %v %+v", err, list)
	}

	tok := store.AppToken{ID: uuid.New(), AppID: a.ID, TokenHash: []byte("tok1"), Scopes: []string{"identity:read"}, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	old := store.AppToken{ID: uuid.New(), AppID: a.ID, TokenHash: []byte("tok-old"), CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)}
	for _, tk := range []store.AppToken{tok, old} {
		if err := st.CreateAppToken(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	gt, ga, err := st.GetAppTokenByHash(ctx, []byte("tok1"), now)
	if err != nil || gt.ID != tok.ID || ga.ID != a.ID || len(gt.Scopes) != 1 || !gt.ExpiresAt.Equal(tok.ExpiresAt) {
		t.Fatalf("get token: %v %+v %+v", err, gt, ga)
	}
	if _, _, err := st.GetAppTokenByHash(ctx, []byte("tok-old"), now); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an expired token: %v", err)
	}
	if _, _, err := st.GetAppTokenByHash(ctx, []byte("tok1"), now.Add(2*time.Hour)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a token past its expiry: %v", err)
	}
	if n, err := st.DeleteExpiredAppTokens(ctx, now); err != nil || n != 1 {
		t.Errorf("prune: %d %v", n, err)
	}

	if err := st.RevokeApp(ctx, a.ID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.GetAppTokenByHash(ctx, []byte("tok1"), now); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a revoked app's token: %v", err)
	}
	if revoked, err := st.GetApp(ctx, a.ID); err != nil || revoked.RevokedAt == nil || !revoked.RevokedAt.Equal(now.Add(time.Minute)) {
		t.Errorf("revoked_at: %v %+v", err, revoked)
	}
	if err := st.RevokeApp(ctx, a.ID, now); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("revoke twice: %v", err)
	}
	if err := st.RevokeApp(ctx, uuid.New(), now); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("revoke unknown: %v", err)
	}
}

func TestListUsersAndIdentityEventsAfter(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	now := time.Now().UTC().Truncate(time.Microsecond)
	if head, err := st.IdentityEventsHead(ctx); err != nil || head != 0 {
		t.Fatalf("an empty log's head: %v %d", err, head)
	}
	var users []store.User
	for i, name := range []string{"A", "B", "C"} {
		u, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: name}, ident("discord", name, now.Add(time.Duration(i)*time.Second)))
		if err != nil {
			t.Fatal(err)
		}
		users = append(users, u)
	}
	if err := st.LinkIdentity(ctx, ident("steam", "s", now)); err == nil {
		t.Fatal("an identity without a user must fail")
	}
	steam := ident("steam", "s", now.Add(time.Minute))
	steam.UserID = users[1].ID
	if err := st.LinkIdentity(ctx, steam); err != nil {
		t.Fatal(err)
	}

	page, err := st.ListUsers(ctx, o.ID, nil, 2)
	if err != nil || len(page) != 2 {
		t.Fatalf("first page: %v %+v", err, page)
	}
	rest, err := st.ListUsers(ctx, o.ID, &store.UserCursor{CreatedAt: page[1].CreatedAt, ID: page[1].ID}, 2)
	if err != nil || len(rest) != 1 {
		t.Fatalf("second page: %v %+v", err, rest)
	}
	seen := map[uuid.UUID]bool{page[0].ID: true, page[1].ID: true, rest[0].ID: true}
	for _, u := range users {
		if !seen[u.ID] {
			t.Errorf("user %s missing from the pages", u.DisplayName)
		}
	}
	if empty, err := st.ListUsers(ctx, o.ID, &store.UserCursor{CreatedAt: rest[0].CreatedAt, ID: rest[0].ID}, 2); err != nil || len(empty) != 0 {
		t.Errorf("past the end: %v %+v", err, empty)
	}
	if other, err := st.ListUsers(ctx, uuid.New(), nil, 10); err != nil || len(other) != 0 {
		t.Errorf("another organization: %v %+v", err, other)
	}
	idents, err := st.ListIdentitiesForUsers(ctx, []uuid.UUID{users[1].ID, users[2].ID})
	if err != nil || len(idents) != 3 {
		t.Fatalf("identities for users: %v %+v", err, idents)
	}
	if none, err := st.ListIdentitiesForUsers(ctx, nil); err != nil || len(none) != 0 {
		t.Errorf("no users: %v %+v", err, none)
	}

	events, err := st.ListIdentityEventsAfter(ctx, 0, 10)
	if err != nil || len(events) != 4 || events[0].ID == 0 || events[3].Event != store.IdentityEventLinked || events[3].Provider != "steam" {
		t.Fatalf("events: %v %+v", err, events)
	}
	for i := 1; i < len(events); i++ {
		if events[i].ID <= events[i-1].ID {
			t.Errorf("ids not ascending: %+v", events)
		}
	}
	if head, err := st.IdentityEventsHead(ctx); err != nil || head != events[3].ID {
		t.Errorf("head: %v %d, want %d", err, head, events[3].ID)
	}
	if later, err := st.ListIdentityEventsAfter(ctx, events[1].ID, 1); err != nil || len(later) != 1 || later[0].ID != events[2].ID {
		t.Errorf("after the second, limit 1: %v %+v", err, later)
	}
	if none, err := st.ListIdentityEventsAfter(ctx, events[3].ID, 10); err != nil || len(none) != 0 {
		t.Errorf("after the last: %v %+v", err, none)
	}
	if mine, err := st.ListIdentityEvents(ctx, users[1].ID); err != nil || len(mine) != 2 || mine[0].ID == 0 {
		t.Errorf("per-user events carry ids: %v %+v", err, mine)
	}
}
