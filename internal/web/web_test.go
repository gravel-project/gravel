package web_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/api"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/identity/identitytest"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/org/orgtest"
	"github.com/gravel-project/gravel/internal/session"
	"github.com/gravel-project/gravel/internal/web"
	"github.com/gravel-project/gravel/internal/web/templates"
)

type rig struct {
	srv     *httptest.Server
	idSt    *identitytest.FakeStore
	discord *identitytest.FakePublisher
	steam   *identitytest.FakeProvider
	token   string
	results []string
}

// testTheme has a logo, a favicon, header and footer links, an owner-only link and one bad
// token, so the pages prove the theme seam.
func testTheme() templates.Theme {
	t := web.DefaultTheme()
	t.Light.Accent = "#0b5fff"
	t.Dark.Background = "not a colour"
	t.LogoURL = "https://cdn.example/logo.png"
	t.FaviconURL = "/static/app.css"
	t.Nav = []templates.NavLink{
		{Label: "Muster", URL: "https://muster.example", Placement: "header"},
		{Label: "Rules", URL: "https://example.com/rules", Placement: "footer"},
		{Label: "Admin", URL: "/admin", Placement: "header", Role: "owner"},
	}
	return t
}

func newRig(t *testing.T) *rig {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := &rig{idSt: identitytest.NewFakeStore()}
	orgSvc := org.New(&orgtest.FakeStore{}, 15*time.Minute, logger)
	o, token, err := orgSvc.EnsureBuiltin(context.Background(), "Hidden Token Gaming")
	if err != nil {
		t.Fatal(err)
	}
	r.token = token
	r.discord = &identitytest.FakePublisher{FakeProvider: identitytest.FakeProvider{ProviderName: "discord", Accounts: map[string]identity.Account{
		"jo":  {Subject: "1", DisplayName: "Jo", AvatarURL: "https://cdn.example/jo.png", Method: identity.MethodOAuth2},
		"sam": {Subject: "2", DisplayName: "Sam", Method: identity.MethodOAuth2},
	}}}
	r.steam = &identitytest.FakeProvider{ProviderName: "steam", Accounts: map[string]identity.Account{
		"jo-steam": {Subject: "76561198000000001", DisplayName: "JoOnSteam", Method: identity.MethodOpenID},
	}}
	ids := identity.New(r.idSt, o.ID, []identity.Registration{{Provider: r.discord, Login: true}, {Provider: r.steam}}, 10*time.Minute, logger)
	sess := session.New(r.idSt, time.Hour, false, logger)
	apiMux := http.NewServeMux()
	apiMux.Handle(hubv1connect.NewOrganizationServiceHandler(api.NewOrganizationServer(orgSvc, logger)))
	apiMux.Handle(hubv1connect.NewIdentityServiceHandler(api.NewIdentityServer(ids, sess, orgSvc, logger)))
	h, err := web.New(ids, sess, sess.Middleware(apiMux), web.StaticTheme{T: testTheme()}, logger)
	if err != nil {
		t.Fatal(err)
	}
	h.Observe = func(provider string, intent identity.Intent, result string) {
		r.results = append(r.results, provider+"/"+string(intent)+"/"+result)
	}
	mux := http.NewServeMux()
	h.Register(mux)
	r.srv = httptest.NewServer(http.NewCrossOriginProtection().Handler(sess.Middleware(mux)))
	t.Cleanup(r.srv.Close)
	return r
}

// browser is a client with a cookie jar that does not follow redirects.
func browser(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// reply is a response with its body already read and closed.
type reply struct {
	StatusCode int
	Header     http.Header
	cookies    []*http.Cookie
}

func (r reply) Cookies() []*http.Cookie { return r.cookies }

func do(t *testing.T, c *http.Client, req *http.Request) (reply, string) {
	t.Helper()
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return reply{StatusCode: resp.StatusCode, Header: resp.Header, cookies: resp.Cookies()}, string(body)
}

func get(t *testing.T, c *http.Client, u string, headers ...string) (reply, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	return do(t, c, req)
}

func post(t *testing.T, c *http.Client, u string, form url.Values, headers map[string]string) (reply, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return do(t, c, req)
}

var hx = map[string]string{"HX-Request": "true"}

var csrfRe = regexp.MustCompile(`name="_csrf" value="([^"]+)"`)

func csrfOf(t *testing.T, body string) string {
	t.Helper()
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no csrf token in page")
	}
	return m[1]
}

// loginAs drives the Discord flow for a code and returns the callback response.
func (r *rig) loginAs(t *testing.T, c *http.Client, code string) reply {
	t.Helper()
	resp, _ := get(t, c, r.srv.URL+"/auth/discord/start")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("start: %d", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	state := loc.Query().Get("state")
	if loc.Host != "discord.example" || state == "" {
		t.Fatalf("start redirect: %s", resp.Header.Get("Location"))
	}
	resp, _ = get(t, c, r.srv.URL+"/auth/discord/callback?code="+code+"&state="+url.QueryEscape(state))
	return resp
}

func TestLoginLinkUnlinkClaimLogout(t *testing.T) {
	r := newRig(t)
	c := browser(t)

	resp, _ := get(t, c, r.srv.URL+"/")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Errorf("home anonymous: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, body := get(t, c, r.srv.URL+"/login")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Log in with Discord") || strings.Contains(body, "Log in with Steam") {
		t.Errorf("login page: %d %s", resp.StatusCode, body)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "form-action 'self'") {
		t.Errorf("csp: %q", csp)
	}
	for _, want := range []string{`<script src="/static/vendor/htmx.min.js" defer>`, `name="htmx-config"`, `href="/theme.css"`, `src="https://cdn.example/logo.png"`, `href="https://muster.example">Muster`, `href="https://example.com/rules">Rules`, `<a class="skip" href="#page">`, `<main id="page" tabindex="-1">`} {
		if !strings.Contains(body, want) {
			t.Errorf("login page should contain %q", want)
		}
	}
	if strings.Contains(body, "/admin") {
		t.Error("the owner-only link must not show to anonymous visitors")
	}
	if strings.Contains(body, "<script>") || strings.Contains(body, "style=") {
		t.Error("no inline script or style, the CSP forbids them")
	}
	resp, _ = get(t, c, r.srv.URL+"/account")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Errorf("account anonymous: %d", resp.StatusCode)
	}

	resp = r.loginAs(t, c, "jo")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/account" {
		t.Fatalf("callback: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	var authCleared, sessionSet bool
	for _, ck := range resp.Cookies() {
		switch ck.Name {
		case "gravel_auth":
			authCleared = ck.MaxAge < 0
		case "gravel_session":
			sessionSet = ck.Value != "" && ck.HttpOnly
		}
	}
	if !authCleared || !sessionSet {
		t.Errorf("cookies after callback: cleared=%v session=%v", authCleared, sessionSet)
	}
	resp, body = get(t, c, r.srv.URL+"/account")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("account: %d", resp.StatusCode)
	}
	for _, want := range []string{"Welcome, Jo", "Link Steam", "your only account", "Claim this hub", `src="https://cdn.example/jo.png"`, "Log out everywhere", `hx-post="/account/claim"`} {
		if !strings.Contains(body, want) {
			t.Errorf("account page should contain %q", want)
		}
	}
	if strings.Contains(body, "/admin") {
		t.Error("the owner-only link must not show before the claim")
	}
	if _, body = get(t, c, r.srv.URL+"/account"); strings.Contains(body, "Welcome, Jo") {
		t.Error("the flash shows once")
	}
	if resp, _ = get(t, c, r.srv.URL+"/login"); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/account" {
		t.Errorf("login while logged in: %d", resp.StatusCode)
	}

	// Link Steam.
	resp, _ = get(t, c, r.srv.URL+"/auth/steam/link")
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusSeeOther || loc.Host != "steam.example" {
		t.Fatalf("link start: %d %s", resp.StatusCode, loc)
	}
	resp, _ = get(t, c, r.srv.URL+"/auth/steam/callback?code=jo-steam&state="+url.QueryEscape(loc.Query().Get("state")))
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("link callback: %d", resp.StatusCode)
	}
	_, body = get(t, c, r.srv.URL+"/account")
	if !strings.Contains(body, "Steam account JoOnSteam linked") || !strings.Contains(body, "Unlink Steam") || strings.Contains(body, "Link Steam") || strings.Contains(body, "your only account") {
		t.Errorf("account after link: %s", body)
	}

	// Unlink needs the CSRF token; over htmx the content comes back with the flash inline.
	csrf := csrfOf(t, body)
	resp, _ = post(t, c, r.srv.URL+"/account/unlink", url.Values{"provider": {"steam"}, "subject": {"76561198000000001"}}, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("unlink without csrf: %d", resp.StatusCode)
	}
	resp, body = post(t, c, r.srv.URL+"/account/unlink", url.Values{"_csrf": {csrf}, "provider": {"steam"}, "subject": {"76561198000000001"}}, hx)
	if resp.StatusCode != http.StatusOK || strings.Contains(body, "<html") || !strings.Contains(body, "Steam account unlinked") || !strings.Contains(body, "Link Steam") || !strings.Contains(body, "your only account") {
		t.Errorf("htmx unlink: %d %s", resp.StatusCode, body)
	}
	if v := resp.Header.Get("Vary"); !strings.Contains(v, "HX-Request") {
		t.Errorf("fragments vary on HX-Request: %q", v)
	}
	resp, _ = post(t, c, r.srv.URL+"/account/unlink", url.Values{"_csrf": {csrf}, "provider": {"discord"}, "subject": {"1"}}, nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("unlink last, plain: %d", resp.StatusCode)
	}
	_, body = get(t, c, r.srv.URL+"/account")
	if !strings.Contains(body, "cannot unlink your only account") {
		t.Errorf("unlink last: %s", body)
	}

	// A cross-site POST is refused before the handler, token or not.
	resp, _ = post(t, c, r.srv.URL+"/account/unlink", url.Values{"_csrf": {csrf}, "provider": {"discord"}, "subject": {"1"}}, map[string]string{"Sec-Fetch-Site": "cross-site"})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site post: %d", resp.StatusCode)
	}

	// Claim the hub: wrong token over htmx is a 200 fragment with the error, right token plain.
	resp, body = post(t, c, r.srv.URL+"/account/claim", url.Values{"_csrf": {csrf}, "token": {"wrong"}}, hx)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "not valid or has expired") || !strings.Contains(body, "Claim this hub") {
		t.Errorf("wrong claim over htmx: %d %s", resp.StatusCode, body)
	}
	post(t, c, r.srv.URL+"/account/claim", url.Values{"_csrf": {csrf}, "token": {r.token}}, nil)
	_, body = get(t, c, r.srv.URL+"/account")
	if !strings.Contains(body, "You now own this hub") || !strings.Contains(body, `class="badge">owner`) || strings.Contains(body, "Claim this hub") || !strings.Contains(body, `href="/admin">Admin`) {
		t.Errorf("after claim: %s", body)
	}

	// Log out over htmx: the API clears the cookie and htmx is told where to go.
	resp, _ = post(t, c, r.srv.URL+"/auth/logout", url.Values{"_csrf": {csrf}}, hx)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("HX-Redirect") != "/login" {
		t.Errorf("htmx logout: %d %q", resp.StatusCode, resp.Header.Get("HX-Redirect"))
	}
	var cleared bool
	for _, ck := range resp.Cookies() {
		if ck.Name == "gravel_session" && ck.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Errorf("the API's cookie clearing must reach the browser: %v", resp.Cookies())
	}
	if resp, _ = get(t, c, r.srv.URL+"/account"); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("after logout the account page needs a login: %d", resp.StatusCode)
	}
	_, body = get(t, c, r.srv.URL+"/login")
	if !strings.Contains(body, "Logged out.") {
		t.Errorf("logout flash: %s", body)
	}
	if got := strings.Join(r.results, " "); got != "discord/login/ok steam/link/ok" {
		t.Errorf("observed: %s", got)
	}
}

func TestCallbackFailures(t *testing.T) {
	r := newRig(t)
	c := browser(t)

	resp, _ := get(t, c, r.srv.URL+"/auth/discord/start")
	loc, _ := url.Parse(resp.Header.Get("Location"))
	cb := r.srv.URL + "/auth/discord/callback?code=jo&state=" + url.QueryEscape(loc.Query().Get("state"))
	if resp, _ = get(t, c, cb); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("first callback: %d", resp.StatusCode)
	}
	if resp, body := get(t, browser(t), cb); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "expired or was already used") || !strings.Contains(body, "Back to the login page") {
		t.Errorf("replay from another browser: %d %s", resp.StatusCode, body)
	}

	c = browser(t)
	get(t, c, r.srv.URL+"/auth/discord/start")
	if resp, body := get(t, c, r.srv.URL+"/auth/discord/callback?code=jo&state=forged"); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "does not match") {
		t.Errorf("mismatch: %d %s", resp.StatusCode, body)
	}

	c = browser(t)
	resp, _ = get(t, c, r.srv.URL+"/auth/discord/start")
	loc, _ = url.Parse(resp.Header.Get("Location"))
	if resp, body := get(t, c, r.srv.URL+"/auth/discord/callback?error=access_denied&state="+url.QueryEscape(loc.Query().Get("state"))); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "cancelled at Discord") {
		t.Errorf("denied: %d %s", resp.StatusCode, body)
	}

	if resp, _ := get(t, c, r.srv.URL+"/auth/steam/start"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("steam login: %d", resp.StatusCode)
	}
	if resp, _ := get(t, c, r.srv.URL+"/auth/xbox/start"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown provider: %d", resp.StatusCode)
	}
	if resp, _ := get(t, c, r.srv.URL+"/auth/steam/link"); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Errorf("link anonymous: %d", resp.StatusCode)
	}

	jo := browser(t)
	r.loginAs(t, jo, "jo")
	resp, _ = get(t, jo, r.srv.URL+"/auth/steam/link")
	loc, _ = url.Parse(resp.Header.Get("Location"))
	get(t, jo, r.srv.URL+"/auth/steam/callback?code=jo-steam&state="+url.QueryEscape(loc.Query().Get("state")))
	sam := browser(t)
	r.loginAs(t, sam, "sam")
	resp, _ = get(t, sam, r.srv.URL+"/auth/steam/link")
	loc, _ = url.Parse(resp.Header.Get("Location"))
	if resp, body := get(t, sam, r.srv.URL+"/auth/steam/callback?code=jo-steam&state="+url.QueryEscape(loc.Query().Get("state"))); resp.StatusCode != http.StatusConflict || !strings.Contains(body, "already linked to another member") || !strings.Contains(body, "Back to your account") {
		t.Errorf("taken: %d %s", resp.StatusCode, body)
	}
	// An htmx request that fails gets the error content, not the page, with the status.
	resp, body := post(t, sam, r.srv.URL+"/account/unlink", url.Values{"provider": {"discord"}, "subject": {"2"}}, hx)
	if resp.StatusCode != http.StatusForbidden || strings.Contains(body, "<html") || !strings.Contains(body, "Form expired") {
		t.Errorf("htmx without csrf: %d %s", resp.StatusCode, body)
	}
	if got := strings.Join(r.results, " "); !strings.Contains(got, "discord/unknown/invalid") || !strings.Contains(got, "discord/unknown/mismatch") || !strings.Contains(got, "discord/unknown/denied") || !strings.Contains(got, "steam/unknown/taken") {
		t.Errorf("observed: %s", got)
	}

	// Callbacks for providers the hub doesn't have are all reported as "other": the path is the
	// requester's, so it must not become a metric label.
	before := len(r.results)
	for i := range 50 {
		get(t, browser(t), r.srv.URL+"/auth/junk"+strconv.Itoa(i)+"/callback?code=x&state=y")
	}
	labels := map[string]bool{}
	for _, got := range r.results[before:] {
		labels[strings.SplitN(got, "/", 2)[0]] = true
	}
	if len(r.results)-before != 50 || len(labels) != 1 || !labels["other"] {
		t.Errorf("junk providers: %d results, providers %v", len(r.results)-before, labels)
	}
}

func TestLogoutEverywhere(t *testing.T) {
	r := newRig(t)
	one, two := browser(t), browser(t)
	r.loginAs(t, one, "jo")
	r.loginAs(t, two, "jo")
	if r.idSt.SessionCount() != 2 {
		t.Fatalf("sessions: %d", r.idSt.SessionCount())
	}
	_, body := get(t, one, r.srv.URL+"/account")
	resp, _ := post(t, one, r.srv.URL+"/auth/logout", url.Values{"_csrf": {csrfOf(t, body)}, "everywhere": {"1"}}, nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout everywhere: %d", resp.StatusCode)
	}
	if r.idSt.SessionCount() != 0 {
		t.Errorf("sessions left: %d", r.idSt.SessionCount())
	}
	if resp, _ := get(t, two, r.srv.URL+"/account"); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("the other browser is logged out too: %d", resp.StatusCode)
	}
	_, body = get(t, one, r.srv.URL+"/login")
	if !strings.Contains(body, "Logged out everywhere (2 sessions)") {
		t.Errorf("flash: %s", body)
	}
}

func TestThemeAndStatic(t *testing.T) {
	r := newRig(t)
	c := browser(t)
	resp, body := get(t, c, r.srv.URL+"/theme.css")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/css") || resp.Header.Get("ETag") == "" {
		t.Fatalf("theme.css: %d %q etag=%q", resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("ETag"))
	}
	if !strings.Contains(body, "--accent:#0b5fff") || !strings.Contains(body, "--bg:#151515") || !strings.Contains(body, "--font:system-ui, sans-serif") || !strings.Contains(body, "prefers-color-scheme: dark") {
		t.Errorf("theme.css should carry the host's valid tokens and the default for the bad one:\n%s", body)
	}
	if resp, _ := get(t, c, r.srv.URL+"/theme.css", "If-None-Match", resp.Header.Get("ETag")); resp.StatusCode != http.StatusNotModified {
		t.Errorf("etag revalidation: %d", resp.StatusCode)
	}
	resp, body = get(t, c, r.srv.URL+"/static/app.css")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/css") || !strings.Contains(body, "var(--accent)") || resp.Header.Get("Cache-Control") == "" {
		t.Errorf("app.css: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	resp, body = get(t, c, r.srv.URL+"/static/vendor/htmx.min.js")
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "javascript") || !strings.HasPrefix(body, "var htmx=") {
		t.Errorf("htmx: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if resp, _ := get(t, c, r.srv.URL+"/static/../web.go"); resp.StatusCode == http.StatusOK {
		t.Error("nothing outside static/ is served")
	}
}

// TestThemeFromSettings renders the pages with the theme the API serves from the Organization
// settings (the hub's wiring), instead of the test rig's static theme.
func TestThemeFromSettings(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	idSt := identitytest.NewFakeStore()
	orgSvc := org.New(&orgtest.FakeStore{}, 15*time.Minute, logger)
	o, _, err := orgSvc.EnsureBuiltin(context.Background(), "Hidden Token Gaming")
	if err != nil {
		t.Fatal(err)
	}
	ids := identity.New(idSt, o.ID, nil, 10*time.Minute, logger)
	sess := session.New(idSt, time.Hour, false, logger)
	apiMux := http.NewServeMux()
	apiMux.Handle(hubv1connect.NewOrganizationServiceHandler(api.NewOrganizationServer(orgSvc, logger)))
	apiMux.Handle(hubv1connect.NewIdentityServiceHandler(api.NewIdentityServer(ids, sess, orgSvc, logger)))
	h, err := web.New(ids, sess, sess.Middleware(apiMux), nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(sess.Middleware(mux))
	defer srv.Close()
	c := browser(t)

	_, css := get(t, c, srv.URL+"/theme.css")
	if !strings.Contains(css, "--accent:#1a7a4a") {
		t.Errorf("defaults before any settings:\n%s", css)
	}
	if _, err := orgSvc.UpdateSettings(context.Background(), org.Settings{
		Theme: org.Theme{Dark: org.Tokens{Accent: "#3ee07a", Background: "#070b17"}, Light: org.Tokens{Accent: "#0a7a3a"}, Font: "Archivo, system-ui, sans-serif", LogoURL: "https://hiddentoken.com/brand/mark.svg", FaviconURL: "https://hiddentoken.com/favicon.svg"},
		Nav:   []org.NavLink{{Label: "Discord", URL: "https://discord.gg/x"}, {Label: "Rules", URL: "https://hiddentoken.com/rules/", Placement: "footer"}, {Label: "Admin", URL: "/admin", Role: "owner"}},
	}); err != nil {
		t.Fatal(err)
	}
	_, css = get(t, c, srv.URL+"/theme.css")
	for _, want := range []string{"--accent:#0a7a3a", "--bg:#fafafa", "--font:Archivo, system-ui, sans-serif", "--accent:#3ee07a", "--bg:#070b17"} {
		if !strings.Contains(css, want) {
			t.Errorf("theme.css should carry %q (set tokens) with defaults for the rest:\n%s", want, css)
		}
	}
	_, body := get(t, c, srv.URL+"/login")
	for _, want := range []string{`src="https://hiddentoken.com/brand/mark.svg"`, `rel="icon" href="https://hiddentoken.com/favicon.svg"`, `href="https://discord.gg/x">Discord`, `href="https://hiddentoken.com/rules/">Rules`} {
		if !strings.Contains(body, want) {
			t.Errorf("login page should contain %q", want)
		}
	}
	if strings.Contains(body, "/admin") {
		t.Error("the owner-only link must not show to anonymous visitors")
	}
}
