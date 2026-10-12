package ingest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type sinks struct {
	mu     sync.Mutex
	events []string
	demos  []string
	max    int64
	fail   bool
}

func (s *sinks) Events(_ context.Context, server string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, server+" "+string(body))
}

func (s *sinks) MaxDemoBytes() int64 { return s.max }

func (s *sinks) Demo(_ context.Context, server string, h http.Header, body io.Reader, _ int64) error {
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if s.fail {
		return errors.New("bucket away")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.demos = append(s.demos, server+" "+h.Get("MatchZy-MatchId")+" "+string(b))
	return nil
}

func (r *rig) plugin(path, token, body, addr string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("MatchZy-MatchId", "27")
	req.RemoteAddr = addr
	rec := httptest.NewRecorder()
	if path == EventsPath {
		r.h.ServeEvents(rec, req)
	} else {
		r.h.ServeDemo(rec, req)
	}
	return rec
}

// A match plugin's events and demos (ADR-0024) come from the server's own host with its feed
// token in the Authorization header: an event is stored, then told; a demo goes to its sink.
func TestPluginRoutes(t *testing.T) {
	r, _ := newLogRig(t)
	const host = "203.0.113.7:4000"
	if rec := r.plugin(EventsPath, "log-token", `{"event":"going_live"}`, host); rec.Code != http.StatusNotFound {
		t.Errorf("no sinks: %d", rec.Code)
	}
	s := &sinks{max: 1 << 10}
	r.h.SetPluginSinks(s, s)
	for name, c := range map[string]struct {
		token, body, addr string
		want              int
	}{
		"stored":         {"log-token", `{"event":"going_live","matchid":27}`, host, http.StatusOK},
		"no token":       {"", `{}`, host, http.StatusUnauthorized},
		"wrong token":    {"nope", `{}`, host, http.StatusUnauthorized},
		"another place":  {"log-token", `{}`, "192.0.2.1:4000", http.StatusUnauthorized},
		"not JSON":       {"log-token", `event`, host, http.StatusBadRequest},
		"empty":          {"log-token", ``, host, http.StatusBadRequest},
		"too large":      {"log-token", "{" + strings.Repeat("x", MaxEventsBody), host, http.StatusRequestEntityTooLarge},
		"a feed's token": {"token-one", `{}`, host, http.StatusUnauthorized}, // wd-1 has no host
	} {
		if rec := r.plugin(EventsPath, c.token, c.body, c.addr); rec.Code != c.want {
			t.Errorf("%s: %d, want %d", name, rec.Code, c.want)
		}
	}
	if len(s.events) != 1 || s.events[0] != `cs2-1 {"event":"going_live","matchid":27}` {
		t.Errorf("events %v", s.events)
	}
	if b := r.st.batches; len(b) != 1 || b[0].Source != EventsSource || b[0].ServerID != "cs2-1" {
		t.Errorf("batches %+v", b)
	}
	r.st.fail = true
	if rec := r.plugin(EventsPath, "log-token", `{"event":"x"}`, host); rec.Code != http.StatusServiceUnavailable || len(s.events) != 1 {
		t.Errorf("not stored: %d, %d told", rec.Code, len(s.events))
	}

	// Demos.
	if rec := r.plugin(DemoPath, "log-token", "demo bytes", host); rec.Code != http.StatusOK || len(s.demos) != 1 || s.demos[0] != "cs2-1 27 demo bytes" {
		t.Errorf("a demo: %d %v", rec.Code, s.demos)
	}
	if rec := r.plugin(DemoPath, "log-token", strings.Repeat("d", 2<<10), host); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("too large: %d", rec.Code)
	}
	if rec := r.plugin(DemoPath, "log-token", "x", "192.0.2.1:4000"); rec.Code != http.StatusUnauthorized {
		t.Errorf("from elsewhere: %d", rec.Code)
	}
	s.fail = true
	if rec := r.plugin(DemoPath, "log-token", "x", host); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("not kept: %d", rec.Code)
	}
	s.max = 0
	if rec := r.plugin(DemoPath, "log-token", "x", host); rec.Code != http.StatusNotFound {
		t.Errorf("no demos kept: %d", rec.Code)
	}
}
