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
	// Recorded are the events the real store would append: the claim, settings updates and the
	// ownership transfer's steps.
	Recorded []Recorded
	// Member says whether a nomination may name id, and their name, as the real store finds it
	// among the organization's users; nil admits anyone, unnamed.
	Member func(id uuid.UUID) (name string, ok bool)
	// OnTransfer is called when a transfer completes, as the real store grants the old owner admin
	// in the same transaction; a rig wires it to its identity store.
	OnTransfer func(oldOwner, newOwner uuid.UUID, at time.Time)
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

func (f *FakeStore) NominateOwner(_ context.Context, _ uuid.UUID, nominee uuid.UUID, by store.Actor, at, expires time.Time) (store.Organization, error) {
	f.Writes = append(f.Writes, "nominate")
	switch {
	case by.UserID == nil || f.Org.OwnerUserID == nil || *f.Org.OwnerUserID != *by.UserID:
		return store.Organization{}, store.ErrNotOwner
	case nominee == *by.UserID:
		return store.Organization{}, store.ErrSelfNomination
	}
	var name string
	if f.Member != nil {
		n, ok := f.Member(nominee)
		if !ok {
			return store.Organization{}, store.ErrNotFound
		}
		name = n
	}
	f.Org.Pending = &store.PendingOwner{UserID: nominee, Name: name, NominatedAt: at, ExpiresAt: expires}
	f.Recorded = append(f.Recorded, Recorded{Kind: "ownership.nominated", By: by})
	return *f.Org, nil
}

func (f *FakeStore) CancelOwnershipTransfer(_ context.Context, _ uuid.UUID, by store.Actor, _ time.Time) (store.Organization, error) {
	f.Writes = append(f.Writes, "cancel-transfer")
	if f.Org.Pending == nil {
		return store.Organization{}, store.ErrNoNomination
	}
	declined := by.UserID != nil && *by.UserID == f.Org.Pending.UserID
	if !declined && (by.UserID == nil || f.Org.OwnerUserID == nil || *f.Org.OwnerUserID != *by.UserID) {
		return store.Organization{}, store.ErrNotOwner
	}
	f.Org.Pending = nil
	kind := "ownership.transfer_cancelled"
	if declined {
		kind = "ownership.declined"
	}
	f.Recorded = append(f.Recorded, Recorded{Kind: kind, By: by})
	return *f.Org, nil
}

func (f *FakeStore) AcceptOwnership(_ context.Context, _ uuid.UUID, by store.Actor, at time.Time) (store.Organization, uuid.UUID, error) {
	f.Writes = append(f.Writes, "accept")
	p := f.Org.Pending
	switch {
	case by.UserID == nil || p == nil || p.UserID != *by.UserID || f.Org.OwnerUserID == nil:
		return store.Organization{}, uuid.Nil, store.ErrNoNomination
	case !p.ExpiresAt.After(at):
		return store.Organization{}, uuid.Nil, store.ErrNominationExpired
	}
	old, me := *f.Org.OwnerUserID, *by.UserID
	f.Org.OwnerUserID, f.Org.Pending = &me, nil
	f.Recorded = append(f.Recorded, Recorded{Kind: "ownership.transferred", By: by})
	if f.OnTransfer != nil {
		f.OnTransfer(old, me, at)
	}
	return *f.Org, old, nil
}
