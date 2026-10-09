package providers_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/identity/providers"
)

const (
	callbackURL  = "https://hub.example/auth/callback"
	clientID     = "cid"
	clientSecret = "very-secret"
	steamID      = "76561198000000001"
	steamKey     = "KEY-THAT-MUST-NOT-LEAK"
	openIDNS     = "http://specs.openid.net/auth/2.0"
)

// fake is one httptest server standing in for every provider host. The providers reach it
// through a transport that rewrites the hosts they dial, so the code under test keeps its real
// URLs. It records the paths it served so a test can assert that a call went where it should
// and nowhere else; anything unrouted fails the test.
type fake struct {
	t   *testing.T
	mux *http.ServeMux
	srv *httptest.Server

	mu    sync.Mutex
	paths []string
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{t: t, mux: http.NewServeMux()}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.mu.Unlock()
		f.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	f.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL)
		http.Error(w, "unexpected", http.StatusNotFound)
	})
	return f
}

// client returns the client to hand a provider: every provider host resolves to the fake.
func (f *fake) client() *http.Client {
	target, err := url.Parse(f.srv.URL)
	if err != nil {
		f.t.Fatal(err)
	}
	return &http.Client{Transport: rewriteHost{target: target, next: f.srv.Client().Transport}}
}

func (f *fake) served() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.paths)
}

// rewriteHost sends requests for the provider hosts to the fake, and refuses everything else,
// including a provider that drops to plain HTTP.
type rewriteHost struct {
	target *url.URL
	next   http.RoundTripper
}

func (rt rewriteHost) RoundTrip(req *http.Request) (*http.Response, error) {
	switch req.URL.Host {
	case "discord.com", "steamcommunity.com", "api.steampowered.com":
	default:
		return nil, fmt.Errorf("unexpected host %q", req.URL.Host)
	}
	if req.URL.Scheme != "https" {
		return nil, fmt.Errorf("plain %s to %s", req.URL.Scheme, req.URL.Host)
	}
	r := req.Clone(req.Context())
	r.URL.Scheme = rt.target.Scheme
	r.URL.Host = rt.target.Host
	return rt.next.RoundTrip(r)
}

func assertFailed(t *testing.T, err error, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

// ---- Discord -----------------------------------------------------------------------------------

type discordOpts struct {
	tokenStatus int    // non-zero: the token endpoint answers with this status and an OAuth2 error body
	userStatus  int    // non-zero: /users/@me answers with this status and no body
	userJSON    string // the /users/@me body
}

func discordFake(t *testing.T, o discordOpts) *fake {
	t.Helper()
	f := newFake(t)
	f.mux.HandleFunc("/api/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("token exchange: parse form: %v", err)
		}
		id, secret, ok := r.BasicAuth()
		if !ok {
			id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
		}
		if id != clientID || secret != clientSecret {
			t.Errorf("token exchange: client %q/%q, want %q/%q", id, secret, clientID, clientSecret)
		}
		for k, want := range map[string]string{"grant_type": "authorization_code", "code": "good", "redirect_uri": callbackURL} {
			if got := r.PostForm.Get(k); got != want {
				t.Errorf("token exchange: %s = %q, want %q", k, got, want)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if o.tokenStatus != 0 {
			w.WriteHeader(o.tokenStatus)
			fmt.Fprint(w, `{"error":"invalid_grant","error_description":"nope"}`)
			return
		}
		fmt.Fprint(w, `{"access_token":"tok","token_type":"Bearer","expires_in":3600}`)
	})
	f.mux.HandleFunc("/api/users/@me", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("users/@me: Authorization = %q, want Bearer tok", got)
		}
		if o.userStatus != 0 {
			w.WriteHeader(o.userStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, o.userJSON)
	})
	return f
}

func TestDiscordBegin(t *testing.T) {
	p := providers.Discord(clientID, clientSecret, callbackURL, newFake(t).client())
	if p.Name() != "discord" || p.DisplayName() != "Discord" {
		t.Fatalf("name %q / %q", p.Name(), p.DisplayName())
	}
	authURL, session, err := p.Begin(t.Context(), "st4te")
	if err != nil {
		t.Fatal(err)
	}
	if session == "" {
		t.Fatal("empty session")
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "discord.com" || u.Path != "/api/oauth2/authorize" {
		t.Fatalf("auth URL = %s", authURL)
	}
	q := u.Query()
	want := map[string]string{
		"client_id":     clientID,
		"redirect_uri":  callbackURL,
		"scope":         "identify",
		"state":         "st4te",
		"response_type": "code",
		"prompt":        "none",
	}
	for k, v := range want {
		if got := q.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestDiscordComplete(t *testing.T) {
	const profile = `{"id":"123","username":"jo","global_name":"Jo","avatar":"abc"}`
	cases := []struct {
		name      string
		opts      discordOpts
		params    url.Values
		want      identity.Account
		wantErr   error
		wantToken bool // the token endpoint must have been called
		wantUser  bool // /users/@me must have been called
	}{
		{
			name:      "ok",
			opts:      discordOpts{userJSON: profile},
			params:    url.Values{"code": {"good"}, "state": {"st4te"}},
			want:      identity.Account{Provider: "discord", Subject: "123", DisplayName: "Jo", Method: identity.MethodOAuth2},
			wantToken: true,
			wantUser:  true,
		},
		{
			name:      "username when global_name is null",
			opts:      discordOpts{userJSON: `{"id":"123","username":"jo","global_name":null,"avatar":"abc"}`},
			params:    url.Values{"code": {"good"}},
			want:      identity.Account{Provider: "discord", Subject: "123", DisplayName: "jo", Method: identity.MethodOAuth2},
			wantToken: true,
			wantUser:  true,
		},
		{
			name:      "username when global_name is absent",
			opts:      discordOpts{userJSON: `{"id":"123","username":"jo","avatar":"abc"}`},
			params:    url.Values{"code": {"good"}},
			want:      identity.Account{Provider: "discord", Subject: "123", DisplayName: "jo", Method: identity.MethodOAuth2},
			wantToken: true,
			wantUser:  true,
		},
		{
			name:    "access denied",
			opts:    discordOpts{userJSON: profile},
			params:  url.Values{"error": {"access_denied"}, "error_description": {"The resource owner denied the request"}},
			wantErr: identity.ErrProviderDenied,
		},
		{
			name:    "other callback error",
			opts:    discordOpts{userJSON: profile},
			params:  url.Values{"error": {"server_error"}},
			wantErr: identity.ErrProviderFailed,
		},
		{
			name:    "missing code",
			opts:    discordOpts{userJSON: profile},
			params:  url.Values{"state": {"st4te"}},
			wantErr: identity.ErrProviderFailed,
		},
		{
			name:      "token endpoint 400",
			opts:      discordOpts{tokenStatus: http.StatusBadRequest, userJSON: profile},
			params:    url.Values{"code": {"good"}},
			wantErr:   identity.ErrProviderFailed,
			wantToken: true,
		},
		{
			name:      "users/@me 401",
			opts:      discordOpts{userStatus: http.StatusUnauthorized, userJSON: profile},
			params:    url.Values{"code": {"good"}},
			wantErr:   identity.ErrProviderFailed,
			wantToken: true,
			wantUser:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := discordFake(t, tc.opts)
			p := providers.Discord(clientID, clientSecret, callbackURL, f.client())
			_, session, err := p.Begin(t.Context(), "st4te")
			if err != nil {
				t.Fatal(err)
			}
			got, err := p.Complete(t.Context(), session, tc.params)
			if tc.wantErr != nil {
				assertFailed(t, err, tc.wantErr)
				if strings.Contains(err.Error(), clientSecret) {
					t.Fatalf("error leaks the client secret: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(got.AvatarURL, "123/abc.jpg") {
					t.Errorf("AvatarURL = %q", got.AvatarURL)
				}
				got.AvatarURL = ""
				if got != tc.want {
					t.Errorf("account = %+v, want %+v", got, tc.want)
				}
			}
			served := f.served()
			if slices.Contains(served, "/api/oauth2/token") != tc.wantToken {
				t.Errorf("token endpoint called: served %v, want called=%v", served, tc.wantToken)
			}
			if slices.Contains(served, "/api/users/@me") != tc.wantUser {
				t.Errorf("users/@me called: served %v, want called=%v", served, tc.wantUser)
			}
			if tc.wantErr == nil && !slices.Equal(served, []string{"/api/oauth2/token", "/api/users/@me"}) {
				t.Errorf("served %v", served)
			}
		})
	}
}

func TestDiscordBeginPublish(t *testing.T) {
	p := providers.Discord(clientID, clientSecret, callbackURL, newFake(t).client())
	authURL, _, err := p.BeginPublish(t.Context(), "st4te")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("scope") != "identify role_connections.write" || q.Get("redirect_uri") != callbackURL || q.Get("state") != "st4te" {
		t.Errorf("publish auth URL = %s", authURL)
	}
	// Login keeps asking for identify alone.
	loginURL, _, _ := p.Begin(t.Context(), "st4te")
	if lu, _ := url.Parse(loginURL); lu.Query().Get("scope") != "identify" {
		t.Errorf("login scope widened: %s", loginURL)
	}
}

func TestDiscordCompletePublish(t *testing.T) {
	const profile = `{"id":"123","username":"jo","global_name":"Jo","avatar":"abc"}`
	const path = "/api/v10/users/@me/applications/" + clientID + "/role-connection"
	f := discordFake(t, discordOpts{userJSON: profile})
	var mu sync.Mutex
	var got map[string]any
	status := http.StatusOK
	f.mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("role connection: %s, Authorization %q, Content-Type %q", r.Method, r.Header.Get("Authorization"), r.Header.Get("Content-Type"))
		}
		mu.Lock()
		defer mu.Unlock()
		got = nil
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("role connection body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status != http.StatusOK {
			fmt.Fprint(w, `{"code":50025,"message":"Invalid OAuth2 access token"}`)
			return
		}
		fmt.Fprint(w, `{"platform_name":"x","platform_username":"y","metadata":{}}`)
	})
	p := providers.Discord(clientID, clientSecret, callbackURL, f.client())
	acct, publish, err := p.CompletePublish(t.Context(), "", url.Values{"code": {"good"}, "state": {"st4te"}})
	if err != nil || acct.Subject != "123" || acct.DisplayName != "Jo" || publish == nil {
		t.Fatalf("complete: %+v %v", acct, err)
	}
	long := strings.Repeat("é", 60) // 120 bytes: cut to 50 on a rune boundary
	if err := publish(t.Context(), identity.Profile{Organization: long, DisplayName: "Jo", Providers: []string{"discord", "steam"}}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	meta, _ := got["metadata"].(map[string]any)
	name, _ := got["platform_name"].(string)
	if got["platform_username"] != "Jo" || meta["steam_linked"] != "1" || meta["xbox_linked"] != "0" || meta["supporter_tier"] != "0" || len(name) != 50 || !utf8.ValidString(name) {
		t.Errorf("role connection = %v", got)
	}
	mu.Unlock()
	if served := f.served(); !slices.Equal(served, []string{"/api/oauth2/token", "/api/users/@me", path}) {
		t.Errorf("served %v", served)
	}

	// No organization name, no platform_name: Discord shows the application's.
	if err := publish(t.Context(), identity.Profile{DisplayName: "Jo", Providers: []string{"discord"}}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if _, ok := got["platform_name"]; ok || got["metadata"].(map[string]any)["steam_linked"] != "0" {
		t.Errorf("without a name: %v", got)
	}
	status = http.StatusUnauthorized
	mu.Unlock()
	err = publish(t.Context(), identity.Profile{DisplayName: "Jo"})
	if !errors.Is(err, identity.ErrProviderFailed) || !strings.Contains(err.Error(), "status 401, code 50025") || strings.Contains(err.Error(), "Invalid OAuth2") {
		t.Errorf("a refusal keeps the status and code, not the message: %v", err)
	}

	// A failed authorization returns no publisher.
	_, publish, err = p.CompletePublish(t.Context(), "", url.Values{"error": {"access_denied"}})
	if !errors.Is(err, identity.ErrProviderDenied) || publish != nil {
		t.Errorf("denied: %v %v", err, publish != nil)
	}
}

// ---- Steam -------------------------------------------------------------------------------------

// steamCallback is the query Steam sends to the callback after a successful login, as a
// well-formed OpenID 2.0 positive assertion whose return_to carries the state.
func steamCallback(state string) url.Values {
	return url.Values{
		"openid.ns":             {openIDNS},
		"openid.mode":           {"id_res"},
		"openid.op_endpoint":    {"https://steamcommunity.com/openid/login"},
		"openid.claimed_id":     {"https://steamcommunity.com/openid/id/" + steamID},
		"openid.identity":       {"https://steamcommunity.com/openid/id/" + steamID},
		"openid.return_to":      {callbackURL + "?state=" + url.QueryEscape(state)},
		"openid.response_nonce": {"2026-10-08T19:00:00Zd41d8cd9"},
		"openid.assoc_handle":   {"1234567890"},
		"openid.signed":         {"signed,op_endpoint,claimed_id,identity,return_to,response_nonce,assoc_handle"},
		"openid.sig":            {"c2lnbmF0dXJl"},
		"state":                 {state},
	}
}

type steamOpts struct {
	callback      url.Values // what the signed fields forwarded to check_authentication must equal
	valid         bool       // the check_authentication answer
	summaryStatus int        // non-zero: the summaries endpoint answers with this status and no body
}

func steamFake(t *testing.T, o steamOpts) *fake {
	t.Helper()
	f := newFake(t)
	f.mux.HandleFunc("/openid/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("openid/login: method %s", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("openid/login: parse form: %v", err)
		}
		if got := r.PostForm.Get("openid.mode"); got != "check_authentication" {
			t.Errorf("openid/login: openid.mode = %q", got)
		}
		fields := []string{"ns", "signed", "sig"}
		fields = append(fields, strings.Split(o.callback.Get("openid.signed"), ",")...)
		for _, field := range fields {
			k := "openid." + field
			if got, want := r.PostForm.Get(k), o.callback.Get(k); got != want {
				t.Errorf("openid/login: %s = %q, want %q", k, got, want)
			}
		}
		fmt.Fprintf(w, "ns:%s\nis_valid:%t\n", openIDNS, o.valid)
	})
	f.mux.HandleFunc("/ISteamUser/GetPlayerSummaries/v0002/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if got := q.Get("key"); got != steamKey {
			t.Errorf("summaries: key = %q", got)
		}
		if got := q.Get("steamids"); got != steamID {
			t.Errorf("summaries: steamids = %q", got)
		}
		if o.summaryStatus != 0 {
			w.WriteHeader(o.summaryStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"response":{"players":[{"steamid":%q,"personaname":"Jo","avatarfull":"https://x/a.jpg"}]}}`, steamID)
	})
	return f
}

func TestSteamBegin(t *testing.T) {
	const state = "aB3_-x"
	p := providers.Steam(steamKey, callbackURL, newFake(t).client())
	if p.Name() != "steam" || p.DisplayName() != "Steam" {
		t.Fatalf("name %q / %q", p.Name(), p.DisplayName())
	}
	authURL, session, err := p.Begin(t.Context(), state)
	if err != nil {
		t.Fatal(err)
	}
	if session == "" {
		t.Fatal("empty session")
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "steamcommunity.com" || u.Path != "/openid/login" {
		t.Fatalf("auth URL = %s", authURL)
	}
	q := u.Query()
	want := map[string]string{
		"openid.ns":         openIDNS,
		"openid.mode":       "checkid_setup",
		"openid.return_to":  callbackURL + "?state=" + state,
		"openid.realm":      "https://hub.example",
		"openid.claimed_id": openIDNS + "/identifier_select",
		"openid.identity":   openIDNS + "/identifier_select",
	}
	for k, v := range want {
		if got := q.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestSteamComplete(t *testing.T) {
	cases := []struct {
		name      string
		apiKey    string
		state     string
		mutate    func(url.Values) // applied to the well-formed callback for state
		valid     bool
		summary   int
		want      identity.Account
		wantErr   error
		wantPaths []string
	}{
		{
			name:      "ok with a key",
			apiKey:    steamKey,
			state:     "st4te",
			valid:     true,
			want:      identity.Account{Provider: "steam", Subject: steamID, DisplayName: "Jo", AvatarURL: "https://x/a.jpg", Method: identity.MethodOpenID},
			wantPaths: []string{"/openid/login", "/ISteamUser/GetPlayerSummaries/v0002/"},
		},
		{
			name:      "ok without a key",
			state:     "st4te",
			valid:     true,
			want:      identity.Account{Provider: "steam", Subject: steamID, Method: identity.MethodOpenID},
			wantPaths: []string{"/openid/login"},
		},
		{
			name:      "state that needs escaping",
			state:     "a b/c=&d",
			valid:     true,
			want:      identity.Account{Provider: "steam", Subject: steamID, Method: identity.MethodOpenID},
			wantPaths: []string{"/openid/login"},
		},
		{
			name:      "is_valid false",
			apiKey:    steamKey,
			state:     "st4te",
			wantErr:   identity.ErrProviderFailed,
			wantPaths: []string{"/openid/login"},
		},
		{
			name:      "cancelled",
			apiKey:    steamKey,
			state:     "st4te",
			mutate:    func(v url.Values) { v.Set("openid.mode", "cancel") },
			valid:     true,
			wantErr:   identity.ErrProviderFailed,
			wantPaths: []string{},
		},
		{
			name:   "tampered return_to",
			apiKey: steamKey,
			state:  "st4te",
			mutate: func(v url.Values) {
				v.Set("openid.return_to", callbackURL+"?state=0ther")
			},
			valid:     true,
			wantErr:   identity.ErrProviderFailed,
			wantPaths: []string{},
		},
		{
			name:   "claimed_id outside steamcommunity",
			apiKey: steamKey,
			state:  "st4te",
			mutate: func(v url.Values) {
				v.Set("openid.claimed_id", "https://example.com/openid/id/"+steamID)
				v.Set("openid.identity", "https://example.com/openid/id/"+steamID)
			},
			valid:     true,
			wantErr:   identity.ErrProviderFailed,
			wantPaths: []string{"/openid/login"},
		},
		{
			name:      "summaries 403",
			apiKey:    steamKey,
			state:     "st4te",
			valid:     true,
			summary:   http.StatusForbidden,
			wantErr:   identity.ErrProviderFailed,
			wantPaths: []string{"/openid/login", "/ISteamUser/GetPlayerSummaries/v0002/"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := steamCallback(tc.state)
			if tc.mutate != nil {
				tc.mutate(params)
			}
			f := steamFake(t, steamOpts{callback: params, valid: tc.valid, summaryStatus: tc.summary})
			p := providers.Steam(tc.apiKey, callbackURL, f.client())
			_, session, err := p.Begin(t.Context(), tc.state)
			if err != nil {
				t.Fatal(err)
			}
			got, err := p.Complete(t.Context(), session, params)
			if tc.wantErr != nil {
				assertFailed(t, err, tc.wantErr)
				if strings.Contains(err.Error(), steamKey) {
					t.Fatalf("error leaks the API key: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if got != tc.want {
					t.Errorf("account = %+v, want %+v", got, tc.want)
				}
			}
			if served := f.served(); !slices.Equal(served, tc.wantPaths) {
				t.Errorf("served %v, want %v", served, tc.wantPaths)
			}
		})
	}
}

// ---- both --------------------------------------------------------------------------------------

func TestNilClient(t *testing.T) {
	for _, p := range []identity.Provider{
		providers.Discord(clientID, clientSecret, callbackURL, nil),
		providers.Steam("", callbackURL, nil),
	} {
		authURL, session, err := p.Begin(t.Context(), "st4te")
		if err != nil {
			t.Fatalf("%s: %v", p.Name(), err)
		}
		if authURL == "" || session == "" {
			t.Fatalf("%s: empty URL or session", p.Name())
		}
	}
}
