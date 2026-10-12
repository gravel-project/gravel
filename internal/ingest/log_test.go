package ingest

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/gravel-project/gravel/internal/httpx"
)

type pin map[string][]netip.Addr

func (p pin) SourceAddresses(server string) ([]netip.Addr, bool) {
	a, ok := p[server]
	return a, ok
}

type tap struct {
	mu    sync.Mutex
	got   []string
	bytes []string
}

func (t *tap) Lines(server string, body []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.got = append(t.got, server)
	t.bytes = append(t.bytes, string(body))
}

func newLogRig(t *testing.T) (*rig, *tap) {
	t.Helper()
	r := newRig(t)
	r.keys.Update([]Feed{{Server: "wd-1", Source: "wardogs", Token: "token-one"}, {Server: "cs2-1", Source: "cs2", Token: "log-token"}})
	tp := &tap{}
	r.h.SetLogTap(tp)
	r.h.SetAddressPin(pin{"cs2-1": {netip.MustParseAddr("203.0.113.7"), netip.MustParseAddr("198.51.100.40")}})
	r.h.SetLogSource("cs2", LogSource{
		Headers: []string{"X-Server-Instance-Token", "X-LogBytes-BeginOffset"},
		Redact: func(body []byte, secrets ...string) []byte {
			for _, s := range secrets {
				body = bytes.ReplaceAll(body, []byte(s), []byte("[REDACTED]"))
			}
			return bytes.ReplaceAll(body, []byte("hunter2"), []byte("[REDACTED]"))
		},
	})
	return r, tp
}

// postLog sends a batch through a mux with the route's pattern, as the hub mounts it, inside the
// access log.
func postLog(r *rig, token, body, addr string, logs *strings.Builder) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+LogPath, r.h.ServeLog)
	h := httpx.Logging(slog.New(slog.NewTextHandler(logs, nil)))(mux)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, strings.Replace(LogPath, "{token}", token, 1), strings.NewReader(body))
	req.RemoteAddr = addr
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-Server-Instance-Token", "abc")
	req.Header.Set("X-LogBytes-BeginOffset", "0")
	req.Header.Set("Cookie", "not=kept")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestLogRouteStoresRedactedWithHeaders(t *testing.T) {
	r, tp := newLogRig(t)
	logs := &strings.Builder{}
	body := "10/12/2026 - 19:00:00.111 - \"sv_password\" = \"hunter2\"\nrcon from x: command \"echo log-token\"\n"
	if rec := postLog(r, "log-token", body, "203.0.113.7:40000", logs); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("answer %d %q", rec.Code, rec.Body.String())
	}
	if len(r.st.batches) != 1 {
		t.Fatalf("%d batches", len(r.st.batches))
	}
	b := r.st.batches[0]
	if b.ServerID != "cs2-1" || b.Source != "cs2" || strings.Contains(string(b.Body), "hunter2") || strings.Contains(string(b.Body), "log-token") {
		t.Errorf("stored %+v %q", b, b.Body)
	}
	if string(b.Headers) != `{"X-LogBytes-BeginOffset":"0","X-Server-Instance-Token":"abc"}` {
		t.Errorf("headers %s: the source's headers and nothing else", b.Headers)
	}
	if len(tp.got) != 1 || tp.got[0] != "cs2-1" || strings.Contains(tp.bytes[0], "hunter2") {
		t.Errorf("tap %v", tp.got)
	}
	if strings.Contains(logs.String(), "log-token") || !strings.Contains(logs.String(), "/api/ingest/log/<redacted>") {
		t.Errorf("access log:\n%s", logs.String())
	}
}

func TestLogRouteRefusals(t *testing.T) {
	r, tp := newLogRig(t)
	logs := &strings.Builder{}
	for _, c := range []struct {
		name, token, addr string
		code              int
	}{
		{"unknown token", "nope", "203.0.113.7:1", http.StatusUnauthorized},
		{"another address", "log-token", "198.51.100.1:1", http.StatusUnauthorized},
	} {
		if rec := postLog(r, c.token, "x\n", c.addr, logs); rec.Code != c.code {
			t.Errorf("%s: %d, want %d", c.name, rec.Code, c.code)
		}
	}
	if v := testutil.ToFloat64(r.h.batches.WithLabelValues("cs2-1", "cs2", "address_mismatch")); v != 1 {
		t.Errorf("address_mismatch counted %v", v)
	}
	// A host that is not running pushes nothing a server's token can vouch for.
	r.h.SetAddressPin(pin{})
	if rec := postLog(r, "log-token", "x\n", "203.0.113.7:1", logs); rec.Code != http.StatusUnauthorized {
		t.Errorf("no host: %d", rec.Code)
	}
	r.h.SetAddressPin(pin{"cs2-1": {netip.MustParseAddr("203.0.113.7"), netip.MustParseAddr("198.51.100.40")}})
	if rec := postLog(r, "log-token", strings.Repeat("x", MaxLogBody+1), "203.0.113.7:1", logs); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("too large: %d", rec.Code)
	}
	if rec := postLog(r, "log-token", "", "203.0.113.7:1", logs); rec.Code != http.StatusBadRequest {
		t.Errorf("empty: %d", rec.Code)
	}
	r.st.fail = true
	if rec := postLog(r, "log-token", "x\n", "203.0.113.7:1", logs); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("not stored: %d", rec.Code)
	}
	if len(r.st.batches) != 0 || len(tp.got) != 0 {
		t.Errorf("stored %d, tapped %d", len(r.st.batches), len(tp.got))
	}
	// The War Dogs token is not a log token, and the log token is not a bearer.
	r.st.fail = false
	if rec := postLog(r, "token-one", "x\n", "203.0.113.7:1", logs); rec.Code != http.StatusUnauthorized {
		t.Errorf("a server without a host on the log route: %d, want 401", rec.Code)
	}
	if rec := r.post("log-token", "x", "203.0.113.7:1"); rec.Code != http.StatusUnauthorized {
		t.Errorf("a log token as a bearer: %d, want 401 (it would skip the address pin)", rec.Code)
	}
}

// TestLogRouteTakesTheReservedAddress: with the provider's egress script, a cloud-VM server's log
// push leaves from its reserved address; the route takes it from there as from the instance's own.
func TestLogRouteTakesTheReservedAddress(t *testing.T) {
	r, _ := newLogRig(t)
	logs := &strings.Builder{}
	for _, addr := range []string{"203.0.113.7:40000", "198.51.100.40:40001"} {
		if rec := postLog(r, "log-token", "x\n", addr, logs); rec.Code != http.StatusOK {
			t.Errorf("from %s: answer %d", addr, rec.Code)
		}
	}
	if rec := postLog(r, "log-token", "x\n", "192.0.2.9:40002", logs); rec.Code != http.StatusUnauthorized {
		t.Errorf("from another address: answer %d", rec.Code)
	}
}
