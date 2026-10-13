package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

// A local account's credential is written with its identity, locks in one statement as failures
// reach the threshold (doubling, capped), clears on success, and goes with the identity.
func TestLocalCredentials(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	now := time.Now().UTC().Truncate(time.Microsecond)
	hash := "$argon2id$v=19$m=64,t=1,p=1$c2FsdA$a2V5"
	local := store.Identity{Provider: store.ProviderLocal, Subject: "neckbeard", DisplayName: "neckbeard", VerificationMethod: "password", VerifiedAt: now,
		Credential: &store.LocalCredential{PasswordHash: hash, PasswordUpdatedAt: now}}
	u, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: "neckbeard"}, local)
	if err != nil {
		t.Fatal(err)
	}
	c, err := st.GetLocalCredential(ctx, "neckbeard")
	if err != nil || c.PasswordHash != hash || !c.PasswordUpdatedAt.Equal(now) || c.FailedAttempts != 0 || c.LockedUntil != nil {
		t.Fatalf("credential: %v %+v", err, c)
	}
	if _, err := st.GetLocalCredential(ctx, "nobody"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	// A credential belongs to a local identity only, and a plain hash never goes in.
	bad := ident("discord", "9", now)
	bad.Credential = &store.LocalCredential{PasswordHash: hash, PasswordUpdatedAt: now}
	if _, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: "x"}, bad); err == nil {
		t.Error("a credential on a discord identity")
	}
	plain := store.Identity{Provider: store.ProviderLocal, Subject: "plain", VerificationMethod: "password", VerifiedAt: now,
		Credential: &store.LocalCredential{PasswordHash: "hunter2", PasswordUpdatedAt: now}}
	if _, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: "plain"}, plain); err == nil {
		t.Error("a password stored as its hash")
	}
	if _, err := st.GetIdentity(ctx, store.ProviderLocal, "plain"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a refused credential left its identity: %v", err)
	}

	l := store.Lockout{Threshold: 3, Base: 15 * time.Minute, Max: 40 * time.Minute}
	for i := 1; i <= 2; i++ {
		if c, err = st.RecordLocalFailure(ctx, "neckbeard", now, l); err != nil || c.FailedAttempts != i || c.LockedUntil != nil {
			t.Fatalf("failure %d: %v %+v", i, err, c)
		}
	}
	if c, err = st.RecordLocalFailure(ctx, "neckbeard", now, l); err != nil || c.FailedAttempts != 0 || c.Lockouts != 1 || c.LockedUntil == nil || !c.LockedUntil.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("first lockout: %v %+v", err, c)
	}
	for range 3 {
		c, _ = st.RecordLocalFailure(ctx, "neckbeard", now, l)
	}
	if c.Lockouts != 2 || !c.LockedUntil.Equal(now.Add(30*time.Minute)) {
		t.Errorf("second lockout doubles: %+v", c)
	}
	for range 3 {
		c, _ = st.RecordLocalFailure(ctx, "neckbeard", now, l)
	}
	if c.Lockouts != 3 || !c.LockedUntil.Equal(now.Add(40*time.Minute)) {
		t.Errorf("third lockout is capped: %+v", c)
	}
	if _, err := st.RecordLocalFailure(ctx, "nobody", now, l); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("failure of an unknown account: %v", err)
	}

	rehash := "$argon2id$v=19$m=128,t=1,p=1$c2FsdA$a2V5"
	if err := st.RecordLocalSuccess(ctx, "neckbeard", rehash); err != nil {
		t.Fatal(err)
	}
	if c, _ = st.GetLocalCredential(ctx, "neckbeard"); c.FailedAttempts != 0 || c.Lockouts != 0 || c.LockedUntil != nil || c.PasswordHash != rehash || !c.PasswordUpdatedAt.Equal(now) {
		t.Errorf("success: %+v", c)
	}
	if err := st.RecordLocalSuccess(ctx, "neckbeard", ""); err != nil {
		t.Fatal(err)
	}
	if c, _ = st.GetLocalCredential(ctx, "neckbeard"); c.PasswordHash != rehash {
		t.Errorf("a success with no rehash changed the hash: %+v", c)
	}
	if err := st.RecordLocalSuccess(ctx, "nobody", ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("success of an unknown account: %v", err)
	}

	// Linked beside another identity, then unlinked: the credential goes with it.
	if err := st.LinkIdentity(ctx, func() store.Identity { i := ident("discord", "1", now); i.UserID = u.ID; return i }()); err != nil {
		t.Fatal(err)
	}
	if err := st.UnlinkIdentity(ctx, u.ID, store.ProviderLocal, "neckbeard", now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetLocalCredential(ctx, "neckbeard"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the credential outlived its identity: %v", err)
	}
}
