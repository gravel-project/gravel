package hub_test

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/oauth2"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/apps"
	"github.com/gravel-project/gravel/internal/hub"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

var hiddenInput = regexp.MustCompile(`<input type="hidden" name="([a-z_]+)" value="([^"]*)">`)

// gravel#6's "done when", through the real hub and Postgres: a first-party app signs a member in
// with golang.org/x/oauth2 (PKCE), calls the API as the member, and introspects the token; the
// metrics count it all; logging out everywhere and revoking the app end the token.
func TestAuthorizationCodeThroughTheHub(t *testing.T) {
	cfg := testConfig(t)
	st := storetest.Open(t)
	storetest.Reset(t, st)
	t.Cleanup(func() { storetest.Reset(t, st) })
	ctx := context.Background()
	h, err := hub.New(ctx, hub.Options{Config: cfg, Logger: quiet(), Version: "test", Providers: fakeProviders()})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	public := newH2CServer(h.Handler())
	defer public.Close()
	internal := httptest.NewServer(h.InternalHandler())
	defer internal.Close()
	o, err := st.GetBuiltinOrganization(ctx)
	if err != nil {
		t.Fatal(err)
	}
	svc := apps.New(st, o.ID, time.Hour, quiet())
	callback := "http://127.0.0.1:8765/auth/callback"
	muster, err := svc.Create(ctx, "Muster", []string{apps.ScopeStatsRead}, callback)
	if err != nil {
		t.Fatal(err)
	}

	c := browser(t)
	loginAs(t, c, public.URL)
	cfgOAuth := oauth2.Config{ClientID: muster.App.ClientID, ClientSecret: muster.Secret, RedirectURL: callback, Scopes: []string{apps.ScopeStatsRead},
		Endpoint: oauth2.Endpoint{AuthURL: public.URL + "/oauth/authorize", TokenURL: public.URL + "/oauth/token", AuthStyle: oauth2.AuthStyleInHeader}}
	verifier := oauth2.GenerateVerifier()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, cfgOAuth.AuthCodeURL("st", oauth2.S256ChallengeOption(verifier)), nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "Sign in to Muster?") {
		t.Fatalf("consent: %d %s", resp.StatusCode, body)
	}
	form := url.Values{"decision": {"allow"}}
	for _, m := range hiddenInput.FindAllStringSubmatch(string(body), -1) {
		form.Set(m[1], html.UnescapeString(m[2]))
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodPost, public.URL+"/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusSeeOther || loc.Query().Get("state") != "st" || loc.Query().Get("code") == "" {
		t.Fatalf("allow: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	tok, err := cfgOAuth.Exchange(ctx, loc.Query().Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		t.Fatal(err)
	}
	me, err := hubv1connect.NewIdentityServiceClient(cfgOAuth.Client(ctx, tok), public.URL).GetMe(ctx, connect.NewRequest(&hubv1.GetMeRequest{}))
	if err != nil || me.Msg.GetUser().GetDisplayName() != "Jo" {
		t.Fatalf("GetMe: %v %v", me, err)
	}

	introspect := func() map[string]any {
		t.Helper()
		f := url.Values{"token": {tok.AccessToken}}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, public.URL+"/oauth/introspect", strings.NewReader(f.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetBasicAuth(muster.App.ClientID, muster.Secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out
	}
	if in := introspect(); in["active"] != true || in["sub"] != me.Msg.GetUser().GetId() || in["scope"] != apps.ScopeStatsRead {
		t.Errorf("introspect: %v", in)
	}

	metrics := func() string {
		resp, err := http.Get(internal.URL + "/metrics") //nolint:noctx // test
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	m := metrics()
	for _, want := range []string{`gravel_oauth_authorizations_total{result="issued"} 1`, `gravel_oauth_token_grants_total{grant_type="authorization_code",result="issued"} 1`,
		`gravel_oauth_introspections_total{result="active"} 1`, `gravel_http_requests_total{code="200",handler="oauth_introspect",method="POST"} 1`} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics should contain %s", want)
		}
	}

	// Logging out everywhere ends the member's tokens in apps too.
	session := loginAs(t, browser(t), public.URL)
	ids := hubv1connect.NewIdentityServiceClient(&http.Client{Transport: cookieTransport{next: h2cClient().Transport, cookie: session}}, public.URL)
	if _, err := ids.RevokeSessions(ctx, connect.NewRequest(&hubv1.RevokeSessionsRequest{})); err != nil {
		t.Fatal(err)
	}
	if in := introspect(); in["active"] != false {
		t.Errorf("after logging out everywhere: %v", in)
	}
	if _, err := hubv1connect.NewIdentityServiceClient(cfgOAuth.Client(ctx, tok), public.URL).GetMe(ctx, connect.NewRequest(&hubv1.GetMeRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("GetMe after logging out everywhere: %v", err)
	}
	if err := svc.Revoke(ctx, muster.App.ClientID); err != nil {
		t.Fatal(err)
	}
	f := url.Values{"token": {tok.AccessToken}}
	req, _ = http.NewRequestWithContext(ctx, http.MethodPost, public.URL+"/oauth/introspect", strings.NewReader(f.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(muster.App.ClientID, muster.Secret)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a revoked app cannot introspect: %v %v", resp, err)
	} else {
		_ = resp.Body.Close()
	}
}
