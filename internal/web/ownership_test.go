package web_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The handover through the pages (#197): the owner nominates a member on Members, the member
// accepts on their account page, and the old owner is an admin afterwards.
func TestOwnershipTransferPages(t *testing.T) {
	r := newRig(t)
	jo, sam := browser(t), browser(t)
	r.loginAs(t, sam, "sam")
	r.loginAs(t, jo, "jo")
	_, acct := get(t, jo, r.srv.URL+"/account")
	post(t, jo, r.srv.URL+"/account/claim", url.Values{"_csrf": {csrfOf(t, acct)}, "token": {r.token}}, nil)

	_, body := get(t, jo, r.srv.URL+"/members")
	if !strings.Contains(body, "Hand the hub to Sam") || strings.Contains(body, "Hand the hub to Jo") {
		t.Fatalf("the owner's Members page: %s", body)
	}
	samID := regexpFind(t, body, `href="/members/([^"]+)">Sam</a>`)
	hx := map[string]string{"HX-Request": "true"}
	resp, body := post(t, jo, r.srv.URL+"/members/nominate", url.Values{"_csrf": {csrfOf(t, body)}, "user_id": {samID}}, hx)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Sam has until ") || !strings.Contains(body, "nominated to own") || strings.Contains(body, "Hand the hub to Sam") {
		t.Fatalf("the nomination: %d %s", resp.StatusCode, body)
	}
	if _, body = get(t, jo, r.srv.URL+"/account"); !strings.Contains(body, "Handing the hub over") || !strings.Contains(body, "You nominated Sam") || !strings.Contains(body, "Cancel the nomination") {
		t.Errorf("the owner's account page: %s", body)
	}

	_, samAcct := get(t, sam, r.srv.URL+"/account")
	if !strings.Contains(samAcct, "You were nominated to own this hub") || !strings.Contains(samAcct, "Accept: own this hub") || !strings.Contains(samAcct, ">Decline<") {
		t.Fatalf("the nominee's account page: %s", samAcct)
	}
	resp, _ = post(t, sam, r.srv.URL+"/account/ownership/accept", url.Values{"_csrf": {csrfOf(t, samAcct)}}, nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("accept: %d", resp.StatusCode)
	}
	if _, samAcct = get(t, sam, r.srv.URL+"/account"); !strings.Contains(samAcct, "You own this hub now.") || strings.Contains(samAcct, "You were nominated") {
		t.Errorf("after accepting: %s", samAcct)
	}
	// Sam owns the hub and can hand it on; Jo is an admin and can't.
	if _, body = get(t, sam, r.srv.URL+"/members"); !strings.Contains(body, "Hand the hub to Jo") || !strings.Contains(body, ">admin<") {
		t.Errorf("the new owner's Members page: %s", body)
	}
	if _, body = get(t, jo, r.srv.URL+"/members"); strings.Contains(body, "Hand the hub to") || !strings.Contains(body, ">admin<") {
		t.Errorf("the old owner (an admin, with no handover): %s", body)
	}
	resp, _ = post(t, jo, r.srv.URL+"/members/nominate", url.Values{"_csrf": {csrfOf(t, body)}, "user_id": {samID}}, nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("the old owner's nomination: %d", resp.StatusCode)
	}
	if _, body = get(t, jo, r.srv.URL+"/members"); !strings.Contains(body, "You can&#39;t nominate that member") {
		t.Errorf("the old owner is told: %s", body)
	}
}

// The nominee may decline, and the owner cancel; neither changes the owner.
func TestOwnershipDeclineAndCancel(t *testing.T) {
	r := newRig(t)
	jo, sam := browser(t), browser(t)
	r.loginAs(t, sam, "sam")
	r.loginAs(t, jo, "jo")
	_, acct := get(t, jo, r.srv.URL+"/account")
	post(t, jo, r.srv.URL+"/account/claim", url.Values{"_csrf": {csrfOf(t, acct)}, "token": {r.token}}, nil)
	_, body := get(t, jo, r.srv.URL+"/members")
	samID := regexpFind(t, body, `href="/members/([^"]+)">Sam</a>`)
	nominate := func() {
		t.Helper()
		_, body := get(t, jo, r.srv.URL+"/members")
		if resp, _ := post(t, jo, r.srv.URL+"/members/nominate", url.Values{"_csrf": {csrfOf(t, body)}, "user_id": {samID}}, nil); resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("nominate: %d", resp.StatusCode)
		}
	}

	nominate()
	_, samAcct := get(t, sam, r.srv.URL+"/account")
	post(t, sam, r.srv.URL+"/account/ownership/decline", url.Values{"_csrf": {csrfOf(t, samAcct)}}, nil)
	if _, samAcct = get(t, sam, r.srv.URL+"/account"); !strings.Contains(samAcct, "You declined: nothing changed.") || strings.Contains(samAcct, "You were nominated") {
		t.Errorf("after declining: %s", samAcct)
	}
	nominate()
	_, acct = get(t, jo, r.srv.URL+"/account")
	post(t, jo, r.srv.URL+"/account/ownership/cancel", url.Values{"_csrf": {csrfOf(t, acct)}}, nil)
	if _, acct = get(t, jo, r.srv.URL+"/account"); !strings.Contains(acct, "Nomination cancelled: nothing changed.") || strings.Contains(acct, "Handing the hub over") {
		t.Errorf("after cancelling: %s", acct)
	}
	if _, samAcct = get(t, sam, r.srv.URL+"/account"); strings.Contains(samAcct, "You were nominated") {
		t.Errorf("the cancelled nominee still sees it: %s", samAcct)
	}
}
