package web_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/web/templates"
)

// Linking Xbox from the account page (gravel#217): a link, never a login; proven through the
// member's own Discord account; a Discord account with no Xbox says so.
func TestXboxLink(t *testing.T) {
	r := newRigWith(t, templates.Theme{}, true, withXbox(map[string]identity.Account{
		"own":    {Subject: "3062899446890994", DisplayName: "Hidden Token", Method: identity.MethodOAuth2, Via: &identity.Via{Provider: "discord", Subject: "1"}},
		"sams":   {Subject: "3062899446890995", DisplayName: "Not Jo's", Method: identity.MethodOAuth2, Via: &identity.Via{Provider: "discord", Subject: "2"}},
		"no-box": {},
	}))
	c := browser(t)
	r.loginAs(t, c, "jo")

	// No login with Xbox, and none offered.
	if resp, _ := get(t, browser(t), r.srv.URL+"/auth/xbox/start"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("an Xbox login: %d", resp.StatusCode)
	}
	if _, body := get(t, browser(t), r.srv.URL+"/login"); strings.Contains(body, "/auth/xbox/") {
		t.Error("the login page offers Xbox")
	}
	_, body := get(t, c, r.srv.URL+"/account")
	if !strings.Contains(body, `href="/auth/xbox/link">Link Xbox</a>`) {
		t.Errorf("the account page offers no Xbox link: %s", body)
	}

	callback := func(code string) (reply, string) {
		t.Helper()
		resp, _ := get(t, c, r.srv.URL+"/auth/xbox/link")
		loc, _ := url.Parse(resp.Header.Get("Location"))
		if resp.StatusCode != http.StatusSeeOther || loc.Host != "xbox.example" {
			t.Fatalf("link start: %d %s", resp.StatusCode, loc)
		}
		return get(t, c, r.srv.URL+"/auth/xbox/callback?code="+code+"&state="+url.QueryEscape(loc.Query().Get("state")))
	}

	// Through Sam's Discord account: refused, saying whose to sign in to.
	if resp, body := callback("sams"); resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "Sign in to Discord as the account you linked here, then link Xbox again.") {
		t.Errorf("through another Discord account: %d %s", resp.StatusCode, body)
	}
	// A Discord account with no Xbox connected.
	r.xbox.CompleteErr = identity.ErrNoAccount
	if resp, body := callback("no-box"); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "No Xbox account") ||
		!strings.Contains(body, "has no Xbox account connected") {
		t.Errorf("no Xbox: %d %s", resp.StatusCode, body)
	}
	r.xbox.CompleteErr = nil

	// Through Jo's own: linked, with the gamertag.
	if resp, _ := callback("own"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("link callback: %d", resp.StatusCode)
	}
	_, body = get(t, c, r.srv.URL+"/account")
	if !strings.Contains(body, "Xbox account Hidden Token linked") || !strings.Contains(body, "Unlink Xbox") || strings.Contains(body, "Link Xbox") {
		t.Errorf("account after the link: %s", body)
	}
}
