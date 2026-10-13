package web_test

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"golang.org/x/oauth2"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/apps"
	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/web"
)

const musterCallback = "http://127.0.0.1:8765/auth/callback"

// muster registers Muster with two scopes and the loopback redirect URI.
func (r *rig) muster(t *testing.T) apps.Created {
	t.Helper()
	c, err := r.apps.Create(context.Background(), store.Actor{}, "Muster", []string{apps.ScopeStatsRead, apps.ScopeIdentityRead}, musterCallback)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// authorizeURL is an authorization request for Muster, as x/oauth2 builds it.
func (r *rig) authorizeURL(m apps.Created, verifier, state string, scopes ...string) string {
	cfg := oauth2.Config{ClientID: m.App.ClientID, RedirectURL: musterCallback, Scopes: scopes,
		Endpoint: oauth2.Endpoint{AuthURL: r.srv.URL + "/oauth/authorize", TokenURL: r.srv.URL + "/oauth/token", AuthStyle: oauth2.AuthStyleInHeader}}
	return cfg.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))
}

// toApp checks a redirect to Muster's callback and returns its query.
func toApp(t *testing.T, resp reply) url.Values {
	t.Helper()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("want a redirect to the app, got %d", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || loc.Scheme+"://"+loc.Host+loc.Path != musterCallback {
		t.Fatalf("redirect to %q, want the app's callback", resp.Header.Get("Location"))
	}
	return loc.Query()
}

// consentForm returns the consent page's form fields with the decision set.
func consentForm(t *testing.T, body, decision string) url.Values {
	t.Helper()
	f := url.Values{"_csrf": {csrfOf(t, body)}, "decision": {decision}}
	for _, k := range []string{"response_type", "client_id", "redirect_uri", "scope", "state", "code_challenge", "code_challenge_method"} {
		m := strings.Index(body, `name="`+k+`" value="`)
		if m < 0 {
			t.Fatalf("consent form has no %s", k)
		}
		rest := body[m+len(`name="`+k+`" value="`):]
		v := rest[:strings.Index(rest, `"`)]
		f.Set(k, strings.NewReplacer("&amp;", "&", "&#34;", `"`, "&#39;", "'", "&lt;", "<", "&gt;", ">").Replace(v))
	}
	return f
}

// The whole flow with an independent client: golang.org/x/oauth2 builds the request with PKCE,
// the member logs in and allows, the client exchanges the code at the real token endpoint, and
// calls GetMe through the Connect client with the token.
func TestAuthorizationCodeConformance(t *testing.T) {
	r := newRig(t)
	m := r.muster(t)
	c := browser(t)
	ctx := context.Background()
	verifier := oauth2.GenerateVerifier()
	cfg := oauth2.Config{ClientID: m.App.ClientID, ClientSecret: m.Secret, RedirectURL: musterCallback, Scopes: []string{apps.ScopeStatsRead},
		Endpoint: oauth2.Endpoint{AuthURL: r.srv.URL + "/oauth/authorize", TokenURL: r.srv.URL + "/oauth/token", AuthStyle: oauth2.AuthStyleInHeader}}
	start := cfg.AuthCodeURL("xyz-state", oauth2.S256ChallengeOption(verifier))

	// Not logged in: through the login, and back to the same request.
	resp, _ := get(t, c, start)
	u, _ := url.Parse(start)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login?return_to="+url.QueryEscape(u.RequestURI()) {
		t.Fatalf("anonymous: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, _ := get(t, c, r.srv.URL+resp.Header.Get("Location")); resp.StatusCode != http.StatusOK {
		t.Fatalf("login page: %d", resp.StatusCode)
	}
	resp = r.loginAs(t, c, "jo")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != u.RequestURI() {
		t.Fatalf("the login returns to the authorization request: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}

	// Consent.
	resp, body := get(t, c, r.srv.URL+resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Sign in to Muster?") || !strings.Contains(body, html.EscapeString(apps.MemberScopes[apps.ScopeStatsRead])) ||
		strings.Contains(body, html.EscapeString(apps.MemberScopes[apps.ScopeIdentityRead])) || !strings.Contains(body, "127.0.0.1:8765") ||
		!strings.Contains(body, `<form method="post" action="/oauth/authorize" class="consent">`) {
		t.Fatalf("consent page: %d\n%s", resp.StatusCode, body)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self' http://127.0.0.1:8765;") {
		t.Errorf("the consent page's form may go back to the app: %q", csp)
	}
	q := toApp(t, func() reply {
		resp, _ := post(t, c, r.srv.URL+"/oauth/authorize", consentForm(t, body, "allow"), nil)
		return resp
	}())
	if q.Get("state") != "xyz-state" || q.Get("code") == "" || q.Get("error") != "" {
		t.Fatalf("allowed: %v", q)
	}

	tok, err := cfg.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(verifier))
	if err != nil || tok.TokenType != "Bearer" || tok.RefreshToken != "" || tok.Extra("scope") != apps.ScopeStatsRead {
		t.Fatalf("Exchange: %+v %v", tok, err)
	}
	ids := hubv1connect.NewIdentityServiceClient(cfg.Client(ctx, tok), r.srv.URL)
	me, err := ids.GetMe(ctx, connect.NewRequest(&hubv1.GetMeRequest{}))
	if err != nil || me.Msg.GetUser().GetDisplayName() != "Jo" {
		t.Fatalf("GetMe with the member's token: %v %v", me, err)
	}
	if _, err := ids.UnlinkIdentity(ctx, connect.NewRequest(&hubv1.UnlinkIdentityRequest{Provider: "discord", Subject: "1"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("the token cannot unlink: %v", err)
	}
	// The code is spent: x/oauth2 presenting it again is refused, and the token goes with it.
	if _, err := cfg.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(verifier)); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("the code twice: %v", err)
	}
	if _, err := ids.GetMe(ctx, connect.NewRequest(&hubv1.GetMeRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("the token of a replayed code: %v", err)
	}

	// Signing in again is silent: the grant is remembered.
	verifier = oauth2.GenerateVerifier()
	resp, _ = get(t, c, cfg.AuthCodeURL("again", oauth2.S256ChallengeOption(verifier)))
	q = toApp(t, resp)
	if q.Get("state") != "again" || q.Get("code") == "" {
		t.Fatalf("silent: %v", q)
	}
	if _, err := cfg.Exchange(ctx, q.Get("code"), oauth2.VerifierOption("a-wrong-verifier-that-is-long-enough-to-be-well-formed-123")); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("the wrong verifier: %v", err)
	}
	if strings.Join(r.authz, " ") != "issued issued" {
		t.Errorf("observed %v", r.authz)
	}
}

func TestAuthorizeRefusals(t *testing.T) {
	r := newRig(t)
	m := r.muster(t)
	c := browser(t)
	r.loginAs(t, c, "jo")
	verifier := oauth2.GenerateVerifier()
	good := r.authorizeURL(m, verifier, "s1", apps.ScopeStatsRead)

	// A bad client or redirect URI: an error page, never a redirect.
	for name, edit := range map[string]func(url.Values){
		"unknown client":             func(q url.Values) { q.Set("client_id", "gravel_nope") },
		"no client":                  func(q url.Values) { q.Del("client_id") },
		"redirect with a slash":      func(q url.Values) { q.Set("redirect_uri", musterCallback+"/") },
		"redirect on another port":   func(q url.Values) { q.Set("redirect_uri", "http://127.0.0.1:8766/auth/callback") },
		"redirect with https":        func(q url.Values) { q.Set("redirect_uri", "https://127.0.0.1:8765/auth/callback") },
		"redirect with a query":      func(q url.Values) { q.Set("redirect_uri", musterCallback+"?x=1") },
		"an attacker's redirect":     func(q url.Values) { q.Set("redirect_uri", "https://evil.example/cb") },
		"no redirect":                func(q url.Values) { q.Del("redirect_uri") },
		"redirect_uri twice":         func(q url.Values) { q.Add("redirect_uri", "https://evil.example/cb") },
		"client_id twice":            func(q url.Values) { q.Add("client_id", m.App.ClientID) },
		"bad client, bad PKCE, both": func(q url.Values) { q.Set("client_id", "x"); q.Del("code_challenge") },
	} {
		u, _ := url.Parse(good)
		q := u.Query()
		edit(q)
		u.RawQuery = q.Encode()
		resp, body := get(t, c, u.String())
		if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" || !strings.Contains(body, "Unknown app") {
			t.Errorf("%s: %d %s", name, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	// The rest goes back to the app with an error and its state.
	for name, tc := range map[string]struct {
		edit func(url.Values)
		want string
	}{
		"missing PKCE":          {func(q url.Values) { q.Del("code_challenge"); q.Del("code_challenge_method") }, "invalid_request"},
		"plain PKCE":            {func(q url.Values) { q.Set("code_challenge_method", "plain") }, "invalid_request"},
		"the implicit grant":    {func(q url.Values) { q.Set("response_type", "token") }, "unsupported_response_type"},
		"a scope the app lacks": {func(q url.Values) { q.Set("scope", apps.ScopeServersModerate) }, "invalid_scope"},
		"state twice":           {func(q url.Values) { q.Add("state", "other") }, "invalid_request"},
	} {
		u, _ := url.Parse(good)
		q := u.Query()
		tc.edit(q)
		u.RawQuery = q.Encode()
		resp, _ := get(t, c, u.String())
		back := toApp(t, resp)
		if back.Get("error") != tc.want || back.Get("state") != "s1" || back.Get("code") != "" {
			t.Errorf("%s: %v", name, back)
		}
	}
	if len(r.appSt.Codes) != 0 || len(r.appSt.Grants) != 0 {
		t.Error("no refusal stored a code or a grant")
	}

	// Cancel goes back with access_denied and the state; nothing is remembered.
	resp, body := get(t, c, good)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("consent: %d", resp.StatusCode)
	}
	resp, _ = post(t, c, r.srv.URL+"/oauth/authorize", consentForm(t, body, "deny"), nil)
	if back := toApp(t, resp); back.Get("error") != "access_denied" || back.Get("state") != "s1" || back.Get("code") != "" {
		t.Errorf("cancel: %v", back)
	}
	if len(r.appSt.Grants) != 0 {
		t.Error("cancel remembered a grant")
	}
	// The consent form refuses a missing or wrong CSRF token, and redirects nowhere.
	f := consentForm(t, body, "allow")
	f.Set("_csrf", "forged")
	if resp, _ := post(t, c, r.srv.URL+"/oauth/authorize", f, nil); resp.StatusCode != http.StatusForbidden || resp.Header.Get("Location") != "" {
		t.Errorf("forged csrf: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	f.Del("_csrf")
	if resp, _ := post(t, c, r.srv.URL+"/oauth/authorize", f, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("no csrf: %d", resp.StatusCode)
	}
	// A consent form whose redirect URI was swapped is an unknown app, not a redirect.
	f = consentForm(t, body, "allow")
	f.Set("redirect_uri", "https://evil.example/cb")
	if resp, _ := post(t, c, r.srv.URL+"/oauth/authorize", f, nil); resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
		t.Errorf("swapped redirect: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if len(r.appSt.Codes) != 0 {
		t.Error("a refused consent issued a code")
	}
}

func TestConsentRememberedThenAskedForMore(t *testing.T) {
	r := newRig(t)
	m := r.muster(t)
	c := browser(t)
	r.loginAs(t, c, "jo")

	_, body := get(t, c, r.authorizeURL(m, oauth2.GenerateVerifier(), "a", apps.ScopeStatsRead))
	if strings.Contains(body, "it now asks for more") {
		t.Error("the first consent is not a wider one")
	}
	resp, _ := post(t, c, r.srv.URL+"/oauth/authorize", consentForm(t, body, "allow"), nil)
	toApp(t, resp)
	resp, _ = get(t, c, r.authorizeURL(m, oauth2.GenerateVerifier(), "b", apps.ScopeStatsRead))
	if q := toApp(t, resp); q.Get("code") == "" {
		t.Errorf("remembered: %v", q)
	}
	resp, body = get(t, c, r.authorizeURL(m, oauth2.GenerateVerifier(), "c", apps.ScopeStatsRead, apps.ScopeIdentityRead))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "it now asks for more") || !strings.Contains(body, html.EscapeString(apps.MemberScopes[apps.ScopeIdentityRead])) {
		t.Fatalf("a wider request asks again: %d", resp.StatusCode)
	}
	resp, _ = post(t, c, r.srv.URL+"/oauth/authorize", consentForm(t, body, "allow"), nil)
	toApp(t, resp)
	// Another member is asked for themselves.
	c2 := browser(t)
	r.loginAs(t, c2, "sam")
	if resp, _ := get(t, c2, r.authorizeURL(m, oauth2.GenerateVerifier(), "d", apps.ScopeStatsRead)); resp.StatusCode != http.StatusOK {
		t.Errorf("another member: %d", resp.StatusCode)
	}
}

// The account page lists the apps a member allowed; Revoke (a POST with the CSRF token) removes
// the grant and makes the member's tokens for the app inactive.
func TestAccountAppsAndRevoke(t *testing.T) {
	r := newRig(t)
	m := r.muster(t)
	c := browser(t)
	r.loginAs(t, c, "jo")
	ctx := context.Background()
	verifier := oauth2.GenerateVerifier()

	_, body := get(t, c, r.srv.URL+"/account")
	if strings.Contains(body, "Apps you allowed") {
		t.Error("no apps section before any app is allowed")
	}
	_, body = get(t, c, r.authorizeURL(m, verifier, "s", apps.ScopeStatsRead))
	resp, _ := post(t, c, r.srv.URL+"/oauth/authorize", consentForm(t, body, "allow"), nil)
	code := toApp(t, resp).Get("code")
	tok, err := r.apps.Exchange(ctx, m.App.ClientID, m.Secret, code, musterCallback, verifier)
	if err != nil {
		t.Fatal(err)
	}
	introspect := func() map[string]any {
		f := url.Values{"token": {tok.Value}}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, r.srv.URL+"/oauth/introspect", strings.NewReader(f.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetBasicAuth(m.App.ClientID, m.Secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out
	}
	if in := introspect(); in["active"] != true || in["client_id"] != m.App.ClientID {
		t.Fatalf("introspect before: %v", in)
	}

	_, body = get(t, c, r.srv.URL+"/account")
	for _, want := range []string{"Apps you allowed", "Muster", html.EscapeString(apps.MemberScopes[apps.ScopeStatsRead]), `action="/account/apps/revoke"`, "Revoke Muster"} {
		if !strings.Contains(body, want) {
			t.Errorf("account page should contain %q", want)
		}
	}
	f := url.Values{"client_id": {m.App.ClientID}}
	if resp, _ := post(t, c, r.srv.URL+"/account/apps/revoke", f, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("revoke without csrf: %d", resp.StatusCode)
	}
	f.Set("_csrf", csrfOf(t, body))
	resp, body = post(t, c, r.srv.URL+"/account/apps/revoke", f, hx)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Revoked.") || strings.Contains(body, "Apps you allowed") {
		t.Errorf("revoke: %d %s", resp.StatusCode, body)
	}
	if in := introspect(); in["active"] != false || len(in) != 1 {
		t.Errorf("introspect after the revoke: %v", in)
	}
	resp, body = post(t, c, r.srv.URL+"/account/apps/revoke", f, hx)
	if !strings.Contains(body, "You have not allowed that app.") {
		t.Errorf("revoke twice: %d %s", resp.StatusCode, body)
	}
	// Next time Muster asks, the member is asked again.
	if resp, _ := get(t, c, r.authorizeURL(m, oauth2.GenerateVerifier(), "s", apps.ScopeStatsRead)); resp.StatusCode != http.StatusOK {
		t.Errorf("after the revoke, consent again: %d", resp.StatusCode)
	}
}

// The login's return path is this origin's only: anything else is ignored, never followed.
func TestLoginReturnIsSameOriginOnly(t *testing.T) {
	for _, ok := range []string{"/oauth/authorize?client_id=x&state=y", "/account", "/"} {
		if !web.SafeReturnPath(ok) {
			t.Errorf("%q should be a safe return", ok)
		}
	}
	for _, bad := range []string{"", "https://evil.example/", "//evil.example/", "/\\evil.example", "\\\\evil.example", "/\t/evil.example", "/\n/evil.example",
		"javascript:alert(1)", "http:evil.example", "evil.example", "/" + strings.Repeat("a", 5000)} {
		if web.SafeReturnPath(bad) {
			t.Errorf("%q must not be a return", bad)
		}
	}
	r := newRig(t)
	for _, target := range []string{"https://evil.example/", "//evil.example/x", "/\\evil.example"} {
		c := browser(t)
		resp, _ := get(t, c, r.srv.URL+"/login?return_to="+url.QueryEscape(target))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("login page: %d", resp.StatusCode)
		}
		if resp := r.loginAs(t, c, "jo"); resp.Header.Get("Location") != "/account" {
			t.Errorf("return_to %q: the login went to %q", target, resp.Header.Get("Location"))
		}
		if resp, _ := get(t, c, r.srv.URL+"/login?return_to="+url.QueryEscape(target)); resp.Header.Get("Location") != "/account" {
			t.Errorf("logged in, return_to %q: %q", target, resp.Header.Get("Location"))
		}
	}
	c := browser(t)
	get(t, c, r.srv.URL+"/login?return_to="+url.QueryEscape("/servers"))
	resp := r.loginAs(t, c, "jo")
	if resp.Header.Get("Location") != "/servers" {
		t.Errorf("a safe return: %q", resp.Header.Get("Location"))
	}
	cleared := false
	for _, ck := range resp.Cookies() {
		if ck.Name == "gravel_return" && ck.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("the return is used once: the callback clears it")
	}
}
