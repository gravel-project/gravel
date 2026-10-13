// Package admin serves the admin dashboard (ADR-0029): the static client web/admin builds into
// dist/, embedded in the binary and served at /admin. It is a client of the hub's API like any
// other, signed in by the session cookie; this package only hands out its files, under the pages'
// Content-Security-Policy. A binary built without the bundle (`go build` without `make admin`)
// answers 503 at /admin and serves everything else as usual.
package admin

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"

	"github.com/gravel-project/gravel/internal/web"
)

// Prefix is the dashboard's path; no member page lives under it.
const Prefix = "/admin"

// NotBuilt is the answer of a binary built without the bundle.
const NotBuilt = "the admin dashboard was not built: build the hub with make build (or make admin first)\n"

//go:embed all:dist
var bundle embed.FS

// Bundle is the embedded bundle: dist/ as web/admin's build left it, or only its .gitkeep.
func Bundle() fs.FS {
	sub, err := fs.Sub(bundle, "dist")
	if err != nil {
		panic(err) // dist is embedded by the directive above; fs.Sub fails only on a bad name
	}
	return sub
}

// Handler serves a bundle.
type Handler struct {
	files fs.FS
	// index is the bundle's index.html, nil without a bundle.
	index []byte
}

// New serves files, a bundle: an index.html at its root and the hashed assets under assets/.
func New(files fs.FS) *Handler {
	h := &Handler{files: files}
	if b, err := fs.ReadFile(files, "index.html"); err == nil {
		h.index = b
	}
	return h
}

// Built reports whether the bundle has its index.html.
func (h *Handler) Built() bool { return h.index != nil }

// Register adds GET /admin and everything under it to mux.
func (h *Handler) Register(mux web.Mux) {
	mux.HandleFunc("GET "+Prefix, h.serve)
	mux.HandleFunc("GET "+Prefix+"/{path...}", h.serve)
}

// serve answers a file of the bundle, or index.html for any other path: the dashboard routes in
// the browser, so /admin/people is its index too. A missing file under assets/ is a 404, not the
// index, so a stale tab asking for an old hashed chunk fails visibly.
func (h *Handler) serve(w http.ResponseWriter, r *http.Request) {
	hdr := w.Header()
	hdr.Set("Content-Security-Policy", web.CSP)
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Referrer-Policy", "same-origin")
	if h.index == nil {
		hdr.Set("Content-Type", "text/plain; charset=utf-8")
		hdr.Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(NotBuilt))
		return
	}
	name := r.PathValue("path")
	if name != "" && name != "index.html" && fs.ValidPath(name) {
		if st, err := fs.Stat(h.files, name); err == nil && !st.IsDir() {
			if strings.HasPrefix(name, "assets/") {
				// Vite names every asset by its content hash: a new build is a new name.
				hdr.Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				hdr.Set("Cache-Control", "public, max-age=3600")
			}
			http.ServeFileFS(w, r, h.files, name)
			return
		}
	}
	if strings.HasPrefix(name, "assets/") {
		http.NotFound(w, r)
		return
	}
	// The index names the current assets, so a browser revalidates it every time.
	hdr.Set("Content-Type", "text/html; charset=utf-8")
	hdr.Set("Cache-Control", "no-cache")
	_, _ = w.Write(h.index)
}
