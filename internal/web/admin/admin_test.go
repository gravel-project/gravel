package admin_test

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"golang.org/x/net/html"

	"github.com/gravel-project/gravel/internal/web"
	"github.com/gravel-project/gravel/internal/web/admin"
)

const index = `<!doctype html><html lang="en"><head><title>Admin</title><script type="module" crossorigin src="/admin/assets/index-abc123.js"></script></head><body><div id="root"></div></body></html>`

func serve(t *testing.T, files fs.FS) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	admin.New(files).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// reply is a response with its body read and closed.
type reply struct {
	StatusCode int
	Header     http.Header
}

func get(t *testing.T, method, url string) (reply, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return reply{StatusCode: resp.StatusCode, Header: resp.Header}, string(b)
}

// A binary built without the bundle says so at /admin, under the same headers.
func TestAdminNotBuilt(t *testing.T) {
	srv := serve(t, fstest.MapFS{".gitkeep": {}})
	for _, p := range []string{"/admin", "/admin/", "/admin/people", "/admin/assets/index-abc123.js"} {
		resp, body := get(t, http.MethodGet, srv.URL+p)
		if resp.StatusCode != http.StatusServiceUnavailable || body != admin.NotBuilt {
			t.Errorf("%s: %d %q, want 503 %q", p, resp.StatusCode, body, admin.NotBuilt)
		}
		if got := resp.Header.Get("Content-Security-Policy"); got != web.CSP {
			t.Errorf("%s: CSP %q", p, got)
		}
	}
	if admin.New(fstest.MapFS{".gitkeep": {}}).Built() {
		t.Error("a bundle without index.html counts as built")
	}
}

// Every route of the dashboard is its index; the hashed assets are files, cached for good, and a
// missing one is a 404, not the index.
func TestAdminIndexFallback(t *testing.T) {
	srv := serve(t, fstest.MapFS{
		"index.html":             {Data: []byte(index)},
		"assets/index-abc123.js": {Data: []byte("console.log(1)")},
		"assets/index-abc.css":   {Data: []byte("body{}")},
	})
	for _, p := range []string{"/admin", "/admin/", "/admin/people", "/admin/servers/htg-wardogs-1", "/admin/index.html"} {
		resp, body := get(t, http.MethodGet, srv.URL+p)
		if resp.StatusCode != http.StatusOK || body != index {
			t.Errorf("%s: %d %q, want the index", p, resp.StatusCode, body)
			continue
		}
		if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Errorf("%s: Content-Type %q", p, ct)
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s: the index is cached: %q", p, cc)
		}
		if got := resp.Header.Get("Content-Security-Policy"); got != web.CSP {
			t.Errorf("%s: CSP %q, want the pages' %q", p, got, web.CSP)
		}
		if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options %q", p, got)
		}
	}
	for p, ct := range map[string]string{"/admin/assets/index-abc123.js": "text/javascript; charset=utf-8", "/admin/assets/index-abc.css": "text/css; charset=utf-8"} {
		resp, _ := get(t, http.MethodGet, srv.URL+p)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != ct {
			t.Errorf("%s: %d %q, want 200 %q", p, resp.StatusCode, resp.Header.Get("Content-Type"), ct)
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
			t.Errorf("%s: Cache-Control %q", p, cc)
		}
	}
	for _, p := range []string{"/admin/assets/index-old.js", "/admin/assets/", "/admin/assets/nested/x.js"} {
		if resp, _ := get(t, http.MethodGet, srv.URL+p); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, resp.StatusCode)
		}
	}
	if resp, body := get(t, http.MethodHead, srv.URL+"/admin"); resp.StatusCode != http.StatusOK || body != "" {
		t.Errorf("HEAD /admin: %d %q", resp.StatusCode, body)
	}
	if resp, _ := get(t, http.MethodPost, srv.URL+"/admin"); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /admin: %d, want 405", resp.StatusCode)
	}
}

// TestAdminBundleNoInline reads the bundle in this binary: nothing the CSP would refuse. Without a
// bundle (a plain go test) it skips, unless GRAVEL_ADMIN_BUILT says one was built (CI's admin
// job), when a missing bundle is the failure.
func TestAdminBundleNoInline(t *testing.T) {
	files := admin.Bundle()
	if !admin.New(files).Built() {
		if os.Getenv("GRAVEL_ADMIN_BUILT") != "" {
			t.Fatal("GRAVEL_ADMIN_BUILT is set but the bundle has no index.html: run make admin")
		}
		t.Skip("no admin bundle in this build (make admin builds one)")
	}
	pages := 0
	err := fs.WalkDir(files, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(name, ".html") {
			return err
		}
		pages++
		b, err := fs.ReadFile(files, name)
		if err != nil {
			return err
		}
		for _, problem := range inline(string(b)) {
			t.Errorf("%s: %s", name, problem)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if pages == 0 {
		t.Fatal("the bundle has no HTML to check")
	}
}

// The check itself finds what it is for.
func TestInlineFinds(t *testing.T) {
	for doc, want := range map[string]int{
		index:                                                      0,
		`<script>alert(1)</script>`:                                1,
		`<p style="color:red">x</p>`:                               1,
		`<a href=" JavaScript:alert(1)">x</a>`:                     1,
		`<button onclick="go()">x</button>`:                        1,
		`<style>p{}</style><script src="/x.js">1</script>`:         2,
		`<link rel="stylesheet" href="/admin/assets/a.css"/>`:      0,
		`<script type="module" src="/admin/assets/a.js"></script>`: 0,
	} {
		if got := inline(doc); len(got) != want {
			t.Errorf("%q: %d problems %q, want %d", doc, len(got), got, want)
		}
	}
}

// inline lists what the pages' CSP would refuse in doc: a script or style element with content, a
// script without src, a style attribute, an event handler attribute, a javascript: URL.
func inline(doc string) []string {
	var problems []string
	z := html.NewTokenizer(strings.NewReader(doc))
	var open string
	for {
		switch z.Next() {
		case html.ErrorToken:
			return problems
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := z.Token()
			hasSrc := false
			for _, a := range tok.Attr {
				switch {
				case a.Key == "style":
					problems = append(problems, "a style attribute on <"+tok.Data+">")
				case strings.HasPrefix(a.Key, "on"):
					problems = append(problems, "an event handler "+a.Key+" on <"+tok.Data+">")
				case strings.HasPrefix(strings.ToLower(strings.TrimSpace(a.Val)), "javascript:"):
					problems = append(problems, "a javascript: URL in "+a.Key+" on <"+tok.Data+">")
				case a.Key == "src":
					hasSrc = true
				}
			}
			switch tok.Data {
			case "script":
				open = "script"
				if !hasSrc {
					problems = append(problems, "an inline <script>")
					open = "" // reported once
				}
			case "style":
				problems = append(problems, "a <style> element")
			}
		case html.TextToken:
			if open == "script" && strings.TrimSpace(string(z.Text())) != "" {
				problems = append(problems, "a <script src> with inline content")
			}
		case html.EndTagToken:
			open = ""
		}
	}
}
