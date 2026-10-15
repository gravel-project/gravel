package hub_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/gravel-project/gravel/games/wardogs/wardogsim"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/config"
	"github.com/gravel-project/gravel/internal/hub"
	"github.com/gravel-project/gravel/internal/notify"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

// TestUnplug is H1's gate (gravel#198, ADR-0025): a hub with no Discord, no Steam, no cloud and no
// metrics store stands up, gets an owner, shows what is wrong and lets them act, and reaches for
// nothing off this machine while it does.
//
//   - hub.yaml configures no login provider, local accounts only; the hub's outbound HTTP goes
//     through a transport that refuses, and records, any dial off loopback.
//   - The host's binaries do the host's part: gravel-hub migrate and servers apply (a simulated
//     War Dogs server on loopback), then, once the hub serves, gravel-hub users create-local.
//   - The member sets their password from the link, logs in with gravel login --local and takes
//     the hub with gravel claim and the token it minted.
//   - The owner adds a webhook notifier on loopback with gravel settings apply.
//   - The fleet shows the server up; the server goes away; the fleet shows it down with an alarm,
//     the events log has server.unreachable, and the webhook receives it, signed.
//   - /admin serves the dashboard's bundle under the strict Content-Security-Policy, and the
//     metrics page's ListDashboards says no store is configured.
//
// It needs a Postgres (GRAVEL_TEST_DATABASE_URL), the admin bundle built (make admin) and a Go
// toolchain to build the two binaries: make unplug runs it, with UNPLUG=1.
func TestUnplug(t *testing.T) {
	if os.Getenv("UNPLUG") == "" {
		t.Skip("the unplug test: make unplug (UNPLUG=1, a Postgres, the admin bundle)")
	}
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	t.Cleanup(func() { storetest.Reset(t, st) })

	// Nothing leaves this machine: every outbound HTTP request the hub makes goes through the
	// default transport (its clients set none of their own), which here refuses any address but
	// loopback and remembers who tried.
	guard := &offLoopback{}
	saved := http.DefaultTransport
	http.DefaultTransport = guard.transport()
	t.Cleanup(func() {
		http.DefaultTransport = saved
		if tried := guard.tried(); len(tried) > 0 {
			t.Errorf("the hub dialled off this machine: %v", tried)
		}
	})

	bins := buildBinaries(t, "gravel-hub", "gravel")
	dir := t.TempDir()
	host := func(args ...string) string {
		t.Helper()
		return runBinary(t, filepath.Join(bins, "gravel-hub"), dir, "", args...)
	}

	// A simulated War Dogs server on loopback, its RCON password in a file only the hub reads.
	profile := wardogsim.DefaultProfile()
	profile.Timezone = "UTC"
	profile.Population = slices.Repeat([]float64{12}, 24)
	profile.RestartHour = nil
	sim, err := wardogsim.New(wardogsim.Options{Token: "sim-token", Profile: profile, Seed: 1, Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	simSrv := httptest.NewServer(sim.Handler())
	t.Cleanup(simSrv.Close)
	simCtx, stopSim := context.WithCancel(ctx)
	t.Cleanup(stopSim)
	go func() { _ = sim.Run(simCtx) }()
	cred := filepath.Join(dir, "rcon")
	writeTestFile(t, cred, "sim-token\n")

	// A webhook on loopback, as an operator's own endpoint: the notifier with no Discord in the
	// path. It checks each delivery's signature and keeps the event kinds.
	const hookKey = "an operator's webhook signing key"
	hook := &receiver{key: hookKey}
	hookSrv := httptest.NewServer(hook)
	t.Cleanup(hookSrv.Close)
	writeTestFile(t, filepath.Join(dir, "hook-key"), hookKey+"\n")

	// hub.yaml: local accounts and the webhook's key, nothing else. No provider, no cloud, no
	// metrics store, no demo bucket.
	cfgPath := filepath.Join(dir, "hub.yaml")
	writeTestFile(t, cfgPath, fmt.Sprintf(`version: 1
organization:
  name: Unplugged
server:
  listen: 127.0.0.1:0
  internal_listen: 127.0.0.1:0
database:
  url: %s
auth:
  local:
    enabled: true
notify:
  secrets:
    ops:
      file: %s
`, storetest.DatabaseURL(t), filepath.Join(dir, "hook-key")))
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(dir, "servers.yaml")
	writeTestFile(t, manifest, `version: 1
games:
  - id: wardogs
servers:
  - id: unplugged-1
    name: "Unplugged #1"
    game: wardogs
    driver: wardogs
    location: here
    endpoint: `+simSrv.URL+`
    credential_file: `+cred+`
    poll_interval: 5s
    trust: official
`)
	host("migrate", "--config", cfgPath)
	host("servers", "apply", manifest, "--config", cfgPath)

	// Serve, with the providers the configuration describes: none.
	h, err := hub.New(ctx, hub.Options{Config: cfg, Logger: quiet(), Version: "unplug"})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- h.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	<-h.Started()
	base := "http://" + h.PublicAddr().String()
	token := h.OwnerClaimToken()
	if token == "" {
		t.Fatal("an unowned hub minted no claim token")
	}

	// The login page offers a username and password, and no provider.
	page := fetch(t, http.DefaultClient, base+"/login")
	if !strings.Contains(page, `action="/auth/local/login"`) || strings.Contains(page, "/auth/discord") || strings.Contains(page, "/auth/steam") {
		t.Errorf("the login page of a hub without providers:\n%s", page)
	}

	// The host makes the first member; the member sets their password from the link.
	out := host("users", "create-local", "--username", "olive", "--config", cfgPath)
	link := regexp.MustCompile(`/auth/local/reset/[A-Za-z0-9_-]+`).FindString(out)
	if link == "" {
		t.Fatalf("users create-local printed no link:\n%s", out)
	}
	const password = "correct horse battery staple"
	browser := newBrowser(t)
	resp, err := browser.PostForm(base+link, url.Values{"password": {password}}) //nolint:noctx // a test
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.Request.URL.Path != "/account" {
		t.Fatalf("setting the password ended at %s (%d)", resp.Request.URL, resp.StatusCode)
	}

	// The CLI logs in with the password and claims the hub with the token it minted.
	home := t.TempDir()
	cli := func(stdin string, args ...string) string {
		t.Helper()
		return runBinary(t, filepath.Join(bins, "gravel"), home, stdin, args...)
	}
	cli(password+"\n", "login", "--local", "--username", "olive", "--hub", base)
	if out := cli("", "claim", "--hub", base, token); !strings.Contains(out, "You own Unplugged") {
		t.Fatalf("gravel claim: %s", out)
	}
	if out := cli("", "whoami", "--hub", base); !strings.Contains(out, "olive") {
		t.Errorf("gravel whoami after the claim: %s", out)
	}

	// The owner adds the notifier with the CLI, as a manifest.
	notifiers := filepath.Join(dir, "settings.yaml")
	writeTestFile(t, notifiers, "version: 1\nnotifiers:\n  - name: ops\n    kind: webhook\n    url: "+hookSrv.URL+"\n")
	cli("", "settings", "apply", notifiers, "--hub", base)

	// The fleet: the server comes up.
	fleet := hubv1connect.NewServerServiceClient(browser, base)
	summary := func() *hubv1.GetFleetSummaryResponse {
		t.Helper()
		r, err := fleet.GetFleetSummary(ctx, connect.NewRequest(&hubv1.GetFleetSummaryRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		return r.Msg
	}
	health := func(s *hubv1.GetFleetSummaryResponse) string {
		for _, x := range s.GetServers() {
			if x.GetId() == "unplugged-1" {
				return x.GetHealth()
			}
		}
		return "absent"
	}
	eventually(t, 30*time.Second, "the server up", func() bool { return health(summary()) == "up" })

	// The server goes away: down within a few polls, then, down for servers.DownAlarmAfter, an
	// alarm; and an event in the log.
	simSrv.Close()
	stopSim()
	eventually(t, 30*time.Second, "the server down", func() bool { return health(summary()) == "down" })
	var last *hubv1.GetFleetSummaryResponse
	alarmed := func() bool {
		last = summary()
		return slices.ContainsFunc(last.GetAlarms(), func(a *hubv1.FleetAlarm) bool {
			return a.GetServerId() == "unplugged-1" && a.GetKind() == "server.down" && a.GetSeverity() == "critical"
		})
	}
	if !poll(servers.DownAlarmAfter+time.Minute, 2*time.Second, alarmed) {
		t.Fatalf("no server.down alarm %s after the server went down: %v", servers.DownAlarmAfter+time.Minute, last)
	}
	// By now the webhook has had the warning, signed.
	eventually(t, 30*time.Second, "server.unreachable at the webhook", func() bool { return slices.Contains(hook.got(), "server.unreachable") })
	if bad := hook.refused(); bad > 0 {
		t.Errorf("%d deliveries to the webhook had a bad signature", bad)
	}
	orgs := hubv1connect.NewOrganizationServiceClient(browser, base)
	eventually(t, 15*time.Second, "server.unreachable in the events log", func() bool {
		r, err := orgs.ListEvents(ctx, connect.NewRequest(&hubv1.ListEventsRequest{Kinds: []string{"server.unreachable"}, ServerId: "unplugged-1"}))
		return err == nil && len(r.Msg.GetEvents()) > 0
	})

	// The dashboard: the bundle, under the pages' strict policy.
	resp, err = browser.Get(base + "/admin") //nolint:noctx // a test
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusServiceUnavailable {
		t.Fatal("/admin: this binary was built without the dashboard; make admin first (make unplug does)")
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `src="/admin/assets/`) || !strings.Contains(csp, "default-src 'none'") ||
		!strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Errorf("/admin: %d, policy %q, body:\n%s", resp.StatusCode, csp, body)
	}
	asset := regexp.MustCompile(`src="(/admin/assets/[^"]+\.js)"`).FindStringSubmatch(string(body))
	if asset == nil {
		t.Fatalf("/admin names no script")
	}
	if js := fetch(t, browser, base+asset[1]); len(js) < 1000 {
		t.Errorf("%s is %d bytes", asset[1], len(js))
	}

	// Every service of the API is reachable on the public listener: a procedure the dashboard
	// calls and the router doesn't forward is a 404 in production that no fake catches.
	protoregistry.GlobalFiles.RangeFilesByPackage("gravel.hub.v1", func(fd protoreflect.FileDescriptor) bool {
		for i := range fd.Services().Len() {
			sd := fd.Services().Get(i)
			proc := "/" + string(sd.FullName()) + "/" + string(sd.Methods().Get(0).Name())
			resp, err := browser.Post(base+proc, "application/json", strings.NewReader("{}")) //nolint:noctx // a test
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusNotFound && resp.Header.Get("Content-Type") != "application/json" {
				t.Errorf("%s is not routed on the public listener", sd.FullName())
			}
		}
		return true
	})

	// Metrics: no store is configured, and the page says so rather than failing.
	dash, err := hubv1connect.NewMetricsServiceClient(browser, base).ListDashboards(ctx, connect.NewRequest(&hubv1.ListDashboardsRequest{}))
	if err != nil || dash.Msg.GetAvailable() {
		t.Errorf("ListDashboards without a store: %v %v", dash, err)
	}
}

// receiver is an operator's webhook: it verifies each delivery and keeps the events' kinds.
type receiver struct {
	key  string
	mu   sync.Mutex
	kind []string
	bad  int
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := notify.Verify(r.key, req.Header, body, time.Now(), 5*time.Minute); err != nil {
		r.bad++
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	var b notify.WebhookBody
	_ = json.Unmarshal(body, &b)
	for _, e := range b.Events {
		r.kind = append(r.kind, e.Kind)
	}
}

func (r *receiver) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.kind)
}

func (r *receiver) refused() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bad
}

// offLoopback is a transport that dials loopback only, and records every other address asked for.
type offLoopback struct {
	mu   sync.Mutex
	seen []string
}

func (g *offLoopback) transport() http.RoundTripper {
	d := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			h, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			if ip := net.ParseIP(h); h == "localhost" || (ip != nil && ip.IsLoopback()) {
				return d.DialContext(ctx, network, addr)
			}
			g.mu.Lock()
			g.seen = append(g.seen, addr)
			g.mu.Unlock()
			return nil, errors.New("the unplug test refuses a dial off this machine: " + addr)
		},
	}
}

func (g *offLoopback) tried() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.seen)
}

// buildBinaries builds the named commands of this module into a directory.
func buildBinaries(t *testing.T, names ...string) string {
	t.Helper()
	root, err := exec.CommandContext(t.Context(), "go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Fatalf("go list -m: %v", err)
	}
	out := t.TempDir()
	for _, n := range names {
		cmd := exec.CommandContext(t.Context(), "go", "build", "-o", filepath.Join(out, n), "./cmd/"+n)
		cmd.Dir = strings.TrimSpace(string(root))
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build ./cmd/%s: %v\n%s", n, err, b)
		}
	}
	return out
}

// runBinary runs a built command with home as its HOME and configuration directory, and fails
// the test on a non-zero exit.
func runBinary(t *testing.T, bin, home, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), bin, args...)
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"), "GRAVEL_HUB=")
	cmd.Stdin = strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %s: %v\n%s%s", filepath.Base(bin), strings.Join(args, " "), err, out.String(), errOut.String())
	}
	return out.String()
}

func newBrowser(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar}
}

func fetch(t *testing.T, c *http.Client, u string) string {
	t.Helper()
	resp, err := c.Get(u) //nolint:noctx // a test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d\n%s", u, resp.StatusCode, b)
	}
	return string(b)
}

// poll asks ok every interval until it holds or limit passes, and says whether it held.
func poll(limit, interval time.Duration, ok func() bool) bool {
	deadline := time.Now().Add(limit)
	for !ok() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(interval)
	}
	return true
}

// eventually polls every half second: the API calls it makes stay under the per-user rate limit.
func eventually(t *testing.T, limit time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("waited %s for %s", limit, what)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
