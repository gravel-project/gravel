package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/")

// t0 is the fake hub's clock.
var t0 = time.Date(2026, 10, 14, 2, 0, 0, 0, time.UTC)

// fakeHub is a hub's API as the CLI sees it: the login form, the token endpoint and the
// procedures the commands call, over Connect. Every API request must be JSON (principle 4).
type fakeHub struct {
	hubv1connect.UnimplementedIdentityServiceHandler
	hubv1connect.UnimplementedOrganizationServiceHandler
	hubv1connect.UnimplementedServerServiceHandler

	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	passwords  map[string]string // username → password
	sessions   map[string]string // cookie value → username
	owner      string
	claimToken string
	clientID   string
	secret     string
	events     []*hubv1.Event // oldest first; ids ascending
	listCalls  []*hubv1.ListEventsRequest
	fleet      *hubv1.GetFleetSummaryResponse
	servers    []*hubv1.Server
	logins     int
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	h := &fakeHub{
		t:          t,
		passwords:  map[string]string{"alice": "correct horse"},
		sessions:   map[string]string{},
		claimToken: "claim-me",
		clientID:   "htg-bot",
		secret:     "s3cret",
	}
	mux := http.NewServeMux()
	mux.Handle(hubv1connect.NewIdentityServiceHandler(h))
	mux.Handle(hubv1connect.NewOrganizationServiceHandler(h))
	mux.Handle(hubv1connect.NewServerServiceHandler(h))
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s came as %q: the CLI speaks Connect's JSON only", r.URL.Path, ct)
			http.Error(w, "json only", http.StatusUnsupportedMediaType)
			return
		}
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "gravel/") {
			t.Errorf("%s came without gravel's user agent: %q", r.URL.Path, r.Header.Get("User-Agent"))
		}
		mux.ServeHTTP(w, r)
	})
	top := http.NewServeMux()
	top.Handle("/", api)
	top.HandleFunc("POST /auth/local/login", h.localLogin)
	top.HandleFunc("POST /oauth/token", h.token)
	h.srv = httptest.NewServer(top)
	t.Cleanup(h.srv.Close)
	return h
}

func (h *fakeHub) localLogin(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.logins++
	u, p := r.PostFormValue("username"), r.PostFormValue("password")
	if want, ok := h.passwords[u]; !ok || want != p {
		http.Error(w, "<html>refused</html>", http.StatusUnauthorized)
		return
	}
	v := fmt.Sprintf("sess-%d", h.logins)
	h.sessions[v] = u
	http.SetCookie(w, &http.Cookie{Name: "gravel_last_provider", Value: "local", Path: "/"})
	http.SetCookie(w, &http.Cookie{Name: "gravel_session", Value: v, Path: "/", HttpOnly: true})
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

func (h *fakeHub) token(w http.ResponseWriter, r *http.Request) {
	id, secret, ok := r.BasicAuth()
	if !ok || id != h.clientID || secret != h.secret {
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"access_token":"app-token","token_type":"Bearer","expires_in":3600}`))
}

// who is the caller: a member's username, "app:<id>", or "".
func (h *fakeHub) who(hdr http.Header) string {
	if hdr.Get("Authorization") == "Bearer app-token" {
		return "app:" + h.clientID
	}
	cookies, err := http.ParseCookie(hdr.Get("Cookie"))
	if err != nil {
		return ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range cookies {
		if c.Name == "gravel_session" {
			return h.sessions[c.Value]
		}
	}
	return ""
}

func (h *fakeHub) member(hdr http.Header) (string, error) {
	who := h.who(hdr)
	if who == "" || strings.HasPrefix(who, "app:") {
		return "", connect.NewError(connect.CodeUnauthenticated, errors.New("log in first"))
	}
	return who, nil
}

func (h *fakeHub) GetMe(_ context.Context, req *connect.Request[hubv1.GetMeRequest]) (*connect.Response[hubv1.GetMeResponse], error) {
	who, err := h.member(req.Header())
	if err != nil {
		return nil, err
	}
	u := &hubv1.User{
		Id: "8d4c0f5e-0000-4000-8000-000000000001", DisplayName: "Alice", Owner: h.owner == who,
		Roles:        []string{"admin"},
		Capabilities: []string{"events:read", "servers:read"},
		Grants:       []*hubv1.RoleGrant{{Role: "admin"}, {Role: "operator", GameId: "cs2"}},
		Identities:   []*hubv1.Identity{{Provider: "local", Subject: who}, {Provider: "discord", Subject: "123", DisplayName: "alice#0"}},
	}
	return connect.NewResponse(&hubv1.GetMeResponse{User: u}), nil
}

func (h *fakeHub) Logout(_ context.Context, req *connect.Request[hubv1.LogoutRequest]) (*connect.Response[hubv1.LogoutResponse], error) {
	if _, err := h.member(req.Header()); err != nil {
		return nil, err
	}
	cookies, _ := http.ParseCookie(req.Header().Get("Cookie"))
	h.mu.Lock()
	for _, c := range cookies {
		delete(h.sessions, c.Value)
	}
	h.mu.Unlock()
	return connect.NewResponse(&hubv1.LogoutResponse{}), nil
}

func (h *fakeHub) ClaimOwnership(_ context.Context, req *connect.Request[hubv1.ClaimOwnershipRequest]) (*connect.Response[hubv1.ClaimOwnershipResponse], error) {
	who, err := h.member(req.Header())
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.owner != "":
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("already owned"))
	case req.Msg.GetToken() != h.claimToken:
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("the owner-claim token is invalid or has expired"))
	}
	h.owner = who
	return connect.NewResponse(&hubv1.ClaimOwnershipResponse{Organization: &hubv1.Organization{Name: "Hidden Token Gaming", Owned: true}}), nil
}

func (h *fakeHub) ListEvents(_ context.Context, req *connect.Request[hubv1.ListEventsRequest]) (*connect.Response[hubv1.ListEventsResponse], error) {
	if h.who(req.Header()) == "" {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("log in first"))
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.listCalls = append(h.listCalls, req.Msg)
	q := req.Msg
	limit := int(q.GetLimit())
	if limit == 0 {
		limit = 100
	}
	var match []*hubv1.Event
	for _, e := range h.events {
		if len(q.GetKinds()) > 0 && !kindMatches(q.GetKinds(), e.GetKind()) {
			continue
		}
		if q.GetServerId() != "" && e.GetServerId() != q.GetServerId() {
			continue
		}
		match = append(match, e)
	}
	res := &hubv1.ListEventsResponse{}
	if n := len(h.events); n > 0 {
		res.HeadId = h.events[n-1].GetId()
	}
	if q.GetAfterId() > 0 { // oldest first
		res.NextAfterId = q.GetAfterId()
		for _, e := range match {
			if e.GetId() > q.GetAfterId() && len(res.Events) < limit {
				res.Events = append(res.Events, e)
				res.NextAfterId = e.GetId()
			}
		}
		return connect.NewResponse(res), nil
	}
	for i := len(match) - 1; i >= 0; i-- { // newest first
		e := match[i]
		if q.GetBeforeId() > 0 && e.GetId() >= q.GetBeforeId() {
			continue
		}
		if len(res.Events) == limit {
			res.NextBeforeId = res.Events[len(res.Events)-1].GetId()
			break
		}
		res.Events = append(res.Events, e)
	}
	if n := len(res.Events); n > 0 {
		res.NextAfterId = res.Events[0].GetId()
	}
	return connect.NewResponse(res), nil
}

func kindMatches(kinds []string, kind string) bool {
	for _, k := range kinds {
		if k == kind || strings.HasPrefix(kind, k+".") {
			return true
		}
	}
	return false
}

func (h *fakeHub) GetFleetSummary(_ context.Context, req *connect.Request[hubv1.GetFleetSummaryRequest]) (*connect.Response[hubv1.GetFleetSummaryResponse], error) {
	if _, err := h.member(req.Header()); err != nil && h.who(req.Header()) == "" {
		return nil, err
	}
	return connect.NewResponse(h.fleet), nil
}

func (h *fakeHub) ListServers(context.Context, *connect.Request[hubv1.ListServersRequest]) (*connect.Response[hubv1.ListServersResponse], error) {
	return connect.NewResponse(&hubv1.ListServersResponse{Servers: h.servers}), nil
}

func (h *fakeHub) GetServerStatus(_ context.Context, req *connect.Request[hubv1.GetServerStatusRequest]) (*connect.Response[hubv1.GetServerStatusResponse], error) {
	for _, s := range h.servers {
		if s.GetId() == req.Msg.GetServerId() {
			return connect.NewResponse(&hubv1.GetServerStatusResponse{Server: s}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, errors.New("not found"))
}

// cli runs gravel with a fresh credentials file per test unless the test shares one.
type cli struct {
	t     *testing.T
	stdin string
}

func (c cli) run(ctx context.Context, args ...string) (code int, stdout, stderr string) {
	c.t.Helper()
	var out, errb bytes.Buffer
	code = run(ctx, args, strings.NewReader(c.stdin), &out, &errb)
	return code, out.String(), errb.String()
}

// isolate points the credentials file at a temporary directory and clears GRAVEL_HUB.
func isolate(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gravel", "credentials")
	t.Setenv("GRAVEL_CREDENTIALS", path)
	t.Setenv("GRAVEL_HUB", "")
	return path
}

// golden compares out, with the hub's random address replaced by HUB, to testdata/name.golden.
func golden(t *testing.T, h *fakeHub, name, out string) {
	t.Helper()
	out = strings.ReplaceAll(out, h.srv.URL, "HUB")
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./cmd/gravel -update to write it)", err)
	}
	if string(want) != out {
		t.Errorf("%s differs; got:\n%s\nwant:\n%s", path, out, want)
	}
}

func login(t *testing.T, h *fakeHub) {
	t.Helper()
	code, _, stderr := cli{t: t, stdin: "alice\ncorrect horse\n"}.run(context.Background(), "login", "--local", "--hub", h.srv.URL)
	if code != exitOK {
		t.Fatalf("login: exit %d: %s", code, stderr)
	}
}

func TestLoginLocalStoresTheSessionOnly0600(t *testing.T) {
	path := isolate(t)
	h := newFakeHub(t)
	code, stdout, stderr := cli{t: t, stdin: "alice\ncorrect horse\n"}.run(context.Background(), "login", "--local", "--hub", h.srv.URL)
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if want := "Logged in to " + h.srv.URL + " as Alice.\n"; stdout != want {
		t.Errorf("stdout %q, want %q", stdout, want)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("credentials file mode %04o, want 0600", fi.Mode().Perm())
	}
	if di, err := os.Stat(filepath.Dir(path)); err != nil || di.Mode().Perm() != 0o700 {
		t.Errorf("credentials dir: %v, mode %04o, want 0700", err, di.Mode().Perm())
	}
	creds, err := loadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	want := credential{HubURL: h.srv.URL, Kind: credentialSession, Cookie: "gravel_session=sess-1"}
	if got, ok := creds.find(h.srv.URL); !ok || got != want || creds.Current != h.srv.URL {
		t.Errorf("stored %+v (current %q), want %+v", got, creds.Current, want)
	}
	if b, _ := os.ReadFile(path); bytes.Contains(b, []byte("correct horse")) {
		t.Error("the password reached the credentials file")
	}
}

func TestLoginLocalRefused(t *testing.T) {
	path := isolate(t)
	h := newFakeHub(t)
	code, _, stderr := cli{t: t, stdin: "alice\nwrong\n"}.run(context.Background(), "login", "--local", "--hub", h.srv.URL, "--username", "alice")
	if code != exitFailed || !strings.Contains(stderr, "don't match") {
		t.Errorf("exit %d, stderr %q; want 1 and the refusal", code, stderr)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused login wrote the credentials file: %v", err)
	}
}

func TestLoginLocalAnswers(t *testing.T) {
	isolate(t)
	cases := []struct {
		status int
		header string
		want   string
	}{
		{http.StatusNotFound, "", "has no username and password login"},
		{http.StatusTooManyRequests, "42", "wait 42 seconds"},
		{http.StatusOK, "", "answered the login with 200 OK"},
		{http.StatusSeeOther, "", "set no session"},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if tc.header != "" {
				w.Header().Set("Retry-After", tc.header)
			}
			if tc.status == http.StatusSeeOther {
				w.Header().Set("Location", "/account")
			}
			w.WriteHeader(tc.status)
		}))
		code, _, stderr := cli{t: t, stdin: "alice\npw\n"}.run(context.Background(), "login", "--local", "--hub", srv.URL)
		srv.Close()
		if code != exitFailed || !strings.Contains(stderr, tc.want) {
			t.Errorf("%d: exit %d, stderr %q; want 1 and %q", tc.status, code, stderr, tc.want)
		}
	}
}

func TestWhoamiAndLogout(t *testing.T) {
	path := isolate(t)
	h := newFakeHub(t)
	login(t, h)
	code, stdout, stderr := cli{t: t}.run(context.Background(), "whoami")
	if code != exitOK {
		t.Fatalf("whoami: exit %d: %s", code, stderr)
	}
	golden(t, h, "whoami", stdout)

	code, stdout, stderr = cli{t: t}.run(context.Background(), "logout")
	if code != exitOK || stdout != "Logged out of "+h.srv.URL+".\n" {
		t.Fatalf("logout: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if len(h.sessions) != 0 {
		t.Errorf("the hub still has sessions %v after logout", h.sessions)
	}
	creds, err := loadCredentials(path)
	if err != nil || len(creds.Credentials) != 0 || creds.Current != "" {
		t.Errorf("after logout the store is %+v (%v), want empty", creds, err)
	}
	if code, _, stderr := (cli{t: t}).run(context.Background(), "whoami", "--hub", h.srv.URL); code != exitLoggedIn || !strings.Contains(stderr, "not logged in") {
		t.Errorf("whoami after logout: exit %d, %q; want 3", code, stderr)
	}
}

func TestEndedSessionExits3(t *testing.T) {
	isolate(t)
	h := newFakeHub(t)
	login(t, h)
	h.mu.Lock()
	clear(h.sessions) // expired, or logged out everywhere from a browser
	h.mu.Unlock()
	for _, args := range [][]string{{"whoami"}, {"fleet"}, {"events"}, {"claim", "x"}} {
		code, _, stderr := cli{t: t}.run(context.Background(), args...)
		if code != exitLoggedIn || !strings.Contains(stderr, "session on "+h.srv.URL+" has ended") {
			t.Errorf("%v: exit %d, stderr %q; want 3 and the session ended", args, code, stderr)
		}
	}
}

func TestNoHubNoCredential(t *testing.T) {
	isolate(t)
	if code, _, stderr := (cli{t: t}).run(context.Background(), "whoami"); code != exitFailed || !strings.Contains(stderr, "no hub") {
		t.Errorf("exit %d, %q; want 1 and no hub", code, stderr)
	}
	h := newFakeHub(t)
	t.Setenv("GRAVEL_HUB", h.srv.URL)
	if code, _, stderr := (cli{t: t}).run(context.Background(), "fleet"); code != exitLoggedIn || !strings.Contains(stderr, "not logged in to "+h.srv.URL) {
		t.Errorf("exit %d, %q; want 3 and not logged in", code, stderr)
	}
}

func TestCredentialsReadableByOthersRefused(t *testing.T) {
	path := isolate(t)
	h := newFakeHub(t)
	login(t, h)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := cli{t: t}.run(context.Background(), "whoami")
	if code != exitFailed || !strings.Contains(stderr, "readable by others") {
		t.Errorf("exit %d, %q; want 1 and the mode refused", code, stderr)
	}
}

func TestTwoHubs(t *testing.T) {
	path := isolate(t)
	a, b := newFakeHub(t), newFakeHub(t)
	login(t, a)
	login(t, b)
	creds, _ := loadCredentials(path)
	if creds.Current != b.srv.URL || len(creds.Credentials) != 2 {
		t.Fatalf("store %+v; want both hubs, b current", creds)
	}
	t.Setenv("GRAVEL_HUB", a.srv.URL) // the environment beats the current hub
	if code, _, stderr := (cli{t: t}).run(context.Background(), "logout"); code != exitOK {
		t.Fatalf("logout: %d %s", code, stderr)
	}
	creds, _ = loadCredentials(path)
	if _, ok := creds.find(a.srv.URL); ok || creds.Current != b.srv.URL {
		t.Errorf("after logging out of a: %+v", creds)
	}
	if len(a.sessions) != 0 || len(b.sessions) != 1 {
		t.Errorf("sessions: a %v, b %v; want a's ended only", a.sessions, b.sessions)
	}
}

func TestClaim(t *testing.T) {
	isolate(t)
	h := newFakeHub(t)
	login(t, h)
	if code, _, stderr := (cli{t: t}).run(context.Background(), "claim", "nope"); code != exitFailed || !strings.Contains(stderr, "refused the token") {
		t.Errorf("wrong token: exit %d, %q", code, stderr)
	}
	code, stdout, stderr := cli{t: t}.run(context.Background(), "claim", "claim-me")
	if code != exitOK || stdout != "You own Hidden Token Gaming ("+h.srv.URL+") now.\n" {
		t.Errorf("claim: exit %d, %q, %q", code, stdout, stderr)
	}
	if h.owner != "alice" {
		t.Errorf("owner %q, want alice", h.owner)
	}
	if code, _, stderr := (cli{t: t}).run(context.Background(), "claim", "claim-me"); code != exitFailed || !strings.Contains(stderr, "already has an owner") {
		t.Errorf("second claim: exit %d, %q", code, stderr)
	}
}

func fleetFixture(down int32) *hubv1.GetFleetSummaryResponse {
	ts := func(d time.Duration) *timestamppb.Timestamp { return timestamppb.New(t0.Add(-d)) }
	f := &hubv1.GetFleetSummaryResponse{
		AsOf:   timestamppb.New(t0),
		Totals: &hubv1.FleetTotals{Servers: 3, Up: 1, Down: down, Idle: 1, Unknown: 1 - down, Players: 12, Capacity: 64},
		Servers: []*hubv1.FleetServer{
			{Id: "htg-wardogs-1", GameId: "wardogs", Health: "up", Reason: "answering", HealthSince: ts(3 * time.Hour), Players: 12, MaxPlayers: 64, Map: "Ozeti", ObservedAt: ts(5 * time.Second)},
			{Id: "htg-cs2-1", GameId: "cs2", Health: "idle", Reason: "parked", HealthSince: ts(24 * time.Hour)},
		},
	}
	if down > 0 {
		f.Alarms = []*hubv1.FleetAlarm{{Kind: "server.down", Severity: "critical", ServerId: "htg-wardogs-2", Since: ts(10 * time.Minute), Detail: "not answering for 10 minutes"}}
		f.Servers = append([]*hubv1.FleetServer{{Id: "htg-wardogs-2", GameId: "wardogs", Health: "down", Reason: "not answering", HealthSince: ts(10 * time.Minute)}}, f.Servers...)
	} else {
		f.Servers = append(f.Servers, &hubv1.FleetServer{Id: "htg-wardogs-2", GameId: "wardogs", Health: "unknown", Reason: "not polled yet"})
	}
	return f
}

func TestFleet(t *testing.T) {
	isolate(t)
	h := newFakeHub(t)
	login(t, h)
	h.fleet = fleetFixture(1)
	code, stdout, stderr := cli{t: t}.run(context.Background(), "fleet")
	if code != exitFailed {
		t.Errorf("a server down: exit %d (%s), want 1", code, stderr)
	}
	golden(t, h, "fleet-down", stdout)
	h.fleet = fleetFixture(0)
	code, stdout, stderr = cli{t: t}.run(context.Background(), "fleet")
	if code != exitOK {
		t.Errorf("nothing down: exit %d (%s), want 0", code, stderr)
	}
	golden(t, h, "fleet-up", stdout)
}

func serversFixture() []*hubv1.Server {
	return []*hubv1.Server{
		{Id: "htg-wardogs-1", Name: "HTG War Dogs #1", GameId: "wardogs", Location: "qonzer-slc", Trust: "official", Driver: "wardogs", Status: &hubv1.ServerStatus{
			State: "ok", Reachable: true, ObservedAt: timestamppb.New(t0), Build: "1.4.2", Players: 12, MaxPlayers: 64, Map: "Ozeti",
			RotationMap: "Europe", NextRotationMap: "Pacific", Capabilities: []string{"status", "players", "kick"},
			Teams: []*hubv1.TeamScore{{Name: "Blue", Score: 120}, {Name: "Red", Score: 95}},
			Join:  &hubv1.ServerJoin{Method: "id", Value: "4411", Hint: "Join by ID in the server browser"},
		}},
		{Id: "htg-cs2-1", Name: "HTG CS2", GameId: "cs2", Trust: "official", Driver: "cloudvm", Status: &hubv1.ServerStatus{State: "parked"}},
	}
}

func TestServers(t *testing.T) {
	isolate(t)
	h := newFakeHub(t)
	h.servers = serversFixture()
	t.Setenv("GRAVEL_HUB", h.srv.URL) // public: no login needed
	code, stdout, stderr := cli{t: t}.run(context.Background(), "servers", "list")
	if code != exitOK {
		t.Fatalf("list: exit %d: %s", code, stderr)
	}
	golden(t, h, "servers-list", stdout)
	code, stdout, stderr = cli{t: t}.run(context.Background(), "servers", "status", "htg-wardogs-1")
	if code != exitOK {
		t.Fatalf("status: exit %d: %s", code, stderr)
	}
	golden(t, h, "servers-status", stdout)
	if code, _, stderr := (cli{t: t}).run(context.Background(), "servers", "status", "nope"); code != exitFailed || !strings.Contains(stderr, `no server "nope"`) {
		t.Errorf("unknown server: exit %d, %q", code, stderr)
	}
}

func eventsFixture(n int) []*hubv1.Event {
	kinds := []string{"server.unreachable", "server.recovered", "role.granted"}
	sev := []string{"warning", "notice", "notice"}
	var out []*hubv1.Event
	for i := range n {
		e := &hubv1.Event{Id: int64(i + 1), At: timestamppb.New(t0.Add(time.Duration(i-n) * time.Minute)), Kind: kinds[i%3], Severity: sev[i%3], Summary: fmt.Sprintf("event %d", i+1)}
		if i%3 != 2 {
			e.ServerId = "htg-wardogs-1"
		}
		out = append(out, e)
	}
	return out
}

func TestEventsBacklog(t *testing.T) {
	isolate(t)
	h := newFakeHub(t)
	login(t, h)
	h.events = eventsFixture(30)
	code, stdout, stderr := cli{t: t}.run(context.Background(), "events", "--limit", "5")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	golden(t, h, "events-limit", stdout)

	now = func() time.Time { return t0 }
	t.Cleanup(func() { now = time.Now })
	code, stdout, stderr = cli{t: t}.run(context.Background(), "events", "--since", "7m", "--kind", "server")
	if code != exitOK {
		t.Fatalf("--since: exit %d: %s", code, stderr)
	}
	golden(t, h, "events-since", stdout)

	// --since pages back past one page of 500.
	h.events = eventsFixture(1200)
	code, stdout, _ = cli{t: t}.run(context.Background(), "events", "--since", "1000m")
	if lines := strings.Count(stdout, "\n"); code != exitOK || lines != 1000 {
		t.Errorf("--since 1000m over 1200 events: exit %d, %d lines; want 1000", code, lines)
	}
	if !strings.HasPrefix(stdout, "2026-10-13T09:20:00Z") || !strings.Contains(stdout, "event 1200\n") {
		t.Errorf("--since 1000m: not oldest first from 1000 minutes ago:\n%.200s", stdout)
	}
}

func TestEventsFollowResumesFromHead(t *testing.T) {
	isolate(t)
	h := newFakeHub(t)
	login(t, h)
	h.events = eventsFixture(3)
	followEvery = 10 * time.Millisecond
	t.Cleanup(func() { followEvery = 5 * time.Second })

	ctx, cancel := context.WithCancel(context.Background())
	var out syncBuffer
	done := make(chan int)
	go func() {
		done <- run(ctx, []string{"events", "--follow", "--limit", "0"}, strings.NewReader(""), &out, &out)
	}()

	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.listCalls) >= 2 })
	h.mu.Lock()
	if got := h.listCalls[1].GetAfterId(); got != 3 {
		t.Errorf("the first follow asked after %d, want the head 3", got)
	}
	h.events = append(h.events, &hubv1.Event{Id: 4, At: timestamppb.New(t0), Kind: "server.down", Severity: "critical", ServerId: "htg-wardogs-2", Summary: "down"})
	h.mu.Unlock()
	waitFor(t, func() bool { return strings.Contains(out.String(), "server.down") })
	cancel()
	if code := <-done; code != exitOK {
		t.Errorf("exit %d after interrupt, want 0", code)
	}
	if got := out.String(); strings.Count(got, "\n") != 1 || strings.Contains(got, "event 3") {
		t.Errorf("follow printed %q; want only the new event", got)
	}
}

func TestLoginApp(t *testing.T) {
	path := isolate(t)
	h := newFakeHub(t)
	h.servers = serversFixture()
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("s3cret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"login", "--app", "--client-id", "htg-bot", "--secret-file", secret, "--hub", h.srv.URL}
	if code, _, stderr := (cli{t: t}).run(context.Background(), args...); code != exitFailed || !strings.Contains(stderr, "readable by others") {
		t.Errorf("a secret others can read: exit %d, %q", code, stderr)
	}
	if err := os.Chmod(secret, 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := cli{t: t}.run(context.Background(), args...)
	if code != exitOK || stdout != "Logged in to "+h.srv.URL+" as the app htg-bot.\n" {
		t.Fatalf("exit %d, %q, %q", code, stdout, stderr)
	}
	b, _ := os.ReadFile(path)
	if bytes.Contains(b, []byte("s3cret")) {
		t.Error("the app's secret reached the credentials file")
	}
	code, stdout, stderr = cli{t: t}.run(context.Background(), "whoami")
	if code != exitOK {
		t.Fatalf("whoami: exit %d: %s", code, stderr)
	}
	if want := "app     htg-bot\n"; !strings.Contains(stdout, want) {
		t.Errorf("whoami %q lacks %q", stdout, want)
	}
	if code, _, stderr := (cli{t: t}).run(context.Background(), "claim", "claim-me"); code != exitLoggedIn || !strings.Contains(stderr, "not an app") {
		t.Errorf("an app claiming: exit %d, %q", code, stderr)
	}
	h.secret = "rotated"
	if code, _, stderr := (cli{t: t}).run(context.Background(), "whoami"); code != exitLoggedIn || !strings.Contains(stderr, "refused the app's client credentials") {
		t.Errorf("a revoked secret: exit %d, %q; want 3", code, stderr)
	}
	h.clientID = "nobody"
	if code, _, _ := (cli{t: t}).run(context.Background(), "login", "--app", "--client-id", "htg-bot", "--secret-file", secret, "--hub", h.srv.URL); code != exitFailed {
		t.Errorf("login with refused credentials: exit %d, want 1", code)
	}
}

func TestUsage(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{
		{"login"}, {"login", "--local", "--app"}, {"claim"}, {"servers"}, {"servers", "status"},
		{"events", "--since", "yesterday"}, {"events", "--limit", "-1"}, {"fleet", "extra"},
	} {
		if code, _, _ := (cli{t: t}).run(context.Background(), args...); code != exitUsage {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

func TestNormalizeHub(t *testing.T) {
	cases := map[string]string{
		"hub.example.org":           "https://hub.example.org",
		"https://Hub.Example.org/":  "https://hub.example.org",
		"http://127.0.0.1:8080":     "http://127.0.0.1:8080",
		"https://hub.example.org/x": "",
		"ftp://hub":                 "",
		"https://u@hub.example.org": "",
		"":                          "",
	}
	for in, want := range cases {
		got, err := normalizeHub(in)
		if (err != nil) != (want == "") || got != want {
			t.Errorf("normalizeHub(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestCleartextWarning(t *testing.T) {
	for hub, warn := range map[string]bool{"http://hub.lan:8080": true, "http://127.0.0.1:8080": false, "http://localhost:8080": false, "https://hub.example.org": false} {
		var b bytes.Buffer
		cleartextWarning(&b, hub)
		if (b.Len() > 0) != warn {
			t.Errorf("%s: warning %q, want %v", hub, b.String(), warn)
		}
	}
}

// TestNoGRPC holds principle 4: the CLI speaks Connect's HTTP-JSON, and the gRPC stack is not in
// its binary at all.
func TestNoGRPC(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	out, err := exec.CommandContext(t.Context(), "go", "list", "-deps", "github.com/gravel-project/gravel/cmd/gravel").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	deps := strings.Fields(string(out))
	if len(deps) < 50 || !strings.Contains(string(out), "connectrpc.com/connect\n") {
		t.Fatalf("go list answered %d packages without connect; the check would prove nothing", len(deps))
	}
	for _, d := range deps {
		if d == "google.golang.org/grpc" || strings.HasPrefix(d, "google.golang.org/grpc/") {
			t.Errorf("cmd/gravel depends on %s: the CLI must not carry the gRPC stack (principle 4)", d)
		}
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
