package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

// A recovery address is proved by the link mailed to it, once, within its day, and only while the
// account still has that address; changing it unproves it and ends the open link (gravel#234).
func TestRecoveryEmail(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	now := time.Now().UTC().Truncate(time.Microsecond)
	u, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: "neckbeard"}, store.Identity{Provider: store.ProviderLocal, Subject: "neckbeard",
		VerificationMethod: "password", VerifiedAt: now, Credential: &store.LocalCredential{PasswordHash: "$argon2id$v=19$m=64,t=1,p=1$c2FsdA$a2V5", PasswordUpdatedAt: now}})
	if err != nil {
		t.Fatal(err)
	}
	addr := func(s string) *string { return &s }
	verification := func(token, email string) *store.EmailVerification {
		return &store.EmailVerification{TokenHash: linkHash(token), Subject: "neckbeard", Email: email, IssuedAt: now, ExpiresAt: now.Add(24 * time.Hour)}
	}
	cred := func() store.LocalCredential {
		t.Helper()
		c, err := st.GetLocalCredential(ctx, "neckbeard")
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	if err := st.SetRecoveryEmail(ctx, "nobody", addr("a@example.com"), nil); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown account: %v", err)
	}
	if err := st.SetRecoveryEmail(ctx, "neckbeard", addr("a@example.com"), verification("t1", "b@example.com")); err == nil {
		t.Error("a link for another address is refused")
	}
	if err := st.SetRecoveryEmail(ctx, "neckbeard", addr("a@example.com"), verification("t1", "a@example.com")); err != nil {
		t.Fatal(err)
	}
	if c := cred(); c.Email == nil || *c.Email != "a@example.com" || c.EmailVerifiedAt != nil {
		t.Errorf("set: %+v", c)
	}
	// A newer link for the same address replaces the open one.
	if err := st.SetRecoveryEmail(ctx, "neckbeard", addr("a@example.com"), verification("t2", "a@example.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetEmailVerification(ctx, linkHash("t1"), now); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the replaced link: %v", err)
	}
	if v, err := st.GetEmailVerification(ctx, linkHash("t2"), now); err != nil || v.Email != "a@example.com" || v.Subject != "neckbeard" {
		t.Fatalf("read: %+v %v", v, err)
	}
	if _, err := st.GetEmailVerification(ctx, linkHash("t2"), now.Add(24*time.Hour)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expired: %v", err)
	}
	if _, _, err := st.VerifyRecoveryEmail(ctx, linkHash("t2"), now.Add(25*time.Hour)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("verify after expiry: %v", err)
	}
	at := now.Add(time.Minute)
	v, userID, err := st.VerifyRecoveryEmail(ctx, linkHash("t2"), at)
	if err != nil || userID != u.ID || v.Email != "a@example.com" {
		t.Fatalf("verify: %+v %v %v", v, userID, err)
	}
	if c := cred(); c.EmailVerifiedAt == nil || !c.EmailVerifiedAt.Equal(at) {
		t.Errorf("verified: %+v", c)
	}
	if _, _, err := st.VerifyRecoveryEmail(ctx, linkHash("t2"), at); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second use: %v", err)
	}
	evs, err := st.ListIdentityEvents(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(evs, func(e store.IdentityEvent) bool { return e.Event == store.IdentityEventEmailVerified }) {
		t.Errorf("identity log: %+v", evs)
	}

	// The same address again keeps its proof; another loses it, and a link for the old one is
	// worthless.
	if err := st.SetRecoveryEmail(ctx, "neckbeard", addr("a@example.com"), nil); err != nil {
		t.Fatal(err)
	}
	if c := cred(); c.EmailVerifiedAt == nil {
		t.Errorf("the same address lost its proof: %+v", c)
	}
	if err := st.SetRecoveryEmail(ctx, "neckbeard", addr("b@example.com"), verification("t3", "b@example.com")); err != nil {
		t.Fatal(err)
	}
	if c := cred(); c.EmailVerifiedAt != nil || *c.Email != "b@example.com" {
		t.Errorf("a new address keeps the old proof: %+v", c)
	}
	if err := st.SetRecoveryEmail(ctx, "neckbeard", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.VerifyRecoveryEmail(ctx, linkHash("t3"), at); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("removing the address ends its link: %v", err)
	}
	if c := cred(); c.Email != nil || c.EmailVerifiedAt != nil {
		t.Errorf("removed: %+v", c)
	}

	// Expired links are pruned; a mailed password link says so on the identity log.
	if err := st.SetRecoveryEmail(ctx, "neckbeard", addr("c@example.com"), verification("t4", "c@example.com")); err != nil {
		t.Fatal(err)
	}
	if n, err := st.DeleteExpiredEmailVerifications(ctx, now.Add(25*time.Hour)); err != nil || n != 1 {
		t.Errorf("prune: %d %v", n, err)
	}
	if err := st.CreatePasswordLink(ctx, store.PasswordLink{ID: uuid.New(), TokenHash: linkHash("reset"), Subject: "neckbeard", Purpose: store.PasswordLinkReset,
		IssuedAt: now, ExpiresAt: now.Add(time.Hour), Mailed: true}); err != nil {
		t.Fatal(err)
	}
	evs, _ = st.ListIdentityEvents(ctx, u.ID)
	if last := evs[len(evs)-1]; last.Event != store.IdentityEventPasswordResetMailed {
		t.Errorf("a mailed link's event: %+v", last)
	}
}
