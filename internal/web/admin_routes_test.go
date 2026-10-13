package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/web"
	"github.com/gravel-project/gravel/internal/web/admin"
)

// patterns records the routes Register adds.
type patterns []string

func (p *patterns) HandleFunc(pattern string, _ func(http.ResponseWriter, *http.Request)) {
	*p = append(*p, pattern)
}

// No member page lives under /admin, the dashboard's (ADR-0029): every route Register adds is
// listed and checked, and on one mux, as the hub has them, /admin reaches the dashboard.
func TestAdminRoutePrecedence(t *testing.T) {
	var h *web.Handler
	newRigWith(t, testTheme(), true, func(_ *rig, _ *[]identity.Option, hs *[]func(*web.Handler)) {
		*hs = append(*hs, func(x *web.Handler) { h = x })
	})
	var routes patterns
	h.Register(&routes)
	if len(routes) < 30 {
		t.Fatalf("Register added %d routes; the recorder missed some", len(routes))
	}
	for _, p := range routes {
		path := p
		if _, rest, ok := strings.Cut(p, " "); ok {
			path = rest
		}
		if path == admin.Prefix || strings.HasPrefix(path, admin.Prefix+"/") || (strings.HasPrefix(path, "/{") && path != "/{$}") {
			t.Errorf("member page route %q can take a path under %s", p, admin.Prefix)
		}
	}

	mux := http.NewServeMux() // a conflicting pattern panics here
	admin.New(fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>Admin</title>")}}).Register(mux)
	h.Register(mux)
	for path, want := range map[string]string{
		"/admin":        "GET /admin",
		"/admin/":       "GET /admin/{path...}",
		"/admin/people": "GET /admin/{path...}",
		"/servers":      "GET /servers",
		"/":             "GET /{$}",
	} {
		_, got := mux.Handler(httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		if got != want {
			t.Errorf("%s routes to %q, want %q", path, got, want)
		}
	}
}
