// Package orgtest is an in-memory org.Store for tests in other packages.
package orgtest

import (
	"bytes"
	"context"
	"encoding/json"
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
	// Recorded are the events the real store would append: the claim and settings updates.
	Recorded []Recorded
}

// Recorded is an event the real store would write, with who made the change.
type Recorded struct {
	Kind     string
	By       store.Actor
	Sections []string
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
	f.Org = &store.Organization{ID: id, Name: name, Builtin: true, CreatedAt: time.Unix(0, 0).UTC(), Settings: json.RawMessage("{}")}
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

func (f *FakeStore) ClaimOrganization(_ context.Context, _ uuid.UUID, hash []byte, by store.Actor, now time.Time) (store.Organization, error) {
	f.Writes = append(f.Writes, "claim")
	if f.Org.Owned() {
		return store.Organization{}, store.ErrOwned
	}
	if !bytes.Equal(f.Org.ClaimTokenHash, hash) || !f.Org.ClaimTokenExpiresAt.After(now) {
		return store.Organization{}, store.ErrClaimRejected
	}
	owner := *by.UserID
	f.Org.ClaimedAt = &now
	f.Org.OwnerUserID = &owner
	f.Org.ClaimTokenHash, f.Org.ClaimTokenExpiresAt = nil, nil
	f.Recorded = append(f.Recorded, Recorded{Kind: "ownership.claimed", By: by})
	return *f.Org, nil
}

func (f *FakeStore) UpdateOrganizationSettings(_ context.Context, _ uuid.UUID, settings json.RawMessage, at time.Time, by store.Actor, sections []string) (store.Organization, error) {
	f.Writes = append(f.Writes, "settings")
	if f.ErrOn == "settings" {
		return store.Organization{}, errors.New("db down")
	}
	f.Org.Settings = append(json.RawMessage(nil), settings...)
	f.Org.SettingsUpdatedAt = &at
	f.Recorded = append(f.Recorded, Recorded{Kind: "settings.updated", By: by, Sections: sections})
	return *f.Org, nil
}
