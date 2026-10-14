package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/store"
)

// The transfer in two steps (gravel#197, ADR-0027 §6): the owner nominates, the nominee accepts in
// person, and the old owner is an admin afterwards, without org:transfer.
func TestOwnershipTransfer(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	jo := r.user(t, "Jo", "1")
	sam := r.user(t, "Sam", "2")
	kim := r.user(t, "Kim", "3")
	if _, err := r.org.Claim(ctx, r.token, store.Actor{UserID: &jo.ID}); err != nil {
		t.Fatal(err)
	}
	client := func(id uuid.UUID) hubv1connect.OrganizationServiceClient {
		return hubv1connect.NewOrganizationServiceClient(cookieClient(http.DefaultClient, r.login(t, id)), r.srv.URL)
	}
	owner, nominee, other := client(jo.ID), client(sam.ID), client(kim.ID)
	anon := hubv1connect.NewOrganizationServiceClient(http.DefaultClient, r.srv.URL)
	nominate := func(c hubv1connect.OrganizationServiceClient, id string) (*connect.Response[hubv1.NominateOwnerResponse], error) {
		return c.NominateOwner(ctx, connect.NewRequest(&hubv1.NominateOwnerRequest{UserId: id}))
	}
	accept := func(c hubv1connect.OrganizationServiceClient) error {
		_, err := c.AcceptOwnership(ctx, connect.NewRequest(&hubv1.AcceptOwnershipRequest{}))
		return err
	}
	cancel := func(c hubv1connect.OrganizationServiceClient) error {
		_, err := c.CancelOwnershipTransfer(ctx, connect.NewRequest(&hubv1.CancelOwnershipTransferRequest{}))
		return err
	}

	// Who may nominate, and whom.
	if _, err := nominate(anon, sam.ID.String()); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("anonymous: %v", err)
	}
	if _, err := nominate(other, sam.ID.String()); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a member: %v", err)
	}
	if _, err := nominate(owner, jo.ID.String()); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("self-nomination: %v", err)
	}
	if _, err := nominate(owner, "not-an-id"); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a bad id: %v", err)
	}
	if _, err := nominate(owner, "6f1c2f8e-0000-4000-8000-000000000000"); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("not a member: %v", err)
	}
	if err := accept(nominee); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("accept with no nomination: %v", err)
	}

	// A nomination, replaced by another, then cancelled: an accept after the cancel finds nothing.
	if _, err := nominate(owner, kim.ID.String()); err != nil {
		t.Fatal(err)
	}
	got, err := nominate(owner, sam.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	o := got.Msg.GetOrganization()
	if o.GetPendingOwnerUserId() != sam.ID.String() || o.GetPendingOwnerName() != "Sam" ||
		time.Until(o.GetPendingOwnerExpiresAt().AsTime()) < 6*24*time.Hour {
		t.Errorf("the nomination: %v", o)
	}
	if err := accept(other); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("the replaced nominee accepts: %v", err)
	}
	// Only the owner and the nominee see the nomination.
	for name, c := range map[string]hubv1connect.OrganizationServiceClient{"owner": owner, "nominee": nominee, "member": other, "visitor": anon} {
		g, err := c.GetOrganization(ctx, connect.NewRequest(&hubv1.GetOrganizationRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		if sees := g.Msg.GetOrganization().GetPendingOwnerUserId() != ""; sees != (name == "owner" || name == "nominee") {
			t.Errorf("%s sees the nomination: %v", name, sees)
		}
	}
	if err := cancel(other); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a member cancels: %v", err)
	}
	if err := cancel(owner); err != nil {
		t.Fatal(err)
	}
	if err := accept(nominee); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("accept after the cancel: %v", err)
	}
	// The nominee may decline.
	if _, err := nominate(owner, sam.ID.String()); err != nil {
		t.Fatal(err)
	}
	if err := cancel(nominee); err != nil {
		t.Fatal(err)
	}
	if n := len(r.orgSt.Recorded); r.orgSt.Recorded[n-1].Kind != "ownership.declined" {
		t.Errorf("a decline is recorded as one: %+v", r.orgSt.Recorded[n-1])
	}

	// Accepted: Sam owns the hub, Jo is an admin and can no longer hand it over.
	if _, err := nominate(owner, sam.ID.String()); err != nil {
		t.Fatal(err)
	}
	if err := accept(nominee); err != nil {
		t.Fatal(err)
	}
	g, err := anon.GetOrganization(ctx, connect.NewRequest(&hubv1.GetOrganizationRequest{}))
	if err != nil || g.Msg.GetOrganization().GetOwnerUserId() != sam.ID.String() || g.Msg.GetOrganization().GetPendingOwnerUserId() != "" {
		t.Fatalf("after the accept: %v %v", g, err)
	}
	me, err := hubv1connect.NewIdentityServiceClient(cookieClient(http.DefaultClient, r.login(t, jo.ID)), r.srv.URL).GetMe(ctx, connect.NewRequest(&hubv1.GetMeRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if u := me.Msg.GetUser(); u.GetOwner() || len(u.GetRoles()) != 1 || u.GetRoles()[0] != "admin" {
		t.Errorf("the old owner: owner %v, roles %v", u.GetOwner(), u.GetRoles())
	}
	if _, err := nominate(owner, kim.ID.String()); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("the old owner nominates: %v", err)
	}
	if err := accept(nominee); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("a second accept: %v", err)
	}
	var kinds []string
	for _, e := range r.orgSt.Recorded {
		kinds = append(kinds, e.Kind)
	}
	if kinds[len(kinds)-1] != "ownership.transferred" {
		t.Errorf("recorded: %v", kinds)
	}
}

// A nomination expires after 7 days.
func TestOwnershipNominationExpires(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	jo := r.user(t, "Jo", "1")
	sam := r.user(t, "Sam", "2")
	if _, err := r.org.Claim(ctx, r.token, store.Actor{UserID: &jo.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.org.NominateOwner(ctx, sam.ID, store.Actor{UserID: &jo.ID}); err != nil {
		t.Fatal(err)
	}
	r.orgSt.Org.Pending.ExpiresAt = time.Now().Add(-time.Minute)
	nominee := hubv1connect.NewOrganizationServiceClient(cookieClient(http.DefaultClient, r.login(t, sam.ID)), r.srv.URL)
	_, err := nominee.AcceptOwnership(ctx, connect.NewRequest(&hubv1.AcceptOwnershipRequest{}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "expired") {
		t.Errorf("an expired nomination: %v", err)
	}
}
