// Package wardogstest serves a recorded War Dogs build (games/wardogs/testdata/<build>/) as a
// fake server: GET routes answer from the files, protected routes check the bearer token with
// the real server's three-strike throttle, and a test can drop or rename advertised routes or
// handle the writes itself.
package wardogstest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gravel-project/gravel/games/wardogs"
)

// Token is the fake server's token unless Options name another.
const Token = "fixture-token"

// Options shape the fake server.
type Options struct {
	// Token is the bearer token the server accepts; empty means Token.
	Token string
	// Remove drops advertised routes, matched by shape ("POST /v1/players/{}/kick" or with any
	// parameter names).
	Remove []string
	// Rename respells advertised routes ("DELETE /v1/bans/{steamId}" → "DELETE /v1/bans/{id}").
	Rename map[string]string
	// ReadOnly makes the configuration document not writable in the capabilities.
	ReadOnly bool
	// Handle answers a non-GET route (by shape), or overrides a GET; the token is checked first.
	Handle map[string]http.HandlerFunc
}

// Request is one request the server saw.
type Request struct {
	Method, Path string
	// Authorized is whether it carried the right token.
	Authorized bool
	// HadToken is whether it carried an Authorization header at all.
	HadToken bool
}

// Server is a running fake.
type Server struct {
	*httptest.Server
	dir string
	opt Options

	mu       sync.Mutex
	caps     []byte
	ready    map[string]bool // advertised shapes
	requests []Request
	strikes  int
}

// New starts a fake serving one build's directory; it stops when the test ends.
func New(t testing.TB, dir string, opt Options) *Server {
	t.Helper()
	if opt.Token == "" {
		opt.Token = Token
	}
	raw, err := os.ReadFile(filepath.Join(dir, "v1", "capabilities.json"))
	if err != nil {
		t.Fatalf("wardogstest: %v", err)
	}
	var caps map[string]any
	if err := json.Unmarshal(raw, &caps); err != nil {
		t.Fatalf("wardogstest: capabilities: %v", err)
	}
	remove := map[string]bool{}
	for _, r := range opt.Remove {
		remove[shape(t, r)] = true
	}
	rename := map[string]string{}
	for from, to := range opt.Rename {
		rename[shape(t, from)] = to
	}
	s := &Server{dir: dir, opt: opt, ready: map[string]bool{}}
	var routes []any
	for _, v := range caps["routes"].([]any) {
		route := v.(string)
		sh := shape(t, route)
		if remove[sh] {
			continue
		}
		if to, ok := rename[sh]; ok {
			route = to
		}
		routes = append(routes, route)
		s.ready[shape(t, route)] = true
	}
	caps["routes"] = routes
	if opt.ReadOnly {
		if cfg, ok := caps["config"].(map[string]any); ok {
			cfg["writable"] = false
		}
	}
	if s.caps, err = json.Marshal(caps); err != nil {
		t.Fatalf("wardogstest: %v", err)
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Upgrade is a game update while the server runs: the capabilities name another build and no
// longer advertise the routes in remove (by shape, as in Options.Remove). The recorded answers
// stay the same.
func (s *Server) Upgrade(t testing.TB, build string, remove ...string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var caps map[string]any
	if err := json.Unmarshal(s.caps, &caps); err != nil {
		t.Fatalf("wardogstest: %v", err)
	}
	drop := map[string]bool{}
	for _, r := range remove {
		drop[shape(t, r)] = true
	}
	var routes []any
	for _, v := range caps["routes"].([]any) {
		if sh := shape(t, v.(string)); drop[sh] {
			delete(s.ready, sh)
		} else {
			routes = append(routes, v)
		}
	}
	caps["routes"], caps["build"] = routes, build
	raw, err := json.Marshal(caps)
	if err != nil {
		t.Fatalf("wardogstest: %v", err)
	}
	s.caps = raw
}

// Builds are the recorded builds' directories under a testdata directory, oldest first.
func Builds(t testing.TB, testdata string) []string {
	t.Helper()
	entries, err := os.ReadDir(testdata)
	if err != nil {
		t.Fatalf("wardogstest: %v", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			if _, err := os.Stat(filepath.Join(testdata, e.Name(), "v1", "capabilities.json")); err == nil {
				out = append(out, filepath.Join(testdata, e.Name()))
			}
		}
	}
	slices.Sort(out)
	return out
}

// Requests are the requests so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// Strikes is how many requests carried a missing or wrong token.
func (s *Server) Strikes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.strikes
}

func shape(t testing.TB, route string) string {
	r, err := wardogs.ParseRoute(route)
	if err != nil {
		t.Fatalf("wardogstest: %v", err)
	}
	return r.Shape()
}

// shapeOf is the advertised shape a request path matches, or "".
func (s *Server) shapeOf(method, path string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for sh := range s.ready {
		r, _ := wardogs.ParseRoute(sh)
		if r.Method != method || len(r.Segments) != len(segs) {
			continue
		}
		match := true
		for i, seg := range r.Segments {
			if seg != "{}" && seg != segs[i] {
				match = false
				break
			}
		}
		if match {
			return sh
		}
	}
	return ""
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	authorized := auth == "Bearer "+s.opt.Token
	s.mu.Lock()
	s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, Authorized: authorized, HadToken: auth != ""})
	caps := s.caps
	s.mu.Unlock()

	if r.Method == http.MethodGet && r.URL.Path == "/v1/capabilities" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(caps)
		return
	}
	sh := s.shapeOf(r.Method, r.URL.Path)
	public := sh == "GET /v1/health"
	if !public {
		s.mu.Lock()
		if !authorized {
			s.strikes++
		}
		throttled := s.strikes >= 3
		s.mu.Unlock()
		switch {
		case throttled:
			w.Header().Set("Retry-After", "30")
			writeError(w, http.StatusTooManyRequests, "rate_limited", "too many failed authentication attempts")
			return
		case !authorized && auth == "":
			writeError(w, http.StatusUnauthorized, "credential_missing", "a bearer token is required")
			return
		case !authorized:
			writeError(w, http.StatusUnauthorized, "credential_invalid", "the bearer token is not valid")
			return
		}
	}
	if sh == "" {
		writeError(w, http.StatusNotFound, "not_found", "no such route")
		return
	}
	if h, ok := s.opt.Handle[sh]; ok {
		h(w, r)
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "the fake serves recorded reads only")
		return
	}
	data, err := os.ReadFile(filepath.Join(s.dir, filepath.FromSlash(strings.Trim(r.URL.Path, "/"))+".json"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "not recorded")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": message}})
}
