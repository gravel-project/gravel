package apps_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/apps"
	"github.com/gravel-project/gravel/internal/apps/appstest"
	"github.com/gravel-project/gravel/internal/store"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newService(t *testing.T, now *time.Time) (*apps.Service, *appstest.FakeStore) {
	t.Helper()
	st := appstest.NewFakeStore()
	svc := apps.New(st, uuid.New(), time.Hour, quiet(), apps.WithClock(func() time.Time { return *now }))
	return svc, st
}

func TestCreate(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	svc, st := newService(t, &now)
	ctx := context.Background()
	c, err := svc.Create(ctx, "  htg-bot ", []string{"identity:read", "identity:read", " "})
	if err != nil {
		t.Fatal(err)
	}
	if c.App.Name != "htg-bot" || !strings.HasPrefix(c.App.ClientID, "gravel_") || len(c.App.ClientID) != len("gravel_")+24 {
		t.Errorf("app: %+v", c.App)
	}
	if len(c.Secret) < 40 || strings.ContainsAny(c.Secret, "+/=") {
		t.Errorf("secret shape: %q", c.Secret)
	}
	if got := c.App.Scopes; len(got) != 1 || got[0] != apps.ScopeIdentityRead {
		t.Errorf("scopes: %v", got)
	}
	if string(st.Apps[c.App.ID].SecretHash) == c.Secret || len(st.Apps[c.App.ID].SecretHash) != 32 {
		t.Error("the secret must be stored as a 32-byte hash")
	}
	for _, bad := range []struct {
		name   string
		scopes []string
		want   error
	}{
		{"", []string{"identity:read"}, apps.ErrInvalidName},
		{strings.Repeat("x", apps.MaxNameLength+1), []string{"identity:read"}, apps.ErrInvalidName},
		{"ok", []string{"admin"}, apps.ErrInvalidScope},
	} {
		if _, err := svc.Create(ctx, bad.name, bad.scopes); !errors.Is(err, bad.want) {
			t.Errorf("Create(%q, %v): %v, want %v", bad.name, bad.scopes, err, bad.want)
		}
	}
	if _, err := svc.Create(ctx, "none", nil); err != nil {
		t.Errorf("an app with no scopes is allowed (useless, but valid): %v", err)
	}
}

func TestIssueAndAuthenticate(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	svc, st := newService(t, &now)
	ctx := context.Background()
	c, err := svc.Create(ctx, "bot", []string{"identity:read"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := svc.Issue(ctx, c.App.ClientID, c.Secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Value == "" || !tok.ExpiresAt.Equal(now.Add(time.Hour)) || len(tok.Scopes) != 1 {
		t.Errorf("token: %+v", tok)
	}
	a, err := svc.Authenticate(ctx, tok.Value)
	if err != nil || a.ID != c.App.ID || a.ClientID != c.App.ClientID || a.Name != "bot" || !a.Has("identity:read") || a.Has("other") {
		t.Errorf("Authenticate: %+v %v", a, err)
	}
	for _, bad := range []struct {
		id, secret string
		scopes     []string
		want       error
	}{
		{"gravel_unknown", c.Secret, nil, apps.ErrInvalidClient},
		{c.App.ClientID, "wrong", nil, apps.ErrInvalidClient},
		{c.App.ClientID, "", nil, apps.ErrInvalidClient},
		{c.App.ClientID, c.Secret, []string{"nope"}, apps.ErrInvalidScope},
	} {
		if _, err := svc.Issue(ctx, bad.id, bad.secret, bad.scopes); !errors.Is(err, bad.want) {
			t.Errorf("Issue(%q, %q, %v): %v, want %v", bad.id, bad.secret, bad.scopes, err, bad.want)
		}
	}
	// A subset of the app's scopes is fine; a scope the app lacks is not, even though it exists.
	now = now.Add(time.Second) // registered after "bot", so List's order is fixed
	narrow, err := svc.Create(ctx, "narrow", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Issue(ctx, narrow.App.ClientID, narrow.Secret, []string{"identity:read"}); !errors.Is(err, apps.ErrInvalidScope) {
		t.Errorf("a scope not granted to the app: %v", err)
	}
	if _, err := svc.Authenticate(ctx, "not-a-token"); !errors.Is(err, apps.ErrInvalidToken) {
		t.Errorf("unknown token: %v", err)
	}
	if _, err := svc.Authenticate(ctx, ""); !errors.Is(err, apps.ErrInvalidToken) {
		t.Errorf("empty token: %v", err)
	}
	// Expiry, then prune.
	now = now.Add(time.Hour + time.Second)
	if _, err := svc.Authenticate(ctx, tok.Value); !errors.Is(err, apps.ErrInvalidToken) {
		t.Errorf("expired token: %v", err)
	}
	if n, err := svc.Prune(ctx); err != nil || n != 1 || st.TokenCount() != 0 {
		t.Errorf("Prune: %d %v (%d left)", n, err, st.TokenCount())
	}
	// Revocation ends the tokens and refuses new ones.
	tok2, err := svc.Issue(ctx, c.App.ClientID, c.Secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Revoke(ctx, c.App.ClientID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, tok2.Value); !errors.Is(err, apps.ErrInvalidToken) {
		t.Errorf("token of a revoked app: %v", err)
	}
	if _, err := svc.Issue(ctx, c.App.ClientID, c.Secret, nil); !errors.Is(err, apps.ErrInvalidClient) {
		t.Errorf("issue for a revoked app: %v", err)
	}
	if err := svc.Revoke(ctx, c.App.ClientID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("revoke twice: %v", err)
	}
	if err := svc.Revoke(ctx, "gravel_unknown"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("revoke unknown: %v", err)
	}
	list, err := svc.List(ctx)
	if err != nil || len(list) != 2 || list[0].RevokedAt == nil || list[1].RevokedAt != nil {
		t.Errorf("List: %+v %v", list, err)
	}
	st.ErrOn = "GetAppTokenByHash"
	if _, err := svc.Authenticate(ctx, tok2.Value); err == nil || errors.Is(err, apps.ErrInvalidToken) {
		t.Errorf("a store failure is not an invalid token: %v", err)
	}
}

func TestContext(t *testing.T) {
	a := apps.App{ID: uuid.New(), ClientID: "gravel_x", Name: "x", Scopes: []string{"identity:read"}}
	ctx := apps.NewContext(context.Background(), a)
	got, ok := apps.FromContext(ctx)
	if !ok || got.ClientID != "gravel_x" {
		t.Errorf("FromContext: %+v %v", got, ok)
	}
	if _, ok := apps.FromContext(context.Background()); ok {
		t.Error("an empty context carries no app")
	}
}

type tokenReply struct {
	status int
	header http.Header
	body   map[string]any
}

func post(t *testing.T, h http.Handler, form url.Values, basic [2]string) tokenReply {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if basic[0] != "" {
		req.SetBasicAuth(url.QueryEscape(basic[0]), url.QueryEscape(basic[1]))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return tokenReply{status: rec.Code, header: rec.Header(), body: body}
}

func TestTokenHandler(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	svc, _ := newService(t, &now)
	ctx := context.Background()
	c, err := svc.Create(ctx, "bot", []string{"identity:read"})
	if err != nil {
		t.Fatal(err)
	}
	h := svc.TokenHandler()
	grant := url.Values{"grant_type": {"client_credentials"}}

	r := post(t, h, grant, [2]string{c.App.ClientID, c.Secret})
	if r.status != 200 || r.body["token_type"] != "Bearer" || r.body["scope"] != "identity:read" || r.body["expires_in"] != float64(3600) || r.body["access_token"] == "" {
		t.Errorf("basic auth: %d %v", r.status, r.body)
	}
	if r.header.Get("Cache-Control") != "no-store" || r.header.Get("Pragma") != "no-cache" || !strings.HasPrefix(r.header.Get("Content-Type"), "application/json") {
		t.Errorf("headers: %v", r.header)
	}
	if _, err := svc.Authenticate(ctx, r.body["access_token"].(string)); err != nil {
		t.Errorf("the issued token authenticates: %v", err)
	}

	body := url.Values{"grant_type": {"client_credentials"}, "client_id": {c.App.ClientID}, "client_secret": {c.Secret}, "scope": {"identity:read"}}
	if r := post(t, h, body, [2]string{}); r.status != 200 {
		t.Errorf("body credentials: %d %v", r.status, r.body)
	}
	twice := url.Values{"grant_type": {"client_credentials"}, "client_id": {"gravel_other"}}
	if r := post(t, h, twice, [2]string{c.App.ClientID, c.Secret}); r.status != 400 || r.body["error"] != "invalid_request" {
		t.Errorf("credentials twice: %d %v", r.status, r.body)
	}
	if r := post(t, h, grant, [2]string{}); r.status != 401 || r.body["error"] != "invalid_client" || !strings.HasPrefix(r.header.Get("WWW-Authenticate"), "Basic") {
		t.Errorf("no credentials: %d %v %q", r.status, r.body, r.header.Get("WWW-Authenticate"))
	}
	if r := post(t, h, grant, [2]string{c.App.ClientID, "wrong"}); r.status != 401 || r.body["error"] != "invalid_client" {
		t.Errorf("wrong secret: %d %v", r.status, r.body)
	}
	if r := post(t, h, url.Values{"grant_type": {"password"}}, [2]string{c.App.ClientID, c.Secret}); r.status != 400 || r.body["error"] != "unsupported_grant_type" {
		t.Errorf("wrong grant: %d %v", r.status, r.body)
	}
	if r := post(t, h, url.Values{}, [2]string{c.App.ClientID, c.Secret}); r.status != 400 || r.body["error"] != "invalid_request" {
		t.Errorf("no grant: %d %v", r.status, r.body)
	}
	if r := post(t, h, url.Values{"grant_type": {"client_credentials"}, "scope": {"identity:read admin"}}, [2]string{c.App.ClientID, c.Secret}); r.status != 400 || r.body["error"] != "invalid_scope" {
		t.Errorf("bad scope: %d %v", r.status, r.body)
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/oauth/token", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 405 || rec.Header().Get("Allow") != "POST" {
		t.Errorf("GET: %d %v", rec.Code, rec.Header())
	}
}

func TestMiddleware(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	svc, st := newService(t, &now)
	ctx := context.Background()
	c, err := svc.Create(ctx, "bot", []string{"identity:read"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := svc.Issue(ctx, c.App.ClientID, c.Secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	var seen *apps.App
	h := svc.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = nil
		if a, ok := apps.FromContext(r.Context()); ok {
			seen = &a
		}
		w.WriteHeader(204)
	}))
	call := func(auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := call(""); rec.Code != 204 || seen != nil {
		t.Errorf("no header: %d %v", rec.Code, seen)
	}
	if rec := call("Basic abc"); rec.Code != 204 || seen != nil {
		t.Errorf("another scheme passes through: %d %v", rec.Code, seen)
	}
	if rec := call("bearer " + tok.Value); rec.Code != 204 || seen == nil || seen.ClientID != c.App.ClientID {
		t.Errorf("valid token (any case): %d %v", rec.Code, seen)
	}
	rec := call("Bearer nope")
	if rec.Code != 401 || !strings.Contains(rec.Header().Get("WWW-Authenticate"), `error="invalid_token"`) || !strings.Contains(rec.Body.String(), "invalid_token") {
		t.Errorf("invalid token: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}
	st.ErrOn = "GetAppTokenByHash"
	if rec := call("Bearer " + tok.Value); rec.Code != 503 {
		t.Errorf("store down: %d", rec.Code)
	}
}
