package web_test

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// verify drives the Linked Roles flow for a code from the verification URL, as a member
// arriving from Discord does, and returns the callback response.
func (r *rig) verify(t *testing.T, c *http.Client, code string) reply {
	t.Helper()
	resp, _ := get(t, c, r.srv.URL+"/auth/discord/roles")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("roles start: %d", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc.Host != "discord.example" || loc.Query().Get("publish") != "1" {
		t.Fatalf("roles redirect: %s", loc)
	}
	resp, _ = get(t, c, r.srv.URL+"/auth/discord/callback?code="+code+"&state="+url.QueryEscape(loc.Query().Get("state")))
	return resp
}

// sessionCookie is the last session cookie a response set: a replaced session is cleared,
// then issued.
func sessionCookie(resp reply) (value string, set bool) {
	for _, ck := range resp.Cookies() {
		if ck.Name == "gravel_session" {
			value, set = ck.Value, true
		}
	}
	return value, set
}

func TestLinkedRolesVerification(t *testing.T) {
	r := newRig(t)
	c := browser(t)

	// From Discord, not logged in: the flow logs the member in (registering them) and publishes.
	resp := r.verify(t, c, "jo")
	if v, ok := sessionCookie(resp); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/account" || !ok || v == "" {
		t.Fatalf("callback: %d %s session=%v", resp.StatusCode, resp.Header.Get("Location"), ok)
	}
	_, body := get(t, c, r.srv.URL+"/account")
	for _, want := range []string{"Discord now knows you have no other accounts linked", "Link Steam", `href="/auth/discord/roles"`, "Update Discord linked roles"} {
		if !strings.Contains(body, want) {
			t.Errorf("account after verification should contain %q", want)
		}
	}
	if got := r.discord.Published(); len(got) != 1 || got[0].DisplayName != "Jo" || !slices.Equal(got[0].Providers, []string{"discord"}) {
		t.Errorf("published: %+v", got)
	}

	// Link Steam, then update: the session stays, Discord learns about Steam.
	resp, _ = get(t, c, r.srv.URL+"/auth/steam/link")
	loc, _ := url.Parse(resp.Header.Get("Location"))
	get(t, c, r.srv.URL+"/auth/steam/callback?code=jo-steam&state="+url.QueryEscape(loc.Query().Get("state")))
	resp = r.verify(t, c, "jo")
	if _, reissued := sessionCookie(resp); reissued {
		t.Error("the member's own session is kept")
	}
	_, body = get(t, c, r.srv.URL+"/account")
	if !strings.Contains(body, "Discord now knows your linked accounts: Steam.") {
		t.Errorf("account after the update: %s", body)
	}
	if got := r.discord.Published(); len(got) != 2 || !slices.Equal(got[1].Providers, []string{"discord", "steam"}) {
		t.Errorf("published: %+v", got)
	}

	// Verifying as someone else replaces the session, like a login.
	resp = r.verify(t, c, "sam")
	if v, ok := sessionCookie(resp); !ok || v == "" {
		t.Error("another member's verification issues their session")
	}
	_, body = get(t, c, r.srv.URL+"/account")
	if !strings.Contains(body, "Sam") || strings.Contains(body, "JoOnSteam") {
		t.Errorf("now Sam: %s", body)
	}

	// Discord refusing the write: logged in, told so.
	r.discord.PublishErr = errors.New("401")
	r.verify(t, c, "jo")
	_, body = get(t, c, r.srv.URL+"/account")
	if !strings.Contains(body, "did not take the update to your linked roles") || !strings.Contains(body, "JoOnSteam") {
		t.Errorf("a failed publish: %s", body)
	}

	if want := []string{"discord/roles/ok", "steam/link/ok", "discord/roles/ok", "discord/roles/ok", "discord/roles/ok"}; !slices.Equal(r.results, want) {
		t.Errorf("observed %v, want %v", r.results, want)
	}

	// Steam publishes nothing.
	resp, body = get(t, c, r.srv.URL+"/auth/steam/roles")
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "publishes no linked roles") {
		t.Errorf("steam roles: %d %s", resp.StatusCode, body)
	}
}
