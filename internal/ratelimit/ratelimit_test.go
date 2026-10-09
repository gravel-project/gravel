package ratelimit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
)

func TestAllowAndRefill(t *testing.T) {
	now := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	l := New(60, 2) // one per second, burst two
	l.now = func() time.Time { return now }
	for i := range 2 {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("burst request %d refused", i)
		}
	}
	ok, retry := l.Allow("a")
	if ok || retry <= 0 || retry > time.Second {
		t.Fatalf("third request: ok=%v retry=%s", ok, retry)
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Error("another key has its own bucket")
	}
	now = now.Add(time.Second)
	if ok, _ := l.Allow("a"); !ok {
		t.Error("one token refills per second")
	}
	if ok, _ := l.Allow("a"); ok {
		t.Error("and only one")
	}
	if n := l.Sweep(); n != 2 {
		t.Errorf("sweep keeps live keys: %d", n)
	}
	now = now.Add(idleFor + time.Second)
	if n := l.Sweep(); n != 0 {
		t.Errorf("sweep drops idle keys: %d", n)
	}
}

func TestZeroBurstRefusesEverything(t *testing.T) {
	l := New(60, 0)
	if ok, retry := l.Allow("a"); ok || retry <= 0 {
		t.Errorf("ok=%v retry=%s", ok, retry)
	}
}

func TestMiddleware(t *testing.T) {
	ip := New(60, 1)
	user := New(60, 2)
	var rejected []string
	scopes := []Scoped{
		{Scope: "user", Limiter: user, Key: func(r *http.Request) string { return r.Header.Get("X-User") }},
		{Scope: "ip", Limiter: ip, Key: ClientIP},
	}
	h := Middleware(scopes, func(s string) { rejected = append(rejected, s) })(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	do := func(remote, user string) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		r.RemoteAddr = remote
		if user != "" {
			r.Header.Set("X-User", user)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := do("10.0.0.1:1234", ""); w.Code != http.StatusNoContent {
		t.Errorf("first anonymous: %d", w.Code)
	}
	w := do("10.0.0.1:9999", "")
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Errorf("second anonymous from the same ip: %d %q", w.Code, w.Header().Get("Retry-After"))
	}
	if w := do("10.0.0.2:1", ""); w.Code != http.StatusNoContent {
		t.Errorf("another ip: %d", w.Code)
	}
	// A user is keyed by user, not ip: two from the limited ip still pass, the third is refused.
	for i := range 2 {
		if w := do("10.0.0.1:1", "u1"); w.Code != http.StatusNoContent {
			t.Errorf("user request %d: %d", i, w.Code)
		}
	}
	if w := do("10.0.0.1:1", "u1"); w.Code != http.StatusTooManyRequests {
		t.Errorf("third user request: %d", w.Code)
	}
	if len(rejected) != 2 || rejected[0] != "ip" || rejected[1] != "user" {
		t.Errorf("rejected scopes: %v", rejected)
	}
}

func TestInterceptor(t *testing.T) {
	ip := New(60, 1)
	scopes := []Scoped{{Scope: "ip", Limiter: ip, Key: ClientIP}}
	called := 0
	next := connect.UnaryFunc(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		called++
		return nil, nil
	})
	wrapped := Interceptor(scopes, nil).WrapUnary(next)
	// A client-side connect.Request has no peer; wrap it with the server-side view the
	// interceptor reads.
	r := &peerRequest{AnyRequest: connect.NewRequest(&struct{}{}), peer: connect.Peer{Addr: "10.0.0.1:5"}}
	if _, err := wrapped(context.Background(), r); err != nil {
		t.Fatalf("first: %v", err)
	}
	_, err := wrapped(context.Background(), r)
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("second: %v", err)
	}
	var ce *connect.Error
	if !errorsAs(err, &ce) || ce.Meta().Get("Retry-After") == "" {
		t.Errorf("want Retry-After metadata, got %v", err)
	}
	if called != 1 {
		t.Errorf("next called %d times", called)
	}
}

type peerRequest struct {
	connect.AnyRequest
	peer connect.Peer
}

func (p *peerRequest) Peer() connect.Peer { return p.peer }

func errorsAs(err error, target **connect.Error) bool {
	ce, ok := err.(*connect.Error) //nolint:errorlint // the interceptor returns the *connect.Error itself
	if ok {
		*target = ce
	}
	return ok
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	r.RemoteAddr = "[::1]:4000"
	if got := ClientIP(r); got != "::1" {
		t.Errorf("v6: %q", got)
	}
	r.RemoteAddr = "noport"
	if got := ClientIP(r); got != "noport" {
		t.Errorf("fallback: %q", got)
	}
}
