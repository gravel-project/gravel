package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

func TestOwnershipTransfer(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	t0 := time.Date(2026, 10, 13, 22, 0, 0, 0, time.UTC)
	member := func(name, subject string) uuid.UUID {
		t.Helper()
		u, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: name}, ident("discord", subject, t0))
		if err != nil {
			t.Fatal(err)
		}
		return u.ID
	}
	jo, sam, kim := member("Jo", "1"), member("Sam", "2"), member("Kim", "3")
	if err := st.SetClaimToken(ctx, o.ID, []byte("h"), t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOrganization(ctx, o.ID, []byte("h"), store.Actor{UserID: &jo}, t0); err != nil {
		t.Fatal(err)
	}
	as := func(id uuid.UUID) store.Actor { return store.Actor{UserID: &id, Via: store.ViaAPI} }
	week := t0.Add(7 * 24 * time.Hour)

	if _, err := st.NominateOwner(ctx, o.ID, sam, as(kim), t0, week); !errors.Is(err, store.ErrNotOwner) {
		t.Errorf("a member nominates: %v", err)
	}
	if _, err := st.NominateOwner(ctx, o.ID, jo, as(jo), t0, week); !errors.Is(err, store.ErrSelfNomination) {
		t.Errorf("self-nomination: %v", err)
	}
	if _, err := st.NominateOwner(ctx, o.ID, uuid.New(), as(jo), t0, week); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("not a member: %v", err)
	}
	if _, err := st.NominateOwner(ctx, o.ID, kim, as(jo), t0, week); err != nil {
		t.Fatal(err)
	}
	n, err := st.NominateOwner(ctx, o.ID, sam, as(jo), t0.Add(time.Minute), week)
	if err != nil || n.Pending == nil || n.Pending.UserID != sam || n.Pending.Name != "Sam" || !n.Pending.ExpiresAt.Equal(week) {
		t.Fatalf("the replacing nomination: %+v %v", n.Pending, err)
	}
	if _, _, err := st.AcceptOwnership(ctx, o.ID, as(kim), t0.Add(2*time.Minute)); !errors.Is(err, store.ErrNoNomination) {
		t.Errorf("the replaced nominee accepts: %v", err)
	}
	if _, _, err := st.AcceptOwnership(ctx, o.ID, as(sam), week); !errors.Is(err, store.ErrNominationExpired) {
		t.Errorf("an accept at the expiry: %v", err)
	}
	if _, err := st.CancelOwnershipTransfer(ctx, o.ID, as(kim), t0); !errors.Is(err, store.ErrNotOwner) {
		t.Errorf("a member cancels: %v", err)
	}
	c, err := st.CancelOwnershipTransfer(ctx, o.ID, as(jo), t0.Add(3*time.Minute))
	if err != nil || c.Pending != nil {
		t.Fatalf("the cancel: %+v %v", c.Pending, err)
	}
	if _, _, err := st.AcceptOwnership(ctx, o.ID, as(sam), t0.Add(4*time.Minute)); !errors.Is(err, store.ErrNoNomination) {
		t.Errorf("an accept after the cancel: %v", err)
	}
	if _, err := st.CancelOwnershipTransfer(ctx, o.ID, as(jo), t0); !errors.Is(err, store.ErrNoNomination) {
		t.Errorf("a cancel with nothing to cancel: %v", err)
	}

	// Two accepts at once: exactly one owner, and it is Sam.
	if _, err := st.NominateOwner(ctx, o.ID, sam, as(jo), t0.Add(5*time.Minute), week); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Go(func() { _, _, errs[i] = st.AcceptOwnership(ctx, o.ID, as(sam), t0.Add(6*time.Minute)) })
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, store.ErrNoNomination):
			t.Errorf("a racing accept: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d accepts went through, want 1", ok)
	}
	after, err := st.GetBuiltinOrganization(ctx)
	if err != nil || after.OwnerUserID == nil || *after.OwnerUserID != sam || after.Pending != nil {
		t.Fatalf("after the accept: owner %v, pending %+v, %v", after.OwnerUserID, after.Pending, err)
	}
	// Jo is an admin, granted by Sam; the log has every step.
	grants, err := st.RoleGrants(ctx, []uuid.UUID{jo})
	if err != nil || len(grants) != 1 || grants[0].Role != store.RoleAdmin || grants[0].GameID != "" || grants[0].GrantedBy == nil || *grants[0].GrantedBy != sam {
		t.Errorf("the old owner's grants: %+v %v", grants, err)
	}
	all := eventKinds(t, st, o.ID, store.EventQuery{Kinds: []string{"ownership", "role"}, Limit: 50})
	want := []string{"role.granted", "ownership.transferred", "ownership.nominated", "ownership.transfer_cancelled", "ownership.nominated", "ownership.nominated", "ownership.claimed"}
	if len(all) != len(want) {
		t.Fatalf("events %v, want %v", all, want)
	}
	for i := range want {
		if all[i] != want[i] {
			t.Fatalf("events %v, want %v", all, want)
		}
	}
	// The nominee declines a fresh nomination.
	if _, err := st.NominateOwner(ctx, o.ID, kim, as(sam), t0.Add(7*time.Minute), week); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CancelOwnershipTransfer(ctx, o.ID, as(kim), t0.Add(8*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if k := eventKinds(t, st, o.ID, store.EventQuery{Kinds: []string{"ownership"}, Limit: 1}); k[0] != "ownership.declined" {
		t.Errorf("the decline: %v", k)
	}
}
