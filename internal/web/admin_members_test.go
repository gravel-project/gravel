package web_test

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// The owner lists members and grants or revokes the moderator role; a member is refused, and only
// the owner sees the Members link (ADR-0013).
func TestMembersPage(t *testing.T) {
	r := newRig(t)
	jo, sam := browser(t), browser(t)
	r.loginAs(t, sam, "sam")
	r.loginAs(t, jo, "jo")

	// Before anyone owns the hub, Jo is a member like Sam.
	if resp, body := get(t, jo, r.srv.URL+"/members"); resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "Only the hub&#39;s owner") {
		t.Errorf("a member: %d", resp.StatusCode)
	}
	_, acct := get(t, jo, r.srv.URL+"/account")
	if strings.Contains(acct, `href="/members"`) {
		t.Error("a member sees the Members link")
	}
	post(t, jo, r.srv.URL+"/account/claim", url.Values{"_csrf": {csrfOf(t, acct)}, "token": {r.token}}, nil)

	resp, body := get(t, jo, r.srv.URL+"/members")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `href="/members"`) || !strings.Contains(body, ">owner<") ||
		!strings.Contains(body, "Make Sam a moderator") || strings.Contains(body, "Make Jo a moderator") || !strings.Contains(body, "Discord") {
		t.Fatalf("the owner's Members page: %d %s", resp.StatusCode, body)
	}
	samID := regexpFind(t, body, `name="user_id" value="([^"]+)"`)

	// Grant with htmx, revoke without; a form without the token is refused.
	resp, body = post(t, jo, r.srv.URL+"/members/role", url.Values{"_csrf": {csrfOf(t, body)}, "user_id": {samID}, "role": {"moderator"}, "granted": {"1"}},
		map[string]string{"HX-Request": "true"})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Sam is now a moderator.") || !strings.Contains(body, ">moderator<") ||
		!strings.Contains(body, "Remove moderator from Sam") {
		t.Errorf("grant: %d %s", resp.StatusCode, body)
	}
	resp, _ = post(t, jo, r.srv.URL+"/members/role", url.Values{"_csrf": {csrfOf(t, body)}, "user_id": {samID}, "role": {"moderator"}, "granted": {"0"}}, nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/members" {
		t.Errorf("revoke: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if _, body = get(t, jo, r.srv.URL+"/members"); !strings.Contains(body, "Sam is no longer a moderator.") || !strings.Contains(body, "Make Sam a moderator") {
		t.Errorf("after the revocation: %s", body)
	}
	if resp, _ = post(t, jo, r.srv.URL+"/members/role", url.Values{"user_id": {samID}, "role": {"moderator"}, "granted": {"1"}}, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a form without the token: %d", resp.StatusCode)
	}
	// Sam can't grant, even with a valid form of their own.
	_, samAcct := get(t, sam, r.srv.URL+"/account")
	if resp, _ = post(t, sam, r.srv.URL+"/members/role", url.Values{"_csrf": {csrfOf(t, samAcct)}, "user_id": {samID}, "role": {"moderator"}, "granted": {"1"}}, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a member granting: %d", resp.StatusCode)
	}
	if ch := r.idSt.Changes(); len(ch) != 2 {
		t.Errorf("role changes = %+v", ch)
	}
}

func regexpFind(t *testing.T, body, pattern string) string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("%s not in the page", pattern)
	}
	return m[1]
}
