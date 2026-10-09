// Package appstest holds an in-memory app store for tests in other packages.
package appstest

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/store"
)

// FakeStore implements apps.Store in memory, with the real store's predicates. ErrOn names a
// method that fails with a synthetic error.
type FakeStore struct {
	mu     sync.Mutex
	ErrOn  string
	Apps   map[uuid.UUID]store.App
	Tokens map[uuid.UUID]store.AppToken
}

// NewFakeStore returns an empty store.
func NewFakeStore() *FakeStore {
	return &FakeStore{Apps: map[uuid.UUID]store.App{}, Tokens: map[uuid.UUID]store.AppToken{}}
}

func (f *FakeStore) fail(method string) error {
	if f.ErrOn == method {
		return errors.New("db down")
	}
	return nil
}

func (f *FakeStore) CreateApp(_ context.Context, a store.App) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("CreateApp"); err != nil {
		return err
	}
	for _, other := range f.Apps {
		if other.ClientID == a.ClientID {
			return errors.New("duplicate client id")
		}
	}
	f.Apps[a.ID] = a
	return nil
}

func (f *FakeStore) GetApp(_ context.Context, id uuid.UUID) (store.App, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("GetApp"); err != nil {
		return store.App{}, err
	}
	a, ok := f.Apps[id]
	if !ok {
		return store.App{}, store.ErrNotFound
	}
	return a, nil
}

func (f *FakeStore) GetAppByClientID(_ context.Context, clientID string) (store.App, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("GetAppByClientID"); err != nil {
		return store.App{}, err
	}
	for _, a := range f.Apps {
		if a.ClientID == clientID {
			return a, nil
		}
	}
	return store.App{}, store.ErrNotFound
}

func (f *FakeStore) ListApps(_ context.Context, orgID uuid.UUID) ([]store.App, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("ListApps"); err != nil {
		return nil, err
	}
	var out []store.App
	for _, a := range f.Apps {
		if a.OrganizationID == orgID {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ClientID < out[j].ClientID // deterministic within one instant, like the real store's (created_at, id)
	})
	return out, nil
}

func (f *FakeStore) RevokeApp(_ context.Context, id uuid.UUID, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("RevokeApp"); err != nil {
		return err
	}
	a, ok := f.Apps[id]
	if !ok || a.RevokedAt != nil {
		return store.ErrNotFound
	}
	a.RevokedAt = &at
	f.Apps[id] = a
	for tid, t := range f.Tokens {
		if t.AppID == id {
			delete(f.Tokens, tid)
		}
	}
	return nil
}

func (f *FakeStore) CreateAppToken(_ context.Context, t store.AppToken) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("CreateAppToken"); err != nil {
		return err
	}
	f.Tokens[t.ID] = t
	return nil
}

func (f *FakeStore) GetAppTokenByHash(_ context.Context, hash []byte, now time.Time) (store.AppToken, store.App, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("GetAppTokenByHash"); err != nil {
		return store.AppToken{}, store.App{}, err
	}
	for _, t := range f.Tokens {
		if bytes.Equal(t.TokenHash, hash) && t.ExpiresAt.After(now) {
			a, ok := f.Apps[t.AppID]
			if !ok || a.RevokedAt != nil {
				return store.AppToken{}, store.App{}, store.ErrNotFound
			}
			return t, a, nil
		}
	}
	return store.AppToken{}, store.App{}, store.ErrNotFound
}

func (f *FakeStore) DeleteExpiredAppTokens(_ context.Context, now time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("DeleteExpiredAppTokens"); err != nil {
		return 0, err
	}
	var n int64
	for id, t := range f.Tokens {
		if !t.ExpiresAt.After(now) {
			delete(f.Tokens, id)
			n++
		}
	}
	return n, nil
}

// TokenCount is how many tokens are stored.
func (f *FakeStore) TokenCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Tokens)
}
