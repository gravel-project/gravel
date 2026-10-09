package hub_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/config"
	"github.com/gravel-project/gravel/internal/hub"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/identity/identitytest"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Organization.Name = "Hidden Token Gaming"
	cfg.Database.URL = storetest.DatabaseURL(t)
	cfg.Database.ConnectTimeout = 10 * time.Second
	cfg.Server.Listen = "127.0.0.1:0"
	cfg.Server.InternalListen = "127.0.0.1:0"
	cfg.Server.ShutdownTimeout = 5 * time.Second
	return cfg
}

func fakeProviders() []identity.Registration {
	discord := &identitytest.FakeProvider{ProviderName: "discord", Accounts: map[string]identity.Account{
		"jo": {Subject: "1", DisplayName: "Jo", Method: identity.MethodOAuth2},
	}}
	return []identity.Registration{{Provider: discord, Login: true}}
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	_ = resp.Body.Close()
	return resp.StatusCode, out
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
	Cookies    []*http.Cookie
}

func get(t *testing.T, c *http.Client, u string) reply {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return reply{StatusCode: resp.StatusCode, Header: resp.Header, Cookies: resp.Cookies()}
}

// loginAs runs the fake Discord flow in the browser and returns the session cookie.
func loginAs(t *testing.T, c *http.Client, base, code string) *http.Cookie {
	t.Helper()
	resp := get(t, c, base+"/auth/discord/start")
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusSeeOther || loc.Query().Get("state") == "" {
		t.Fatalf("start: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp = get(t, c, base+"/auth/discord/callback?code="+code+"&state="+url.QueryEscape(loc.Query().Get("state")))
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/account" {
		t.Fatalf("callback: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	for _, ck := range resp.Cookies {
		if ck.Name == "gravel_session" && ck.Value != "" {
			return ck
		}
	}
	t.Fatal("no session cookie")
	return nil
}

type cookieTransport struct {
	next   http.RoundTripper
	cookie *http.Cookie
}

func (ct cookieTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.AddCookie(ct.cookie)
	return ct.next.RoundTrip(r)
}

func TestHubEndToEnd(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	cfg.Database.Migrate = config.MigrateManual
	if _, err := hub.New(ctx, hub.Options{Config: cfg, Logger: quiet()}); err == nil || !strings.Contains(err.Error(), "not current") {
		t.Fatalf("manual migrate on an empty database must refuse to start: %v", err)
	}

	cfg.Database.Migrate = config.MigrateAuto
	regs := fakeProviders()
	h, err := hub.New(ctx, hub.Options{Config: cfg, Logger: quiet(), Version: "test", Providers: regs})
	if err != nil {
		t.Fatal(err)
	}
	token := h.OwnerClaimToken()
	if token == "" {
		t.Fatal("a fresh hub must mint an owner-claim token")
	}

	public := newH2CServer(h.Handler())
	defer public.Close()
	internal := httptest.NewServer(h.InternalHandler())
	defer internal.Close()

	if code, body := getJSON(t, public.URL+"/healthz"); code != 200 || body["status"] != "ok" || body["version"] != "test" {
		t.Errorf("healthz: %d %v", code, body)
	}
	if code, body := getJSON(t, public.URL+"/readyz"); code != 200 || body["status"] != "ready" {
		t.Errorf("readyz: %d %v", code, body)
	}

	anon := hubv1connect.NewOrganizationServiceClient(h2cClient(), public.URL, connect.WithGRPC())
	if _, err := anon.ClaimOwnership(ctx, connect.NewRequest(&hubv1.ClaimOwnershipRequest{Token: token})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("a claim without a login: %v", err)
	}

	// Log in through the pages, then claim over gRPC with the session cookie.
	b := browser(t)
	cookie := loginAs(t, b, public.URL, "jo")
	authed := &http.Client{Transport: cookieTransport{next: h2cClient().Transport, cookie: cookie}}
	grpc := hubv1connect.NewOrganizationServiceClient(authed, public.URL, connect.WithGRPC())
	if _, err := grpc.ClaimOwnership(ctx, connect.NewRequest(&hubv1.ClaimOwnershipRequest{Token: "wrong"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("wrong token over gRPC: %v", err)
	}
	claimed, err := grpc.ClaimOwnership(ctx, connect.NewRequest(&hubv1.ClaimOwnershipRequest{Token: token}))
	if err != nil || !claimed.Msg.GetOrganization().GetOwned() || claimed.Msg.GetOrganization().GetOwnerUserId() == "" {
		t.Fatalf("claim over gRPC: %v %v", err, claimed)
	}
	if _, err := grpc.ClaimOwnership(ctx, connect.NewRequest(&hubv1.ClaimOwnershipRequest{Token: token})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("second claim: %v", err)
	}

	// The JSON side, with the cookie: the owner sees owner: true and their identity.
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, public.URL+"/gravel.hub.v1.IdentityService/GetMe", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var me map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&me)
	_ = resp.Body.Close()
	u, _ := me["user"].(map[string]any)
	if resp.StatusCode != 200 || u["owner"] != true || u["displayName"] != "Jo" || resp.Header.Get("X-Request-ID") == "" {
		t.Errorf("GetMe as the owner: %d %v (request id %q)", resp.StatusCode, me, resp.Header.Get("X-Request-ID"))
	}
	if ids, _ := u["identities"].([]any); len(ids) != 1 {
		t.Errorf("identities: %v", u["identities"])
	}
	if resp := get(t, b, public.URL+"/account"); resp.StatusCode != http.StatusOK {
		t.Errorf("account page: %d", resp.StatusCode)
	}

	mresp, err := http.Get(internal.URL + "/metrics") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	metrics, _ := io.ReadAll(mresp.Body)
	_ = mresp.Body.Close()
	for _, want := range []string{
		`gravel_build_info{go_version="`,
		`gravel_rpc_requests_total{code="ok",procedure="/gravel.hub.v1.OrganizationService/ClaimOwnership"} 1`,
		`gravel_rpc_requests_total{code="failed_precondition",procedure="/gravel.hub.v1.OrganizationService/ClaimOwnership"} 1`,
		`gravel_rpc_requests_total{code="unauthenticated",procedure="/gravel.hub.v1.OrganizationService/ClaimOwnership"} 1`,
		`gravel_http_requests_total{code="200",handler="healthz",method="GET"} 1`,
		`gravel_http_requests_total{code="303",handler="pages",method="GET"} 2`,
		`gravel_auth_completions_total{intent="login",provider="discord",result="ok"} 1`,
		`gravel_wal_archiver_readable 1`,
		`gravel_wal_archived_total 0`,
		`gravel_store_stats_readable 1`,
		`gravel_users 1`,
		`gravel_identities{provider="discord"} 1`,
		`gravel_database_size_bytes `,
		`go_goroutines`,
	} {
		if !strings.Contains(string(metrics), want) {
			t.Errorf("metrics should contain %s", want)
		}
	}
	h.Prune(ctx)

	// A restart of an owned hub mints nothing.
	h2, err := hub.New(ctx, hub.Options{Config: cfg, Logger: quiet(), Providers: regs})
	if err != nil {
		t.Fatal(err)
	}
	if h2.OwnerClaimToken() != "" {
		t.Error("an owned hub must not mint a token")
	}
	h2.Close()

	h.Close()
	if code, body := getJSON(t, public.URL+"/readyz"); code != http.StatusServiceUnavailable || body["status"] != "not ready" {
		t.Errorf("readyz with the pool closed: %d %v", code, body)
	}
}

func TestRateLimitsAndClientIP(t *testing.T) {
	cfg := testConfig(t)
	cfg.RateLimit.PerIP = config.Limit{RequestsPerMinute: 1, Burst: 2}
	cfg.Server.ClientIPHeader = "X-Test-IP"
	regs := fakeProviders()
	h, err := hub.New(context.Background(), hub.Options{Config: cfg, Logger: quiet(), Providers: regs})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	public := httptest.NewServer(h.Handler())
	defer public.Close()
	internal := httptest.NewServer(h.InternalHandler())
	defer internal.Close()

	hit := func(ip string) int {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, public.URL+"/login", nil)
		req.Header.Set("X-Test-IP", ip)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests && resp.Header.Get("Retry-After") == "" {
			t.Error("429 without Retry-After")
		}
		return resp.StatusCode
	}
	if a, b := hit("10.0.0.1"), hit("10.0.0.1"); a != 200 || b != 200 {
		t.Errorf("burst: %d %d", a, b)
	}
	if c := hit("10.0.0.1"); c != http.StatusTooManyRequests {
		t.Errorf("third from the same ip: %d", c)
	}
	if d := hit("10.0.0.2"); d != 200 {
		t.Errorf("another ip has its own bucket: %d", d)
	}
	for range 3 {
		if code, _ := getJSON(t, public.URL+"/healthz"); code != 200 {
			t.Errorf("probes are never limited: %d", code)
		}
	}
	rpc, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, public.URL+"/gravel.hub.v1.OrganizationService/GetOrganization", strings.NewReader("{}"))
	rpc.Header.Set("Content-Type", "application/json")
	rpc.Header.Set("X-Test-IP", "10.0.0.1")
	resp, err := http.DefaultClient.Do(rpc)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || out["code"] != "resource_exhausted" {
		t.Errorf("a procedure from the limited ip: %d %v", resp.StatusCode, out)
	}
	mresp, _ := http.Get(internal.URL + "/metrics") //nolint:noctx // test
	metrics, _ := io.ReadAll(mresp.Body)
	_ = mresp.Body.Close()
	if !strings.Contains(string(metrics), `gravel_rate_limited_total{scope="ip"} 2`) {
		t.Errorf("metrics should count both refusals:\n%s", metrics)
	}
}

func TestRunServesAndStops(t *testing.T) {
	cfg := testConfig(t)
	h, err := hub.New(context.Background(), hub.Options{Config: cfg, Logger: quiet(), Providers: []identity.Registration{}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	select {
	case <-h.Started():
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not start listening")
	}
	if code, _ := getJSON(t, "http://"+h.PublicAddr().String()+"/healthz"); code != 200 {
		t.Errorf("healthz over Run: %d", code)
	}
	if code, _ := getJSON(t, "http://"+h.InternalAddr().String()+"/metrics"); code != 200 {
		t.Errorf("metrics over Run: %d", code)
	}
	if resp := get(t, browser(t), "http://"+h.PublicAddr().String()+"/login"); resp.StatusCode != 200 {
		t.Errorf("login page with no providers still renders: %d", resp.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop")
	}
	d := net.Dialer{Timeout: time.Second}
	if _, err := d.DialContext(context.Background(), "tcp", h.PublicAddr().String()); err == nil {
		t.Error("public listener should be closed after Run returns")
	}
}

func TestRunFailsOnBusyPort(t *testing.T) {
	cfg := testConfig(t)
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	cfg.Server.Listen = ln.Addr().String()
	h, err := hub.New(context.Background(), hub.Options{Config: cfg, Logger: quiet(), Providers: []identity.Registration{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Errorf("want a listen error, got %v", err)
	}
}
