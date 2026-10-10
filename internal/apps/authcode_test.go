package apps_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
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

const (
	redirect = "https://muster.example/auth/callback"
	verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk-and-some-more"
)

func challenge(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// codeRig is a service with Muster registered for the code flow, and a member.
type codeRig struct {
	svc    *apps.Service
	st     *appstest.FakeStore
	now    *time.Time
	muster apps.Created
	member uuid.UUID
}

func newCodeRig(t *testing.T) *codeRig {
	t.Helper()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	svc, st := newService(t, &now)
	m, err := svc.Create(context.Background(), "Muster", []string{apps.ScopeIdentityRead, apps.ScopeStatsRead, apps.ScopeServersConfigure}, redirect, "http://127.0.0.1:8765/cb")
	if err != nil {
		t.Fatal(err)
	}
	return &codeRig{svc: svc, st: st, now: &now, muster: m, member: uuid.New()}
}

func (r *codeRig) request(scope string) apps.AuthorizationRequest {
	return apps.AuthorizationRequest{ResponseType: "code", ClientID: r.muster.App.ClientID, RedirectURI: redirect, Scope: scope,
		CodeChallenge: challenge(verifier), CodeChallengeMethod: "S256"}
}

func (r *codeRig) code(t *testing.T, scope string) string {
	t.Helper()
	code, _, err := r.svc.Authorize(context.Background(), r.request(scope), r.member)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func TestValidateRedirectURI(t *testing.T) {
	for _, ok := range []string{"https://muster.example/cb", "https://muster.example:8443/cb?x=1", "http://127.0.0.1:8765/cb", "http://localhost/cb", "http://[::1]:9000/cb"} {
		if err := apps.ValidateRedirectURI(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "http://muster.example/cb", "ftp://muster.example/cb", "https://muster.example/cb#frag", "https://user:pw@muster.example/cb",
		"https:///cb", "/cb", "javascript:alert(1)", "https://muster.example/c b", `https://muster.example\cb`, "http://127.0.0.2/cb", "http://localhost.evil.example/cb",
		"https://muster.example/" + strings.Repeat("x", apps.MaxRedirectURILength)} {
		if err := apps.ValidateRedirectURI(bad); !errors.Is(err, apps.ErrInvalidRedirectURI) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	now := time.Now()
	svc, _ := newService(t, &now)
	if _, err := svc.Create(context.Background(), "bad", nil, "http://muster.example/cb"); !errors.Is(err, apps.ErrInvalidRedirectURI) {
		t.Errorf("Create with a plain-http redirect: %v", err)
	}
}

// A bad client or redirect URI is never redirected to; every other refusal is the app's to hear.
func TestCheckAuthorizationRefusals(t *testing.T) {
	r := newCodeRig(t)
	ctx := context.Background()
	revoked, err := r.svc.Create(ctx, "Old", nil, redirect)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.svc.Revoke(ctx, revoked.App.ClientID); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		edit func(*apps.AuthorizationRequest)
		want error
	}{
		"unknown client":                  {func(q *apps.AuthorizationRequest) { q.ClientID = "gravel_nope" }, apps.ErrInvalidClient},
		"no client":                       {func(q *apps.AuthorizationRequest) { q.ClientID = "" }, apps.ErrInvalidClient},
		"revoked app":                     {func(q *apps.AuthorizationRequest) { q.ClientID = revoked.App.ClientID }, apps.ErrInvalidClient},
		"no redirect_uri":                 {func(q *apps.AuthorizationRequest) { q.RedirectURI = "" }, apps.ErrInvalidRedirectURI},
		"redirect with a trailing slash":  {func(q *apps.AuthorizationRequest) { q.RedirectURI = redirect + "/" }, apps.ErrInvalidRedirectURI},
		"redirect on another port":        {func(q *apps.AuthorizationRequest) { q.RedirectURI = "https://muster.example:444/auth/callback" }, apps.ErrInvalidRedirectURI},
		"redirect over http":              {func(q *apps.AuthorizationRequest) { q.RedirectURI = "http://muster.example/auth/callback" }, apps.ErrInvalidRedirectURI},
		"redirect with an extra query":    {func(q *apps.AuthorizationRequest) { q.RedirectURI = redirect + "?next=/" }, apps.ErrInvalidRedirectURI},
		"redirect with another case":      {func(q *apps.AuthorizationRequest) { q.RedirectURI = "https://Muster.example/auth/callback" }, apps.ErrInvalidRedirectURI},
		"redirect as a prefix":            {func(q *apps.AuthorizationRequest) { q.RedirectURI = redirect + "/../../evil" }, apps.ErrInvalidRedirectURI},
		"loopback redirect on a new port": {func(q *apps.AuthorizationRequest) { q.RedirectURI = "http://127.0.0.1:8766/cb" }, apps.ErrInvalidRedirectURI},
	} {
		q := r.request("")
		tc.edit(&q)
		if _, err := r.svc.CheckAuthorization(ctx, q, r.member); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	for name, tc := range map[string]struct {
		edit func(*apps.AuthorizationRequest)
		code string
	}{
		"no response_type":       {func(q *apps.AuthorizationRequest) { q.ResponseType = "" }, "invalid_request"},
		"the implicit grant":     {func(q *apps.AuthorizationRequest) { q.ResponseType = "token" }, "unsupported_response_type"},
		"missing PKCE":           {func(q *apps.AuthorizationRequest) { q.CodeChallenge, q.CodeChallengeMethod = "", "" }, "invalid_request"},
		"plain PKCE":             {func(q *apps.AuthorizationRequest) { q.CodeChallenge, q.CodeChallengeMethod = verifier, "plain" }, "invalid_request"},
		"no method (plain)":      {func(q *apps.AuthorizationRequest) { q.CodeChallengeMethod = "" }, "invalid_request"},
		"a malformed challenge":  {func(q *apps.AuthorizationRequest) { q.CodeChallenge = "short" }, "invalid_request"},
		"a scope the app lacks":  {func(q *apps.AuthorizationRequest) { q.Scope = apps.ScopeServersModerate }, "invalid_scope"},
		"a scope that is no one": {func(q *apps.AuthorizationRequest) { q.Scope = "admin" }, "invalid_scope"},
	} {
		q := r.request("")
		tc.edit(&q)
		auth, err := r.svc.CheckAuthorization(ctx, q, r.member)
		var refusal *apps.AuthorizationError
		if !errors.As(err, &refusal) || refusal.Code != tc.code || auth.App.ClientID != r.muster.App.ClientID {
			t.Errorf("%s: %v (%+v), want %s naming the app", name, err, refusal, tc.code)
		}
		if _, _, err := r.svc.Authorize(ctx, q, r.member); !errors.As(err, &refusal) {
			t.Errorf("%s: Authorize issued anyway: %v", name, err)
		}
	}
	if len(r.st.Codes) != 0 || len(r.st.Grants) != 0 {
		t.Errorf("refusals stored %d codes and %d grants", len(r.st.Codes), len(r.st.Grants))
	}
}

// Consent is remembered per member and app, and asked again only for more.
func TestConsentRememberedThenAskedForMore(t *testing.T) {
	r := newCodeRig(t)
	ctx := context.Background()
	auth, err := r.svc.CheckAuthorization(ctx, r.request(apps.ScopeStatsRead), r.member)
	if err != nil || !auth.ConsentRequired || len(auth.Scopes) != 1 {
		t.Fatalf("first time: %+v %v", auth, err)
	}
	if auth, _ := r.svc.CheckAuthorization(ctx, r.request(""), uuid.Nil); !auth.ConsentRequired {
		t.Error("nobody logged in: consent is required")
	}
	r.code(t, apps.ScopeStatsRead)
	for _, scope := range []string{apps.ScopeStatsRead, "", apps.ScopeStatsRead + " " + apps.ScopeStatsRead} {
		if auth, err := r.svc.CheckAuthorization(ctx, r.request(scope), r.member); err != nil || auth.ConsentRequired {
			t.Errorf("covered by the grant (%q): %+v %v", scope, auth, err)
		}
	}
	if auth, _ := r.svc.CheckAuthorization(ctx, r.request(apps.ScopeStatsRead), uuid.New()); !auth.ConsentRequired {
		t.Error("another member is asked")
	}
	wider := apps.ScopeStatsRead + " " + apps.ScopeIdentityRead
	if auth, _ := r.svc.CheckAuthorization(ctx, r.request(wider), r.member); !auth.ConsentRequired {
		t.Error("a wider request asks again")
	}
	r.code(t, apps.ScopeIdentityRead) // allowing a different scope widens the grant, never narrows it
	g, err := r.st.GetAppGrant(ctx, r.member, r.muster.App.ID)
	if err != nil || strings.Join(g.Scopes, " ") != "identity:read stats:read" {
		t.Errorf("grant after a second consent: %+v %v", g, err)
	}
	if auth, _ := r.svc.CheckAuthorization(ctx, r.request(wider), r.member); auth.ConsentRequired {
		t.Error("both scopes are covered now")
	}
	list, err := r.svc.Grants(ctx, r.member)
	if err != nil || len(list) != 1 || list[0].AppName != "Muster" || list[0].ClientID != r.muster.App.ClientID {
		t.Errorf("Grants: %+v %v", list, err)
	}
}

func TestExchange(t *testing.T) {
	r := newCodeRig(t)
	ctx := context.Background()
	id, secret := r.muster.App.ClientID, r.muster.Secret

	code := r.code(t, apps.ScopeStatsRead)
	if len(r.st.Codes) != 1 {
		t.Fatal("one code stored")
	}
	for _, c := range r.st.Codes {
		if string(c.CodeHash) == code || !c.ExpiresAt.Equal(r.now.Add(apps.CodeTTL)) || c.UserID != r.member || c.RedirectURI != redirect {
			t.Errorf("stored code: %+v", c)
		}
	}
	tok, err := r.svc.Exchange(ctx, id, secret, code, redirect, verifier)
	if err != nil || tok.Value == "" || !tok.ExpiresAt.Equal(r.now.Add(apps.MemberTokenTTL)) || strings.Join(tok.Scopes, " ") != apps.ScopeStatsRead {
		t.Fatalf("Exchange: %+v %v", tok, err)
	}
	p, err := r.svc.Authenticate(ctx, tok.Value)
	if err != nil || !p.IsMember() || p.UserID != r.member || p.ClientID != id || !p.Has(apps.ScopeStatsRead) || p.Has(apps.ScopeIdentityRead) {
		t.Errorf("the member's token: %+v %v", p, err)
	}

	// Replaying the code is refused and revokes the token issued from it.
	if _, err := r.svc.Exchange(ctx, id, secret, code, redirect, verifier); !errors.Is(err, apps.ErrCodeReplayed) || !errors.Is(err, apps.ErrInvalidGrant) {
		t.Errorf("replay: %v", err)
	}
	if _, err := r.svc.Authenticate(ctx, tok.Value); !errors.Is(err, apps.ErrInvalidToken) {
		t.Errorf("the token of a replayed code: %v", err)
	}

	for name, tc := range map[string]struct {
		redirect, verifier string
		wait               time.Duration
	}{
		"the wrong PKCE verifier":         {redirect, strings.Replace(verifier, "d", "e", 1), 0},
		"no PKCE verifier":                {redirect, "", 0},
		"a short verifier":                {redirect, "abc", 0},
		"the challenge as the verifier":   {redirect, challenge(verifier), 0},
		"a redirect_uri with a slash":     {redirect + "/", verifier, 0},
		"the other registered redirect":   {"http://127.0.0.1:8765/cb", verifier, 0},
		"no redirect_uri":                 {"", verifier, 0},
		"an expired code":                 {redirect, verifier, apps.CodeTTL},
		"a code just inside its lifetime": {redirect, verifier, apps.CodeTTL - time.Second},
	} {
		code := r.code(t, "")
		*r.now = r.now.Add(tc.wait)
		_, err := r.svc.Exchange(ctx, id, secret, code, tc.redirect, tc.verifier)
		*r.now = r.now.Add(-tc.wait)
		if name == "a code just inside its lifetime" {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if !errors.Is(err, apps.ErrInvalidGrant) || errors.Is(err, apps.ErrCodeReplayed) {
			t.Errorf("%s: %v", name, err)
		}
		// A refused presentation spends the code: the right one afterwards is a replay.
		if _, err := r.svc.Exchange(ctx, id, secret, code, redirect, verifier); !errors.Is(err, apps.ErrCodeReplayed) {
			t.Errorf("%s, then the right verifier: %v", name, err)
		}
	}
	if _, err := r.svc.Exchange(ctx, id, secret, "no-such-code", redirect, verifier); !errors.Is(err, apps.ErrInvalidGrant) {
		t.Errorf("unknown code: %v", err)
	}
	if _, err := r.svc.Exchange(ctx, id, secret, "", redirect, verifier); !errors.Is(err, apps.ErrInvalidGrant) {
		t.Errorf("no code: %v", err)
	}
	code = r.code(t, "")
	if _, err := r.svc.Exchange(ctx, id, "wrong", code, redirect, verifier); !errors.Is(err, apps.ErrInvalidClient) {
		t.Errorf("wrong secret: %v", err)
	}
	other, err := r.svc.Create(ctx, "Other", nil, redirect)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Exchange(ctx, other.App.ClientID, other.Secret, code, redirect, verifier); !errors.Is(err, apps.ErrInvalidGrant) {
		t.Errorf("another client's code: %v", err)
	}
	// An app revoked between the code and the exchange gets nothing.
	code = r.code(t, "")
	if err := r.svc.Revoke(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Exchange(ctx, id, secret, code, redirect, verifier); !errors.Is(err, apps.ErrInvalidClient) {
		t.Errorf("revoked app: %v", err)
	}
	if len(r.st.Grants) != 0 {
		t.Error("revoking the app deletes its grants")
	}
}

func TestRevokeGrantAndPrune(t *testing.T) {
	r := newCodeRig(t)
	ctx := context.Background()
	tok, err := r.svc.Exchange(ctx, r.muster.App.ClientID, r.muster.Secret, r.code(t, ""), redirect, verifier)
	if err != nil {
		t.Fatal(err)
	}
	pending := r.code(t, "") // not yet exchanged
	appTok, err := r.svc.Issue(ctx, r.muster.App.ClientID, r.muster.Secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	n, err := r.svc.RevokeGrant(ctx, r.member, r.muster.App.ClientID)
	if err != nil || n != 1 {
		t.Fatalf("RevokeGrant: %d %v", n, err)
	}
	if in, _ := r.svc.Introspect(ctx, tok.Value); in.Active {
		t.Error("a revoked grant's token is inactive")
	}
	if _, err := r.svc.Exchange(ctx, r.muster.App.ClientID, r.muster.Secret, pending, redirect, verifier); !errors.Is(err, apps.ErrInvalidGrant) {
		t.Errorf("a code of a revoked grant: %v", err)
	}
	if in, _ := r.svc.Introspect(ctx, appTok.Value); !in.Active {
		t.Error("the app's own token is untouched")
	}
	if _, err := r.svc.RevokeGrant(ctx, r.member, r.muster.App.ClientID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("revoke twice: %v", err)
	}
	if _, err := r.svc.RevokeGrant(ctx, r.member, "gravel_nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("revoke an unknown app: %v", err)
	}
	// A used code is kept while a token from it can live, so a replay is caught; then pruned.
	r.code(t, "")
	*r.now = r.now.Add(apps.MemberTokenTTL)
	if _, err := r.svc.Prune(ctx); err != nil || len(r.st.Codes) != 1 {
		t.Errorf("prune inside the window: %v, %d codes", err, len(r.st.Codes))
	}
	*r.now = r.now.Add(apps.CodeTTL)
	if _, err := r.svc.Prune(ctx); err != nil || len(r.st.Codes) != 0 {
		t.Errorf("prune after: %v, %d codes", err, len(r.st.Codes))
	}
}

func TestIntrospect(t *testing.T) {
	r := newCodeRig(t)
	ctx := context.Background()
	member, err := r.svc.Exchange(ctx, r.muster.App.ClientID, r.muster.Secret, r.code(t, apps.ScopeStatsRead), redirect, verifier)
	if err != nil {
		t.Fatal(err)
	}
	app, err := r.svc.Issue(ctx, r.muster.App.ClientID, r.muster.Secret, []string{apps.ScopeIdentityRead})
	if err != nil {
		t.Fatal(err)
	}
	in, err := r.svc.Introspect(ctx, member.Value)
	if err != nil || !in.Active || in.Sub != r.member.String() || in.ClientID != r.muster.App.ClientID || in.Scope != apps.ScopeStatsRead || in.Exp != member.ExpiresAt.Unix() {
		t.Errorf("member token: %+v %v", in, err)
	}
	in, err = r.svc.Introspect(ctx, app.Value)
	if err != nil || !in.Active || in.Sub != "" || in.Scope != apps.ScopeIdentityRead {
		t.Errorf("app token: %+v %v", in, err)
	}
	if in, _ := r.svc.Introspect(ctx, "unknown"); in.Active {
		t.Error("unknown token")
	}
	// Another organization's token, in the same store, is inactive here.
	otherOrg := apps.New(r.st, uuid.New(), time.Hour, quiet(), apps.WithClock(func() time.Time { return *r.now }))
	foreign, err := otherOrg.Create(ctx, "Theirs", nil)
	if err != nil {
		t.Fatal(err)
	}
	ftok, err := otherOrg.Issue(ctx, foreign.App.ClientID, foreign.Secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	if in, _ := r.svc.Introspect(ctx, ftok.Value); in.Active {
		t.Error("another organization's token is inactive")
	}
	if _, err := r.svc.Authenticate(ctx, ftok.Value); !errors.Is(err, apps.ErrInvalidToken) {
		t.Errorf("another organization's token authenticates nothing: %v", err)
	}
	if _, err := r.svc.Issue(ctx, foreign.App.ClientID, foreign.Secret, nil); !errors.Is(err, apps.ErrInvalidClient) {
		t.Errorf("another organization's client: %v", err)
	}
	*r.now = r.now.Add(apps.MemberTokenTTL)
	if in, _ := r.svc.Introspect(ctx, member.Value); in.Active {
		t.Error("expired token")
	}
	if b, _ := json.Marshal(apps.Introspection{Active: false, Sub: "x", ClientID: "y"}); string(b) != `{"active":false}` {
		t.Errorf("inactive JSON: %s", b)
	}
}

func form(t *testing.T, h http.Handler, path string, f url.Values, basic [2]string) tokenReply {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, strings.NewReader(f.Encode()))
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

func TestTokenHandlerAuthorizationCode(t *testing.T) {
	r := newCodeRig(t)
	h := r.svc.TokenHandler()
	creds := [2]string{r.muster.App.ClientID, r.muster.Secret}
	grant := func(code string) url.Values {
		return url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect}, "code_verifier": {verifier}}
	}
	code := r.code(t, apps.ScopeStatsRead)
	res := form(t, h, "/oauth/token", grant(code), creds)
	if res.status != 200 || res.body["token_type"] != "Bearer" || res.body["expires_in"] != float64(3600) || res.body["scope"] != "stats:read" || res.body["refresh_token"] != nil {
		t.Fatalf("exchange: %d %v", res.status, res.body)
	}
	if res := form(t, h, "/oauth/token", grant(code), creds); res.status != 400 || res.body["error"] != "invalid_grant" {
		t.Errorf("replay: %d %v", res.status, res.body)
	}
	for _, k := range []string{"code", "redirect_uri", "code_verifier"} {
		f := grant(r.code(t, ""))
		f.Del(k)
		if res := form(t, h, "/oauth/token", f, creds); res.status != 400 || res.body["error"] != "invalid_request" || !strings.Contains(res.body["error_description"].(string), k) {
			t.Errorf("without %s: %d %v", k, res.status, res.body)
		}
	}
	f := grant(r.code(t, ""))
	f.Set("code_verifier", strings.Repeat("a", 43))
	if res := form(t, h, "/oauth/token", f, creds); res.status != 400 || res.body["error"] != "invalid_grant" {
		t.Errorf("wrong verifier: %d %v", res.status, res.body)
	}
	if res := form(t, h, "/oauth/token", grant(r.code(t, "")), [2]string{r.muster.App.ClientID, "wrong"}); res.status != 401 || res.body["error"] != "invalid_client" {
		t.Errorf("wrong secret: %d %v", res.status, res.body)
	}
	// client_secret_post works too.
	f = grant(r.code(t, ""))
	f.Set("client_id", r.muster.App.ClientID)
	f.Set("client_secret", r.muster.Secret)
	if res := form(t, h, "/oauth/token", f, [2]string{}); res.status != 200 {
		t.Errorf("client_secret_post: %d %v", res.status, res.body)
	}
}

func TestIntrospectHandler(t *testing.T) {
	r := newCodeRig(t)
	ctx := context.Background()
	h := r.svc.IntrospectHandler()
	tok, err := r.svc.Exchange(ctx, r.muster.App.ClientID, r.muster.Secret, r.code(t, apps.ScopeStatsRead), redirect, verifier)
	if err != nil {
		t.Fatal(err)
	}
	// Any app of the organization may ask (a service the app talks to has its own credentials).
	checker, err := r.svc.Create(ctx, "checker", nil)
	if err != nil {
		t.Fatal(err)
	}
	creds := [2]string{checker.App.ClientID, checker.Secret}
	res := form(t, h, "/oauth/introspect", url.Values{"token": {tok.Value}}, creds)
	want := map[string]any{"active": true, "sub": r.member.String(), "client_id": r.muster.App.ClientID, "scope": "stats:read", "exp": float64(tok.ExpiresAt.Unix()), "token_type": "Bearer"}
	if res.status != 200 || len(res.body) != len(want) || res.header.Get("Cache-Control") != "no-store" {
		t.Errorf("active: %d %v", res.status, res.body)
	}
	for k, v := range want {
		if res.body[k] != v {
			t.Errorf("active %s = %v, want %v", k, res.body[k], v)
		}
	}
	res = form(t, h, "/oauth/introspect", url.Values{"token": {"nope"}, "token_type_hint": {"access_token"}}, creds)
	if res.status != 200 || len(res.body) != 1 || res.body["active"] != false {
		t.Errorf("inactive carries nothing else: %d %v", res.status, res.body)
	}
	if res := form(t, h, "/oauth/introspect", url.Values{"token": {tok.Value}}, [2]string{}); res.status != 401 || res.body["error"] != "invalid_client" || res.body["active"] != nil {
		t.Errorf("unauthenticated: %d %v", res.status, res.body)
	}
	if res := form(t, h, "/oauth/introspect", url.Values{"token": {tok.Value}}, [2]string{checker.App.ClientID, "wrong"}); res.status != 401 || res.body["active"] != nil {
		t.Errorf("wrong secret: %d %v", res.status, res.body)
	}
	if res := form(t, h, "/oauth/introspect", url.Values{"token": {tok.Value}}, [2]string{tok.Value, ""}); res.status != 401 {
		t.Errorf("a bearer token is not a client credential: %d %v", res.status, res.body)
	}
	if res := form(t, h, "/oauth/introspect", url.Values{}, creds); res.status != 400 || res.body["error"] != "invalid_request" {
		t.Errorf("no token: %d %v", res.status, res.body)
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/oauth/introspect?token="+tok.Value, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 405 {
		t.Errorf("GET: %d", rec.Code)
	}
}

// A member's token is never an app in the request context: an app-only check (apps.FromContext,
// as on_behalf_of and app-only procedures use) cannot be satisfied by an app acting as a member.
func TestMiddlewareKeepsMembersApartFromApps(t *testing.T) {
	r := newCodeRig(t)
	ctx := context.Background()
	tok, err := r.svc.Exchange(ctx, r.muster.App.ClientID, r.muster.Secret, r.code(t, apps.ScopeStatsRead), redirect, verifier)
	if err != nil {
		t.Fatal(err)
	}
	appTok, err := r.svc.Issue(ctx, r.muster.App.ClientID, r.muster.Secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	var isApp, isMember bool
	var m apps.Member
	h := r.svc.Middleware(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, isApp = apps.FromContext(req.Context())
		m, isMember = apps.MemberFromContext(req.Context())
	}))
	call := func(token string) {
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	call(tok.Value)
	if isApp || !isMember || m.UserID != r.member || m.App.ClientID != r.muster.App.ClientID || !m.Has(apps.ScopeStatsRead) {
		t.Errorf("member token: app=%v member=%v %+v", isApp, isMember, m)
	}
	call(appTok.Value)
	if !isApp || isMember {
		t.Errorf("app token: app=%v member=%v", isApp, isMember)
	}
}
