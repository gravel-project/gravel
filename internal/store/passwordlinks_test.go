package store_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

func linkHash(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

// A link sets its account's password once, within its hour; a newer link deletes the open ones;
// links go with their identity.
func TestPasswordLinks(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	now := time.Now().UTC().Truncate(time.Microsecond)
	hash := "$argon2id$v=19$m=64,t=1,p=1$c2FsdA$a2V5"
	u, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: "neckbeard"}, store.Identity{Provider: store.ProviderLocal, Subject: "neckbeard",
		VerificationMethod: "password", VerifiedAt: now, Credential: &store.LocalCredential{PasswordHash: hash, PasswordUpdatedAt: now}})
	if err != nil {
		t.Fatal(err)
	}
	link := func(token, purpose string, at time.Time) store.PasswordLink {
		return store.PasswordLink{ID: uuid.New(), TokenHash: linkHash(token), Subject: "neckbeard", Purpose: purpose, IssuedBy: &u.ID, IssuedAt: at, ExpiresAt: at.Add(time.Hour)}
	}
	if err := st.CreatePasswordLink(ctx, store.PasswordLink{ID: uuid.New(), TokenHash: linkHash("x"), Subject: "nobody", Purpose: "set", IssuedAt: now, ExpiresAt: now.Add(time.Hour)}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown account: %v", err)
	}
	if err := st.CreatePasswordLink(ctx, link("first", store.PasswordLinkSet, now)); err != nil {
		t.Fatal(err)
	}
	if err := st.CreatePasswordLink(ctx, link("second", store.PasswordLinkReset, now)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetPasswordLink(ctx, linkHash("first"), now); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a newer link left the first: %v", err)
	}
	got, err := st.GetPasswordLink(ctx, linkHash("second"), now)
	if err != nil || got.Subject != "neckbeard" || got.Purpose != "reset" || got.IssuedBy == nil || *got.IssuedBy != u.ID {
		t.Fatalf("get: %v %+v", err, got)
	}
	if _, err := st.GetPasswordLink(ctx, linkHash("second"), now.Add(time.Hour)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expired: %v", err)
	}
	if _, _, err := st.UsePasswordLink(ctx, linkHash("second"), now.Add(2*time.Hour), "$argon2id$x"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("use after expiry: %v", err)
	}
	if _, err := st.RecordLocalFailure(ctx, "neckbeard", now, store.Lockout{Threshold: 1, Base: time.Hour, Max: time.Hour}); err != nil {
		t.Fatal(err)
	}
	newHash := "$argon2id$v=19$m=64,t=1,p=1$bmV3$a2V5"
	used, userID, err := st.UsePasswordLink(ctx, linkHash("second"), now.Add(time.Minute), newHash)
	if err != nil || userID != u.ID || used.UsedAt == nil {
		t.Fatalf("use: %v %v %+v", err, userID, used)
	}
	c, _ := st.GetLocalCredential(ctx, "neckbeard")
	if c.PasswordHash != newHash || c.LockedUntil != nil || c.Lockouts != 0 || !c.PasswordUpdatedAt.Equal(now.Add(time.Minute)) {
		t.Errorf("credential after the link: %+v", c)
	}
	if _, _, err := st.UsePasswordLink(ctx, linkHash("second"), now.Add(2*time.Minute), "$argon2id$y"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second use: %v", err)
	}
	events, err := st.ListIdentityEventsAfter(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range events {
		names = append(names, e.Event)
	}
	if want := "registered password_reset_issued password_reset_issued password_reset"; join(names) != want {
		t.Errorf("events: %v", names)
	}
	if n, err := st.DeleteExpiredPasswordLinks(ctx, now.Add(2*time.Hour)); err != nil || n != 1 {
		t.Errorf("prune: %d %v", n, err)
	}
	// A link goes with its identity.
	if err := st.CreatePasswordLink(ctx, link("third", store.PasswordLinkReset, now)); err != nil {
		t.Fatal(err)
	}
	if err := st.LinkIdentity(ctx, func() store.Identity { i := ident("discord", "1", now); i.UserID = u.ID; return i }()); err != nil {
		t.Fatal(err)
	}
	if err := st.UnlinkIdentity(ctx, u.ID, store.ProviderLocal, "neckbeard", now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetPasswordLink(ctx, linkHash("third"), now); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the link outlived its identity: %v", err)
	}
}

func join(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += " "
		}
		out += v
	}
	return out
}
