package org

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/gravel-project/gravel/internal/org/orgtest"
	"github.com/gravel-project/gravel/internal/store"
)

func newTestService(f *orgtest.FakeStore) (*Service, *time.Time) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	s := New(f, 15*time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.now = func() time.Time { return now }
	return s, &now
}

func TestEnsureBuiltinCreatesAndMints(t *testing.T) {
	f := &orgtest.FakeStore{}
	s, _ := newTestService(f)
	o, token, err := s.EnsureBuiltin(context.Background(), " Hidden Token Gaming ")
	if err != nil {
		t.Fatal(err)
	}
	if o.Name != "Hidden Token Gaming" || !o.Builtin || o.Owned() {
		t.Errorf("org: %+v", o)
	}
	if len(token) != 43 { // 32 random bytes, base64url without padding
		t.Errorf("token length %d: %q", len(token), token)
	}
	if o.ClaimTokenExpiresAt == nil || !o.ClaimTokenExpiresAt.Equal(s.now().Add(15*time.Minute)) {
		t.Errorf("expiry: %v", o.ClaimTokenExpiresAt)
	}
	if got := f.Writes; len(got) != 2 || got[0] != "create" || got[1] != "set-token" {
		t.Errorf("writes: %v", got)
	}
}

func TestEnsureBuiltinRotatesWhileUnowned(t *testing.T) {
	f := &orgtest.FakeStore{}
	s, _ := newTestService(f)
	_, first, _ := s.EnsureBuiltin(context.Background(), "x")
	_, second, err := s.EnsureBuiltin(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if first == second || second == "" {
		t.Error("a restart while unowned must mint a new token")
	}
	if _, err := s.Claim(context.Background(), first); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("the rotated-away token must be refused: %v", err)
	}
	if _, err := s.Claim(context.Background(), second); err != nil {
		t.Errorf("current token must work: %v", err)
	}
}

func TestEnsureBuiltinRenames(t *testing.T) {
	f := &orgtest.FakeStore{}
	s, _ := newTestService(f)
	_, _, _ = s.EnsureBuiltin(context.Background(), "old")
	o, _, err := s.EnsureBuiltin(context.Background(), "new")
	if err != nil || o.Name != "new" || f.Org.Name != "new" {
		t.Errorf("rename: %v %+v", err, o)
	}
}

func TestClaimOnce(t *testing.T) {
	f := &orgtest.FakeStore{}
	s, _ := newTestService(f)
	_, token, _ := s.EnsureBuiltin(context.Background(), "x")

	if _, err := s.Claim(context.Background(), "nope"); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("wrong token: %v", err)
	}
	if _, err := s.Claim(context.Background(), ""); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("empty token: %v", err)
	}
	o, err := s.Claim(context.Background(), " "+token+"\n")
	if err != nil || !o.Owned() || o.ClaimTokenHash != nil {
		t.Fatalf("claim: %v %+v", err, o)
	}
	if _, err := s.Claim(context.Background(), token); !errors.Is(err, ErrAlreadyOwned) {
		t.Errorf("second use must say owned: %v", err)
	}
	o2, tok2, err := s.EnsureBuiltin(context.Background(), "x")
	if err != nil || tok2 != "" || !o2.Owned() {
		t.Errorf("no token once owned: %v %q", err, tok2)
	}
}

func TestClaimExpired(t *testing.T) {
	f := &orgtest.FakeStore{}
	s, now := newTestService(f)
	_, token, _ := s.EnsureBuiltin(context.Background(), "x")
	*now = now.Add(16 * time.Minute)
	if _, err := s.Claim(context.Background(), token); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("expired token must be refused: %v", err)
	}
}

func TestClaimRaceLosesToStore(t *testing.T) {
	// The in-memory compare passes but the store says somebody else claimed first.
	f := &orgtest.FakeStore{}
	s, now := newTestService(f)
	_, token, _ := s.EnsureBuiltin(context.Background(), "x")
	claimed := now.Add(-time.Second)
	f.Org.ClaimedAt = &claimed // owned behind our back, token still visible in the copy we read
	snapshot := *f.Org
	snapshot.ClaimedAt = nil
	racy := &racingStore{FakeStore: f, snapshot: snapshot}
	s.st = racy
	if _, err := s.Claim(context.Background(), token); !errors.Is(err, ErrAlreadyOwned) {
		t.Errorf("want ErrAlreadyOwned from the atomic predicate, got %v", err)
	}
}

type racingStore struct {
	*orgtest.FakeStore
	snapshot store.Organization
}

func (r *racingStore) GetBuiltinOrganization(context.Context) (store.Organization, error) {
	return r.snapshot, nil
}

func TestStoreErrorsPropagate(t *testing.T) {
	f := &orgtest.FakeStore{ErrOn: "get"}
	s, _ := newTestService(f)
	if _, _, err := s.EnsureBuiltin(context.Background(), "x"); err == nil {
		t.Error("want error")
	}
	if _, err := s.Claim(context.Background(), "t"); err == nil {
		t.Error("want error")
	}
}
