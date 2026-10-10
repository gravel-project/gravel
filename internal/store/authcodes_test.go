package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

// TestMemberTokensCodesAndGrants covers migration 14 (ADR-0015): redirect URIs on apps, codes
// spent once with a replay revoking their tokens, grants, members' tokens going with a grant, the
// app and the member's sessions, and the audit log naming a member and the app they acted through.
func TestMemberTokensCodesAndGrants(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	now := time.Now().UTC().Truncate(time.Microsecond)
	jo, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: "Jo"}, ident("discord", "1", now))
	if err != nil {
		t.Fatal(err)
	}
	app := store.App{ID: uuid.New(), OrganizationID: o.ID, Name: "Muster", ClientID: "gravel_muster", SecretHash: []byte("x"), Scopes: []string{"stats:read"},
		RedirectURIs: []string{"https://muster.example/cb", "http://127.0.0.1:8765/cb"}, CreatedAt: now}
	if err := st.CreateApp(ctx, app); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetAppByClientID(ctx, "gravel_muster")
	if err != nil || len(got.RedirectURIs) != 2 || got.RedirectURIs[1] != "http://127.0.0.1:8765/cb" {
		t.Fatalf("redirect URIs: %+v %v", got, err)
	}

	code := func(hash string) store.AuthorizationCode {
		c := store.AuthorizationCode{ID: uuid.New(), CodeHash: []byte(hash), AppID: app.ID, UserID: jo.ID, RedirectURI: "https://muster.example/cb",
			CodeChallenge: "c", Scopes: []string{"stats:read"}, CreatedAt: now, ExpiresAt: now.Add(time.Minute)}
		if err := st.CreateAuthorizationCode(ctx, c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	uid := jo.ID
	memberToken := func(hash string) func(store.AuthorizationCode) (store.AppToken, error) {
		return func(c store.AuthorizationCode) (store.AppToken, error) {
			return store.AppToken{ID: uuid.New(), AppID: c.AppID, TokenHash: []byte(hash), Scopes: c.Scopes, CreatedAt: now, ExpiresAt: now.Add(time.Hour), UserID: &uid}, nil
		}
	}
	c1 := code("code-1")
	got1, err := st.RedeemAuthorizationCode(ctx, []byte("code-1"), now, memberToken("tok-1"))
	if err != nil || got1.ID != c1.ID || got1.UsedAt == nil {
		t.Fatalf("redeem: %+v %v", got1, err)
	}
	tk, ta, err := st.GetAppTokenByHash(ctx, []byte("tok-1"), now)
	if err != nil || tk.UserID == nil || *tk.UserID != jo.ID || tk.CodeID == nil || *tk.CodeID != c1.ID || ta.ID != app.ID || len(ta.RedirectURIs) != 2 {
		t.Fatalf("member token: %+v %+v %v", tk, ta, err)
	}
	if _, err := st.RedeemAuthorizationCode(ctx, []byte("code-1"), now, memberToken("tok-1b")); !errors.Is(err, store.ErrCodeReplayed) {
		t.Errorf("replay: %v", err)
	}
	if _, _, err := st.GetAppTokenByHash(ctx, []byte("tok-1"), now); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the replayed code's token: %v", err)
	}
	if _, err := st.RedeemAuthorizationCode(ctx, []byte("nope"), now, memberToken("x")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown code: %v", err)
	}
	// A refusal from issue spends the code and stores no token.
	code("code-2")
	refused := errors.New("wrong verifier")
	if _, err := st.RedeemAuthorizationCode(ctx, []byte("code-2"), now, func(store.AuthorizationCode) (store.AppToken, error) { return store.AppToken{}, refused }); !errors.Is(err, refused) {
		t.Errorf("refused: %v", err)
	}
	if _, err := st.RedeemAuthorizationCode(ctx, []byte("code-2"), now, memberToken("tok-2")); !errors.Is(err, store.ErrCodeReplayed) {
		t.Errorf("a refused code is spent: %v", err)
	}

	// Two presentations of one code at once: one token, then none (the replay revokes it).
	code("code-3")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = st.RedeemAuthorizationCode(ctx, []byte("code-3"), now, memberToken("tok-3-"+string(rune('a'+i))))
		}()
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) || (!errors.Is(errs[0], store.ErrCodeReplayed) && !errors.Is(errs[1], store.ErrCodeReplayed)) {
		t.Errorf("concurrent redemptions: %v", errs)
	}
	for _, h := range []string{"tok-3-a", "tok-3-b"} {
		if _, _, err := st.GetAppTokenByHash(ctx, []byte(h), now); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("after a racing replay, %s: %v", h, err)
		}
	}

	// Grants.
	if _, err := st.GetAppGrant(ctx, jo.ID, app.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("no grant yet: %v", err)
	}
	if err := st.SaveAppGrant(ctx, store.AppGrant{UserID: jo.ID, AppID: app.ID, Scopes: []string{"stats:read"}, GrantedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveAppGrant(ctx, store.AppGrant{UserID: jo.ID, AppID: app.ID, Scopes: []string{"identity:read", "stats:read"}, GrantedAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	g, err := st.GetAppGrant(ctx, jo.ID, app.ID)
	if err != nil || len(g.Scopes) != 2 || !g.GrantedAt.Equal(now.Add(time.Minute)) || g.AppName != "Muster" || g.ClientID != "gravel_muster" {
		t.Errorf("grant: %+v %v", g, err)
	}
	list, err := st.ListAppGrants(ctx, jo.ID)
	if err != nil || len(list) != 1 || list[0].AppName != "Muster" {
		t.Errorf("list grants: %+v %v", list, err)
	}
	code("code-4")
	if _, err := st.RedeemAuthorizationCode(ctx, []byte("code-4"), now, memberToken("tok-4")); err != nil {
		t.Fatal(err)
	}
	pending := code("code-5")
	n, err := st.DeleteAppGrant(ctx, jo.ID, app.ID)
	if err != nil || n != 1 {
		t.Errorf("delete grant: %d %v", n, err)
	}
	if _, _, err := st.GetAppTokenByHash(ctx, []byte("tok-4"), now); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a revoked grant's token: %v", err)
	}
	if _, err := st.RedeemAuthorizationCode(ctx, pending.CodeHash, now, memberToken("tok-5")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a revoked grant's code: %v", err)
	}
	if _, err := st.DeleteAppGrant(ctx, jo.ID, app.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete twice: %v", err)
	}

	// Logging out everywhere ends what apps hold for the member, and leaves app tokens alone.
	code("code-6")
	if _, err := st.RedeemAuthorizationCode(ctx, []byte("code-6"), now, memberToken("tok-6")); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateAppToken(ctx, store.AppToken{ID: uuid.New(), AppID: app.ID, TokenHash: []byte("app-tok"), CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteUserSessions(ctx, jo.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.GetAppTokenByHash(ctx, []byte("tok-6"), now); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a member's token after logging out everywhere: %v", err)
	}
	if _, _, err := st.GetAppTokenByHash(ctx, []byte("app-tok"), now); err != nil {
		t.Errorf("the app's own token: %v", err)
	}

	// Pruning keeps codes until the cutoff.
	if n, err := st.DeleteExpiredAuthorizationCodes(ctx, now); err != nil || n != 0 {
		t.Errorf("prune before expiry: %d %v", n, err)
	}
	if n, err := st.DeleteExpiredAuthorizationCodes(ctx, now.Add(time.Minute)); err != nil || n == 0 {
		t.Errorf("prune after: %d %v", n, err)
	}

	// The audit log names a member and the app they acted through; it still needs an actor.
	if err := st.ApplyServers(ctx, o.ID, []store.Game{{ID: "wardogs", Bands: json.RawMessage(`[]`)}}, []store.ManagedServer{{ID: "wd-1", GameID: "wardogs", Name: "W",
		Driver: "wardogs", Location: "slc", Endpoint: "http://203.0.113.10:7789", CredentialFile: "/x", PollInterval: 15 * time.Second, Trust: "official"}}, now); err != nil {
		t.Fatal(err)
	}
	id, err := st.InsertAuditEntry(ctx, store.AuditEntry{OrganizationID: o.ID, At: now, ActorUserID: &jo.ID, ActorAppID: &app.ID, ServerID: "wd-1", Action: "broadcast"})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := st.ListAuditEntries(ctx, o.ID, store.AuditFilter{Limit: 10})
	if err != nil || len(entries) != 1 || entries[0].ID != id || *entries[0].ActorUserID != jo.ID || *entries[0].ActorAppID != app.ID || entries[0].ActorAppName != "Muster" {
		t.Errorf("member through an app: %+v %v", entries, err)
	}

	// Revoking the app takes its grants and codes with it.
	if err := st.SaveAppGrant(ctx, store.AppGrant{UserID: jo.ID, AppID: app.ID, Scopes: []string{"stats:read"}, GrantedAt: now}); err != nil {
		t.Fatal(err)
	}
	last := code("code-7")
	if err := st.RevokeApp(ctx, app.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetAppGrant(ctx, jo.ID, app.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a revoked app's grant: %v", err)
	}
	if _, err := st.RedeemAuthorizationCode(ctx, last.CodeHash, now, memberToken("tok-7")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a revoked app's code: %v", err)
	}
}
