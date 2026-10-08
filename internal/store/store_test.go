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

func TestMigrateAndStatus(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()

	status, err := st.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Pending != 0 || status.Current != status.Latest || status.Latest < 1 {
		t.Errorf("after Open: %+v", status)
	}
	if err := st.Ready(ctx); err != nil {
		t.Errorf("ready: %v", err)
	}
	if err := st.Migrate(ctx); err != nil { // idempotent
		t.Errorf("second migrate: %v", err)
	}

	if err := st.MigrateDownTo(ctx, 0); err != nil {
		t.Fatal(err)
	}
	status, _ = st.Status(ctx)
	if status.Current != 0 || status.Pending == 0 {
		t.Errorf("after down: %+v", status)
	}
	if err := st.Ready(ctx); err == nil {
		t.Error("not ready while migrations are pending")
	}
	storetest.Reset(t, st)
}

func TestOrganizationLifecycle(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()

	if _, err := st.GetBuiltinOrganization(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("empty: %v", err)
	}
	id := uuid.New()
	o, err := st.CreateBuiltinOrganization(ctx, id, "HTG")
	if err != nil || o.ID != id || !o.Builtin || o.Owned() {
		t.Fatalf("create: %v %+v", err, o)
	}
	again, err := st.CreateBuiltinOrganization(ctx, uuid.New(), "other")
	if err != nil || again.ID != id {
		t.Errorf("second builtin must return the existing row: %v %+v", err, again)
	}
	if err := st.UpdateOrganizationName(ctx, id, "Hidden Token Gaming"); err != nil {
		t.Error(err)
	}
	if err := st.UpdateOrganizationName(ctx, uuid.New(), "x"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("rename unknown: %v", err)
	}

	hash := []byte("0123456789abcdef0123456789abcdef")
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := st.SetClaimToken(ctx, id, hash, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOrganization(ctx, id, []byte("wrong"), now); !errors.Is(err, store.ErrClaimRejected) {
		t.Errorf("wrong hash: %v", err)
	}
	if _, err := st.ClaimOrganization(ctx, id, hash, now.Add(2*time.Minute)); !errors.Is(err, store.ErrClaimRejected) {
		t.Errorf("expired: %v", err)
	}
	claimed, err := st.ClaimOrganization(ctx, id, hash, now)
	if err != nil || !claimed.Owned() || claimed.ClaimTokenHash != nil || claimed.ClaimTokenExpiresAt != nil {
		t.Fatalf("claim: %v %+v", err, claimed)
	}
	if _, err := st.ClaimOrganization(ctx, id, hash, now); !errors.Is(err, store.ErrOwned) {
		t.Errorf("second claim: %v", err)
	}
	if err := st.SetClaimToken(ctx, id, hash, now.Add(time.Minute)); !errors.Is(err, store.ErrOwned) {
		t.Errorf("no token once owned: %v", err)
	}
	got, err := st.GetBuiltinOrganization(ctx)
	if err != nil || got.Name != "Hidden Token Gaming" || !got.Owned() {
		t.Errorf("get: %v %+v", err, got)
	}
}

func TestOpenWaitsThenFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	_, err := store.Open(ctx, "postgres://nobody@127.0.0.1:1/none?connect_timeout=1", 1, 1200*time.Millisecond, discardLogger())
	if err == nil {
		t.Fatal("want error")
	}
	if time.Since(start) < 500*time.Millisecond {
		t.Errorf("should have retried before giving up: %v after %s", err, time.Since(start))
	}
}
