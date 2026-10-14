package web_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// People moved to the admin dashboard (gravel#203): /members sends an old link there, and the
// header links to it for those who grant roles, as it linked to the templ page.
func TestMembersMovedToAdminPeople(t *testing.T) {
	r := newRig(t)
	jo, sam := browser(t), browser(t)
	r.loginAs(t, sam, "sam")
	r.loginAs(t, jo, "jo")
	if resp, _ := get(t, sam, r.srv.URL+"/members"); resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != "/admin/people" {
		t.Errorf("/members: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if _, acct := get(t, sam, r.srv.URL+"/account"); strings.Contains(acct, `href="/admin/people"`) {
		t.Error("a member who grants nothing is linked to People")
	}
	_, acct := get(t, jo, r.srv.URL+"/account")
	post(t, jo, r.srv.URL+"/account/claim", url.Values{"_csrf": {csrfOf(t, acct)}, "token": {r.token}}, nil)
	if _, acct = get(t, jo, r.srv.URL+"/account"); !strings.Contains(acct, `<a href="/admin/people">People</a>`) {
		t.Errorf("the owner's header: %s", acct)
	}
	for _, path := range []string{"/members/role", "/members/nominate"} {
		if resp, _ := post(t, jo, r.srv.URL+path, url.Values{"_csrf": {csrfOf(t, acct)}}, nil); resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s still answers: %d", path, resp.StatusCode)
		}
	}
}
