package web_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/store"
)

// idOf is the member a browser is logged in as: /profile sends them to /members/<id>.
func idOf(t *testing.T, c *http.Client, r *rig) uuid.UUID {
	t.Helper()
	resp, _ := get(t, c, r.srv.URL+"/profile")
	id, err := uuid.Parse(strings.TrimPrefix(resp.Header.Get("Location"), "/members/"))
	if err != nil {
		t.Fatalf("/profile sent the member to %q", resp.Header.Get("Location"))
	}
	return id
}

// nominate is the owner's nomination, as /admin/people makes it through the API (gravel#203).
func (r *rig) nominate(t *testing.T, by, nominee uuid.UUID) error {
	t.Helper()
	_, err := r.org.NominateOwner(context.Background(), nominee, store.Actor{UserID: &by, Via: store.ViaAPI})
	return err
}

// The handover through the pages (#197): the owner nominates a member (on /admin/people since
// #203), the member accepts on their account page, and the old owner is an admin afterwards.
func TestOwnershipTransferPages(t *testing.T) {
	r := newRig(t)
	jo, sam := browser(t), browser(t)
	r.loginAs(t, sam, "sam")
	r.loginAs(t, jo, "jo")
	_, acct := get(t, jo, r.srv.URL+"/account")
	post(t, jo, r.srv.URL+"/account/claim", url.Values{"_csrf": {csrfOf(t, acct)}, "token": {r.token}}, nil)

	joID, samID := idOf(t, jo, r), idOf(t, sam, r)
	if err := r.nominate(t, joID, samID); err != nil {
		t.Fatal(err)
	}
	var body string
	if _, body = get(t, jo, r.srv.URL+"/account"); !strings.Contains(body, "Handing the hub over") || !strings.Contains(body, "You nominated Sam") || !strings.Contains(body, "Cancel the nomination") {
		t.Errorf("the owner's account page: %s", body)
	}

	_, samAcct := get(t, sam, r.srv.URL+"/account")
	if !strings.Contains(samAcct, "You were nominated to own this hub") || !strings.Contains(samAcct, "Accept: own this hub") || !strings.Contains(samAcct, ">Decline<") {
		t.Fatalf("the nominee's account page: %s", samAcct)
	}
	resp, _ := post(t, sam, r.srv.URL+"/account/ownership/accept", url.Values{"_csrf": {csrfOf(t, samAcct)}}, nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("accept: %d", resp.StatusCode)
	}
	if _, samAcct = get(t, sam, r.srv.URL+"/account"); !strings.Contains(samAcct, "You own this hub now.") || strings.Contains(samAcct, "You were nominated") {
		t.Errorf("after accepting: %s", samAcct)
	}
	// Sam owns the hub and can hand it on; Jo is an admin and can't.
	j, err := r.ids.Me(context.Background(), joID)
	if err != nil || !slices.ContainsFunc(j.Grants, func(g store.RoleGrant) bool { return g.Role == store.RoleAdmin && g.GameID == "" }) {
		t.Errorf("the old owner is not an admin: %+v %v", j.Grants, err)
	}
	if err := r.nominate(t, joID, samID); !errors.Is(err, org.ErrNotOwner) {
		t.Errorf("the old owner's nomination: %v", err)
	}
	if err := r.nominate(t, samID, joID); err != nil {
		t.Errorf("the new owner's nomination: %v", err)
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
	joID, samID := idOf(t, jo, r), idOf(t, sam, r)
	nominate := func() {
		t.Helper()
		if err := r.nominate(t, joID, samID); err != nil {
			t.Fatal(err)
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
