package web_test

import (
	"strings"
	"testing"

	"connectrpc.com/connect"
)

// A hub with no login provider (a demo hub, gravel#146) offers no login anywhere: no "Log in" in
// the header, and the server page says why who's on is hidden instead of pointing at a login
// that can't happen. A hub with one keeps both.
func TestNoLoginProviderOffersNoLogin(t *testing.T) {
	for _, c := range []struct {
		name          string
		login         bool
		header, wants string
	}{
		{"with a provider", true, `<a href="/login">Log in</a>`, "Log in to see who&#39;s on."},
		{"without one", false, "", "Only members can see who&#39;s on, and this hub has no login."},
	} {
		r := newRigWith(t, testTheme(), c.login)
		r.servers.playersErr = connect.CodeUnauthenticated
		b := browser(t)
		for _, page := range []string{"/servers", "/boards", "/servers/htg-wardogs-1"} {
			_, body := get(t, b, r.srv.URL+page)
			if has := strings.Contains(body, `href="/login"`); has != c.login {
				t.Errorf("%s: %s links to /login: %t", c.name, page, has)
			}
			if c.header != "" && !strings.Contains(body, c.header) {
				t.Errorf("%s: %s has no Log in link in the header", c.name, page)
			}
		}
		if _, body := get(t, b, r.srv.URL+"/servers/htg-wardogs-1"); !strings.Contains(body, c.wants) {
			t.Errorf("%s: the server page lacks %q", c.name, c.wants)
		}
	}
}
