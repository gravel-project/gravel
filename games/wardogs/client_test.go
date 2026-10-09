package wardogs_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gravel-project/gravel/games/wardogs"
	"github.com/gravel-project/gravel/games/wardogs/wardogstest"
)

func newClient(t *testing.T, url, token string) *wardogs.Client {
	t.Helper()
	c, err := wardogs.New(url, wardogs.Options{Token: token, Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewRejectsABadBaseURL(t *testing.T) {
	for _, u := range []string{"", "ftp://x", "http://", "http://h/path", "http://h?x=1", "::"} {
		if _, err := wardogs.New(u, wardogs.Options{}); err == nil {
			t.Errorf("New(%q) passed", u)
		}
	}
	c, err := wardogs.New("http://203.0.113.10:7789/", wardogs.Options{Token: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL() != "http://203.0.113.10:7789" || strings.Contains(c.String(), "secret") {
		t.Errorf("base %q, string %q", c.BaseURL(), c.String())
	}
}

// One bad token costs one strike: the client sends nothing protected after a 401, so the
// server's three-strike throttle can never trip, and a new token lifts it.
func TestARefusedTokenIsNeverRetried(t *testing.T) {
	srv := wardogstest.New(t, latest(t), wardogstest.Options{})
	c := newClient(t, srv.URL, "wrong")
	ctx := context.Background()
	for range 5 {
		if _, err := c.Status(ctx); !errors.Is(err, wardogs.ErrTokenRefused) {
			t.Fatalf("status with a bad token = %v", err)
		}
	}
	if srv.Strikes() != 1 {
		t.Fatalf("strikes = %d, want 1", srv.Strikes())
	}
	if c.HasToken() {
		t.Error("a refused token still counts as a token")
	}
	// The public routes still answer.
	if _, err := c.Health(ctx); err != nil {
		t.Errorf("health after a refusal = %v", err)
	}
	c.SetToken(wardogstest.Token)
	if _, err := c.Status(ctx); err != nil {
		t.Errorf("status with the right token = %v", err)
	}
	if srv.Strikes() != 1 {
		t.Errorf("strikes = %d after the new token", srv.Strikes())
	}
}

// Concurrent callers with a bad token still spend one strike: protected requests are serialised
// until the token has been accepted once.
func TestConcurrentCallersSpendOneStrike(t *testing.T) {
	srv := wardogstest.New(t, latest(t), wardogstest.Options{})
	c := newClient(t, srv.URL, "wrong")
	ctx := context.Background()
	if _, err := c.Capabilities(ctx); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() { _, _ = c.Players(ctx) })
	}
	wg.Wait()
	if srv.Strikes() != 1 {
		t.Errorf("strikes = %d, want 1", srv.Strikes())
	}

	// Once accepted, calls run in parallel without the probe lock (no deadlock, all succeed).
	c.SetToken(wardogstest.Token)
	errs := make(chan error, 16)
	for range 16 {
		wg.Go(func() { _, err := c.Players(ctx); errs <- err })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

func TestNoTokenSendsNothingProtected(t *testing.T) {
	srv := wardogstest.New(t, latest(t), wardogstest.Options{})
	c := newClient(t, srv.URL, "")
	ctx := context.Background()
	if _, err := c.Status(ctx); !errors.Is(err, wardogs.ErrNoToken) {
		t.Errorf("status without a token = %v", err)
	}
	if _, err := c.Health(ctx); err != nil {
		t.Errorf("health without a token = %v", err)
	}
	if srv.Strikes() != 0 {
		t.Errorf("strikes = %d", srv.Strikes())
	}
	for _, r := range srv.Requests() {
		if r.Path != "/v1/capabilities" && r.Path != "/v1/health" {
			t.Errorf("sent %s %s without a token", r.Method, r.Path)
		}
	}
}

func TestPublicRoutesCarryNoToken(t *testing.T) {
	srv := wardogstest.New(t, latest(t), wardogstest.Options{})
	c := newClient(t, srv.URL, wardogstest.Token)
	ctx := context.Background()
	if _, err := c.Health(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Status(ctx); err != nil {
		t.Fatal(err)
	}
	for _, r := range srv.Requests() {
		public := r.Path == "/v1/capabilities" || r.Path == "/v1/health"
		if public == r.HadToken {
			t.Errorf("%s %s: token sent = %v", r.Method, r.Path, r.HadToken)
		}
	}
}

func TestRetryAfterIsKept(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	srv := wardogstest.New(t, latest(t), wardogstest.Options{Handle: map[string]http.HandlerFunc{
		"GET /v1/status": func(w http.ResponseWriter, _ *http.Request) {
			if calls.Add(1) == 1 {
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			_, _ = io.WriteString(w, `{"map":"Ozeti","players":{"current":1,"max":100}}`)
		},
	}})
	c, err := wardogs.New(srv.URL, wardogs.Options{Token: wardogstest.Token, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, err = c.Status(ctx)
	var rl *wardogs.RateLimitedError
	if !errors.As(err, &rl) || rl.RetryAfter != 7*time.Second {
		t.Fatalf("first status = %v", err)
	}
	now = now.Add(6 * time.Second)
	if _, err := c.Status(ctx); !errors.As(err, &rl) || calls.Load() != 1 {
		t.Fatalf("inside the wait: %v, %d calls", err, calls.Load())
	}
	if _, err := c.Health(ctx); !errors.As(err, &rl) {
		t.Errorf("health inside the wait = %v", err)
	}
	now = now.Add(2 * time.Second)
	if s, err := c.Status(ctx); err != nil || s.Map != "Ozeti" || calls.Load() != 2 {
		t.Errorf("after the wait: %+v, %v, %d calls", s, err, calls.Load())
	}
}

func TestServerErrorsKeepTheirCode(t *testing.T) {
	srv := wardogstest.New(t, latest(t), wardogstest.Options{Handle: map[string]http.HandlerFunc{
		"POST /v1/bans": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":"player_not_found","message":"no such player on the server"}}`)
		},
	}})
	c := newClient(t, srv.URL, wardogstest.Token)
	err := c.Ban(context.Background(), "76561190000000001", "")
	var apiErr *wardogs.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 404 || !wardogs.IsCode(err, "player_not_found") {
		t.Errorf("ban of an absent player = %v", err)
	}
	if wardogs.IsCode(errors.New("x"), "player_not_found") {
		t.Error("IsCode matched a plain error")
	}
}

func TestModerationRequests(t *testing.T) {
	type seen struct{ method, path, body string }
	var (
		mu  sync.Mutex
		got []seen
	)
	record := func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, seen{r.Method, r.URL.Path, strings.TrimSpace(string(b))})
		mu.Unlock()
		if r.URL.Path == "/v1/broadcast" {
			_, _ = io.WriteString(w, `{"ok":true,"pending":false,"message":"hi"}`)
		}
	}
	srv := wardogstest.New(t, latest(t), wardogstest.Options{
		// This build spells the player parameter {steamId}; the client follows.
		Rename: map[string]string{"POST /v1/players/{id}/kick": "POST /v1/players/{steamId}/kick"},
		Handle: map[string]http.HandlerFunc{
			"POST /v1/players/{}/kick": record, "POST /v1/players/{}/kill": record,
			"POST /v1/players/{}/message": record, "PATCH /v1/players/{}": record,
			"POST /v1/broadcast": record, "POST /v1/bans": record, "DELETE /v1/bans/{}": record,
		},
	})
	c := newClient(t, srv.URL, wardogstest.Token)
	ctx := context.Background()
	id := wardogs.SteamID("76561190000000001")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(c.Kick(ctx, id, "AFK"))
	must(c.Kill(ctx, id))
	must(c.Message(ctx, id, "hello"))
	must(c.MovePlayer(ctx, id, "Valkyra"))
	b, err := c.Broadcast(ctx, "hi")
	must(err)
	if !b.OK {
		t.Errorf("broadcast = %+v", b)
	}
	must(c.Ban(ctx, id, "cheating"))
	must(c.Ban(ctx, id, ""))
	must(c.Unban(ctx, id))
	want := []seen{
		{"POST", "/v1/players/76561190000000001/kick", `{"reason":"AFK"}`},
		{"POST", "/v1/players/76561190000000001/kill", ""},
		{"POST", "/v1/players/76561190000000001/message", `{"message":"hello"}`},
		{"PATCH", "/v1/players/76561190000000001", `{"faction":"Valkyra"}`},
		{"POST", "/v1/broadcast", `{"message":"hi"}`},
		{"POST", "/v1/bans", `{"reason":"cheating","steamId":"76561190000000001"}`},
		{"POST", "/v1/bans", `{"steamId":"76561190000000001"}`},
		{"DELETE", "/v1/bans/76561190000000001", ""},
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("requests = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("request %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestABodyOverTheLimitIsNotSent(t *testing.T) {
	srv := wardogstest.New(t, latest(t), wardogstest.Options{})
	c := newClient(t, srv.URL, wardogstest.Token)
	ctx := context.Background()
	if _, err := c.Capabilities(ctx); err != nil {
		t.Fatal(err)
	}
	before := len(srv.Requests())
	_, err := c.Broadcast(ctx, strings.Repeat("x", 70000))
	if !errors.Is(err, wardogs.ErrBodyTooLarge) || len(srv.Requests()) != before {
		t.Errorf("oversized broadcast = %v, %d requests sent", err, len(srv.Requests())-before)
	}
}

// A field whose type drifted costs that field in production and fails a strict client.
func TestTolerantAndStrictDecoding(t *testing.T) {
	srv := wardogstest.New(t, latest(t), wardogstest.Options{Handle: map[string]http.HandlerFunc{
		"GET /v1/status": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"map":"Ozeti","lighting":{"id":"DayClear"},"players":{"current":3,"max":100},"newField":[1,2]}`)
		},
	}})
	var mismatches []string
	tolerant, err := wardogs.New(srv.URL, wardogs.Options{Token: wardogstest.Token, OnMismatch: func(route string, err error) {
		mismatches = append(mismatches, route+": "+err.Error())
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s, err := tolerant.Status(ctx)
	if err != nil || s.Map != "Ozeti" || s.Players.Current != 3 || s.Lighting != "" {
		t.Errorf("tolerant status = %+v, %v", s, err)
	}
	if len(mismatches) != 1 || !strings.Contains(mismatches[0], "GET /v1/status") {
		t.Errorf("mismatches = %v", mismatches)
	}
	if _, err := newClient(t, srv.URL, wardogstest.Token).Status(ctx); err == nil {
		t.Error("a strict client accepted a drifted field")
	}
}

func TestSteamIDReadsEveryEncoding(t *testing.T) {
	srv := wardogstest.New(t, latest(t), wardogstest.Options{Handle: map[string]http.HandlerFunc{
		"GET /v1/reserved-slots": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"reservedSlots":["76561190000000001",76561190000000002,{"steamId":"76561190000000003","note":"x"},null],"count":4}`)
		},
	}})
	r, err := newClient(t, srv.URL, wardogstest.Token).ReservedSlots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []wardogs.SteamID{"76561190000000001", "76561190000000002", "76561190000000003", ""}
	if len(r.ReservedSlots) != len(want) {
		t.Fatalf("slots = %v", r.ReservedSlots)
	}
	for i := range want {
		if r.ReservedSlots[i] != want[i] {
			t.Errorf("slot %d = %q, want %q", i, r.ReservedSlots[i], want[i])
		}
	}
}

func TestTimes(t *testing.T) {
	if _, ok := (wardogs.Ban{BannedAtUTC: "0001-01-01T00:00:00.000Z"}).BannedAt(); ok {
		t.Error("a config-sourced ban has a time")
	}
	at, ok := (wardogs.AuditEntry{TimestampUTC: "2026-10-09T22:58:29.444Z"}).Time()
	if !ok || at.Minute() != 58 {
		t.Errorf("audit time = %v, %v", at, ok)
	}
}
