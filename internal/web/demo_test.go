package web_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gravel-project/gravel/internal/web/templates"
)

// A demo hub's pages (gravel#112) say on every page, before the header, that the data is made
// up: a plain element the stylesheet styles, so it holds under the CSP and in both themes. An
// htmx swap keeps it, since it sits outside the swapped main. A hub that is not a demo shows
// none.
func TestDemoBanner(t *testing.T) {
	r := newRig(t)
	c := browser(t)
	const banner = `<section class="demo" aria-label="Demo hub"><p>` + templates.DemoNotice + `</p></section>`

	for _, page := range []string{"/servers", "/boards", "/login"} {
		if _, body := get(t, c, r.srv.URL+page); strings.Contains(body, templates.DemoNotice) {
			t.Errorf("%s on a hub that is not a demo has the banner", page)
		}
	}
	r.org.WithDemo(true)
	for _, page := range []string{"/servers", "/boards", "/login", "/servers/htg-wardogs-1", "/servers/no-such-server"} {
		resp, body := get(t, c, r.srv.URL+page)
		if !strings.Contains(body, banner) {
			t.Errorf("%s (%d) has no demo banner", page, resp.StatusCode)
			continue
		}
		if strings.Index(body, banner) > strings.Index(body, `<header class="top">`) {
			t.Errorf("%s: the banner is not above the header", page)
		}
		if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "style-src 'self'") || strings.Contains(body, "style=") {
			t.Errorf("%s: the banner needs an inline style: %q", page, csp)
		}
	}
	// htmx asks for the content alone; the banner stays where it is, in the page around it.
	if resp, body := get(t, c, r.srv.URL+"/servers", "HX-Request", "true"); resp.StatusCode != http.StatusOK || strings.Contains(body, templates.DemoNotice) {
		t.Errorf("an htmx swap carries the banner into main: %d", resp.StatusCode)
	}
	if _, css := get(t, c, r.srv.URL+"/static/app.css"); !strings.Contains(css, ".demo {") || !strings.Contains(css, "var(--fg)") {
		t.Error("the stylesheet does not style the banner from the theme's tokens")
	}
}
