// Package orgtest is an in-memory org.Store for tests in other packages.
package orgtest

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/store"
)

// FakeStore mirrors the real store's predicates in memory.
type FakeStore struct {
	Org    *store.Organization
	ErrOn  string
	Writes []string
}

func (f *FakeStore) GetBuiltinOrganization(context.Context) (store.Organization, error) {
	if f.ErrOn == "get" {
		return store.Organization{}, errors.New("db down")
	}
	if f.Org == nil {
		return store.Organization{}, store.ErrNotFound
	}
	return *f.Org, nil
}

func (f *FakeStore) CreateBuiltinOrganization(_ context.Context, id uuid.UUID, name string) (store.Organization, error) {
	f.Writes = append(f.Writes, "create")
	f.Org = &store.Organization{ID: id, Name: name, Builtin: true, CreatedAt: time.Unix(0, 0).UTC()}
	return *f.Org, nil
}

func (f *FakeStore) UpdateOrganizationName(_ context.Context, _ uuid.UUID, name string) error {
	f.Writes = append(f.Writes, "rename")
	f.Org.Name = name
	return nil
}

func (f *FakeStore) SetClaimToken(_ context.Context, _ uuid.UUID, hash []byte, expiresAt time.Time) error {
	f.Writes = append(f.Writes, "set-token")
	if f.Org.Owned() {
		return store.ErrOwned
	}
	f.Org.ClaimTokenHash = hash
	f.Org.ClaimTokenExpiresAt = &expiresAt
	return nil
}

func (f *FakeStore) ClaimOrganization(_ context.Context, _ uuid.UUID, hash []byte, now time.Time) (store.Organization, error) {
	f.Writes = append(f.Writes, "claim")
	if f.Org.Owned() {
		return store.Organization{}, store.ErrOwned
	}
	if !bytes.Equal(f.Org.ClaimTokenHash, hash) || !f.Org.ClaimTokenExpiresAt.After(now) {
		return store.Organization{}, store.ErrClaimRejected
	}
	f.Org.ClaimedAt = &now
	f.Org.ClaimTokenHash, f.Org.ClaimTokenExpiresAt = nil, nil
	return *f.Org, nil
}
