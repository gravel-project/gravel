package web_test

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/ratelimit"
)

func cookieValue(t *testing.T, c *http.Client, base, name string) string {
	t.Helper()
	u, _ := url.Parse(base)
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == name {
			return ck.Value
		}
	}
	return ""
}

// Registration, logout, a refused login and a good one, through the pages (ADR-0026).
func TestLocalRegisterLogoutLogin(t *testing.T) {
	r := newRigWith(t, testTheme(), true, withLocal(identity.LocalPolicy{RegistrationOpen: true}, nil))
	c := browser(t)
	resp, body := get(t, c, r.srv.URL+"/login")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `action="/auth/local/login"`) || !strings.Contains(body, `href="/auth/local/register"`) ||
		!strings.Contains(body, "Log in with Discord") || strings.Index(body, "Log in with Discord") > strings.Index(body, "local-heading") {
		t.Fatalf("login page: %d %s", resp.StatusCode, body)
	}
	if resp, body = get(t, c, r.srv.URL+"/auth/local/register"); resp.StatusCode != http.StatusOK || !strings.Contains(body, `autocomplete="new-password"`) || !strings.Contains(body, "At least 12 characters") {
		t.Fatalf("register page: %d %s", resp.StatusCode, body)
	}
	resp, body = post(t, c, r.srv.URL+"/auth/local/register", url.Values{"username": {"Neckbeard"}, "password": {"short"}}, nil)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "A password has at least 12 characters.") || !strings.Contains(body, `value="Neckbeard"`) {
		t.Fatalf("weak password: %d %s", resp.StatusCode, body)
	}
	resp, _ = post(t, c, r.srv.URL+"/auth/local/register", url.Values{"username": {"Neckbeard"}, "password": {"an old game and a few servers"}}, nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/account" {
		t.Fatalf("register: %d %v", resp.StatusCode, resp.Header)
	}
	if got := cookieValue(t, c, r.srv.URL, "gravel_last_provider"); got != "local" {
		t.Errorf("last provider = %q", got)
	}
	_, body = get(t, c, r.srv.URL+"/account")
	if !strings.Contains(body, "Welcome, neckbeard") || !strings.Contains(body, "Username and password") || strings.Contains(body, `action="/account/local"`) {
		t.Errorf("account: %s", body)
	}

	// A second browser: the username is taken.
	other := browser(t)
	if resp, body = post(t, other, r.srv.URL+"/auth/local/register", url.Values{"username": {"NECKBEARD"}, "password": {"a different long password"}}, nil); resp.StatusCode != http.StatusConflict || !strings.Contains(body, "That username is taken") {
		t.Errorf("taken: %d %s", resp.StatusCode, body)
	}

	_, body = get(t, c, r.srv.URL+"/account")
	if resp, _ = post(t, c, r.srv.URL+"/auth/logout", url.Values{"_csrf": {csrfOf(t, body)}}, nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	// The browser used a password last, so the form comes first now.
	if _, body = get(t, c, r.srv.URL+"/login"); strings.Index(body, "local-heading") > strings.Index(body, "Log in with Discord") {
		t.Errorf("the last-used form is not first: %s", body)
	}
	_, wrong := post(t, c, r.srv.URL+"/auth/local/login", url.Values{"username": {"neckbeard"}, "password": {"not the password at all"}}, nil)
	resp, unknown := post(t, c, r.srv.URL+"/auth/local/login", url.Values{"username": {"nobody"}, "password": {"not the password at all"}}, nil)
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(wrong, "don&#39;t match an account") || !strings.Contains(unknown, "don&#39;t match an account") || !strings.Contains(unknown, `value="nobody"`) {
		t.Errorf("refused: %d\n%s\n%s", resp.StatusCode, wrong, unknown)
	}
	resp, _ = post(t, c, r.srv.URL+"/auth/local/login", url.Values{"username": {" Neckbeard "}, "password": {"an old game and a few servers"}}, nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/account" {
		t.Fatalf("login: %d %v", resp.StatusCode, resp.Header)
	}
	if _, body = get(t, c, r.srv.URL+"/account"); !strings.Contains(body, "Logged in as neckbeard") {
		t.Errorf("after login: %s", body)
	}
	want := []string{"invalid", "ok", "invalid", "refused", "refused", "ok"}
	if !slices.Equal(r.locals, want) {
		t.Errorf("observed %v, want %v", r.locals, want)
	}
}

// Registration is the host's choice: closed, there is no registration page and no link to one.
func TestLocalRegistrationClosed(t *testing.T) {
	r := newRigWith(t, testTheme(), true, withLocal(identity.LocalPolicy{}, nil))
	c := browser(t)
	if _, body := get(t, c, r.srv.URL+"/login"); !strings.Contains(body, `action="/auth/local/login"`) || strings.Contains(body, "/auth/local/register") {
		t.Errorf("login page: %s", body)
	}
	if resp, _ := get(t, c, r.srv.URL+"/auth/local/register"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET register: %d", resp.StatusCode)
	}
	if resp, _ := post(t, c, r.srv.URL+"/auth/local/register", url.Values{"username": {"jo.plays"}, "password": {"a long enough password"}}, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST register: %d", resp.StatusCode)
	}
	if len(r.idSt.Users) != 0 {
		t.Error("a closed registration made a member")
	}
}

// A member who signed in with Discord sets a username and password on the account page, then logs
// in with it as the same member.
func TestLocalSetFromAccount(t *testing.T) {
	r := newRigWith(t, testTheme(), true, withLocal(identity.LocalPolicy{MinPasswordLength: 10}, nil))
	c := browser(t)
	r.loginAs(t, c, "jo")
	_, body := get(t, c, r.srv.URL+"/account")
	if !strings.Contains(body, `action="/account/local"`) || !strings.Contains(body, "At least 10 characters") {
		t.Fatalf("account form: %s", body)
	}
	if resp, _ := post(t, c, r.srv.URL+"/account/local", url.Values{"username": {"jo.plays"}, "password": {"ten chars!"}}, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("no csrf: %d", resp.StatusCode)
	}
	resp, body := post(t, c, r.srv.URL+"/account/local", url.Values{"_csrf": {csrfOf(t, body)}, "username": {"Jo.Plays"}, "password": {"ten chars!"}}, hx)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "You can log in as jo.plays with your password too.") || strings.Contains(body, `action="/account/local"`) {
		t.Fatalf("set: %d %s", resp.StatusCode, body)
	}
	jo := r.idSt.Identities["discord/1"].UserID
	fresh := browser(t)
	if resp, _ := post(t, fresh, r.srv.URL+"/auth/local/login", url.Values{"username": {"jo.plays"}, "password": {"ten chars!"}}, nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	if got := r.idSt.Identities["local/jo.plays"].UserID; got != jo {
		t.Errorf("the password logs in %s, not Jo %s", got, jo)
	}
}

// With local accounts off there is no form and the routes answer 404.
func TestLocalOff(t *testing.T) {
	r := newRig(t)
	c := browser(t)
	if _, body := get(t, c, r.srv.URL+"/login"); strings.Contains(body, "/auth/local/") {
		t.Errorf("login page offers local: %s", body)
	}
	if resp, _ := post(t, c, r.srv.URL+"/auth/local/login", url.Values{"username": {"jo.plays"}, "password": {"x"}}, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("login: %d", resp.StatusCode)
	}
	if resp, _ := get(t, c, r.srv.URL+"/auth/local/register"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("register: %d", resp.StatusCode)
	}
}

// A hub with no provider at all and local accounts on offers the form: the old-school floor
// (ADR-0025).
func TestLocalOnlyHubOffersTheForm(t *testing.T) {
	r := newRigWith(t, testTheme(), false, withLocal(identity.LocalPolicy{}, nil))
	c := browser(t)
	_, body := get(t, c, r.srv.URL+"/login")
	if !strings.Contains(body, `action="/auth/local/login"`) || strings.Contains(body, "No login provider") || strings.Contains(body, "Log in with") {
		t.Errorf("login page: %s", body)
	}
	if _, body = get(t, c, r.srv.URL+"/servers"); !strings.Contains(body, `href="/login"`) {
		t.Errorf("no way to log in from the pages: %s", body)
	}
}

// Logins hash a password each, so an address gets few of them.
func TestLocalRateLimit(t *testing.T) {
	r := newRigWith(t, testTheme(), false, withLocal(identity.LocalPolicy{}, ratelimit.New(1, 1)))
	c := browser(t)
	form := url.Values{"username": {"nobody"}, "password": {"whatever it is"}}
	if resp, _ := post(t, c, r.srv.URL+"/auth/local/login", form, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("first: %d", resp.StatusCode)
	}
	resp, body := post(t, c, r.srv.URL+"/auth/local/login", form, nil)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" || !strings.Contains(body, "Too many tries") {
		t.Errorf("second: %d %v %s", resp.StatusCode, resp.Header, body)
	}
	if !slices.Equal(r.locals, []string{"refused", "rate_limited"}) {
		t.Errorf("observed %v", r.locals)
	}
}
