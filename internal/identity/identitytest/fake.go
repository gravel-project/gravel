// Package identitytest holds an in-memory identity and session store and a fake provider for
// tests in other packages.
package identitytest

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/store"
)

// FakeStore implements identity.Store and session.Store in memory, with the real store's
// predicates. ErrOn names a method that fails with a synthetic error.
type FakeStore struct {
	mu         sync.Mutex
	ErrOn      string
	Users      map[uuid.UUID]store.User
	Identities map[string]store.Identity // key: provider + "/" + subject
	Events     []store.IdentityEvent
	Sessions   map[string]store.Session // key: string(token hash)
	Attempts   map[string]store.AuthAttempt
}

// NewFakeStore returns an empty store.
func NewFakeStore() *FakeStore {
	return &FakeStore{
		Users: map[uuid.UUID]store.User{}, Identities: map[string]store.Identity{},
		Sessions: map[string]store.Session{}, Attempts: map[string]store.AuthAttempt{},
	}
}

func key(provider, subject string) string { return provider + "/" + subject }

func (f *FakeStore) fail(method string) error {
	if f.ErrOn == method {
		return errors.New("db down")
	}
	return nil
}

func (f *FakeStore) GetUser(_ context.Context, id uuid.UUID) (store.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("GetUser"); err != nil {
		return store.User{}, err
	}
	u, ok := f.Users[id]
	if !ok {
		return store.User{}, store.ErrNotFound
	}
	return u, nil
}

func (f *FakeStore) RegisterUser(_ context.Context, u store.User, ident store.Identity) (store.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("RegisterUser"); err != nil {
		return store.User{}, err
	}
	if _, ok := f.Identities[key(ident.Provider, ident.Subject)]; ok {
		return store.User{}, store.ErrIdentityExists
	}
	u.CreatedAt = ident.VerifiedAt
	at := ident.VerifiedAt
	u.LastLoginAt = &at
	f.Users[u.ID] = u
	ident.UserID = u.ID
	ident.LinkedAt = at
	ident.LastLoginAt = &at
	f.Identities[key(ident.Provider, ident.Subject)] = ident
	f.event(u.ID, ident.Provider, ident.Subject, store.IdentityEventRegistered, at)
	return u, nil
}

func (f *FakeStore) LinkIdentity(_ context.Context, ident store.Identity) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("LinkIdentity"); err != nil {
		return err
	}
	if _, ok := f.Identities[key(ident.Provider, ident.Subject)]; ok {
		return store.ErrIdentityExists
	}
	ident.LinkedAt = ident.VerifiedAt
	f.Identities[key(ident.Provider, ident.Subject)] = ident
	f.event(ident.UserID, ident.Provider, ident.Subject, store.IdentityEventLinked, ident.VerifiedAt)
	return nil
}

func (f *FakeStore) GetIdentity(_ context.Context, provider, subject string) (store.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("GetIdentity"); err != nil {
		return store.Identity{}, err
	}
	i, ok := f.Identities[key(provider, subject)]
	if !ok {
		return store.Identity{}, store.ErrNotFound
	}
	return i, nil
}

func (f *FakeStore) ListIdentities(_ context.Context, userID uuid.UUID) ([]store.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("ListIdentities"); err != nil {
		return nil, err
	}
	var out []store.Identity
	for _, i := range f.Identities {
		if i.UserID == userID {
			out = append(out, i)
		}
	}
	// Oldest link first, like the real query.
	for a := range out {
		for b := a + 1; b < len(out); b++ {
			if out[b].LinkedAt.Before(out[a].LinkedAt) || (out[b].LinkedAt.Equal(out[a].LinkedAt) && out[b].Provider < out[a].Provider) {
				out[a], out[b] = out[b], out[a]
			}
		}
	}
	return out, nil
}

func (f *FakeStore) RecordLogin(_ context.Context, provider, subject, displayName, avatarURL string, at time.Time) (store.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("RecordLogin"); err != nil {
		return store.Identity{}, err
	}
	i, ok := f.Identities[key(provider, subject)]
	if !ok {
		return store.Identity{}, store.ErrNotFound
	}
	i.DisplayName, i.AvatarURL, i.LastLoginAt = displayName, avatarURL, &at
	f.Identities[key(provider, subject)] = i
	u := f.Users[i.UserID]
	u.LastLoginAt = &at
	f.Users[i.UserID] = u
	f.event(i.UserID, provider, subject, store.IdentityEventLogin, at)
	return i, nil
}

func (f *FakeStore) RefreshIdentity(_ context.Context, provider, subject, displayName, avatarURL string) (store.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("RefreshIdentity"); err != nil {
		return store.Identity{}, err
	}
	i, ok := f.Identities[key(provider, subject)]
	if !ok {
		return store.Identity{}, store.ErrNotFound
	}
	i.DisplayName, i.AvatarURL = displayName, avatarURL
	f.Identities[key(provider, subject)] = i
	return i, nil
}

func (f *FakeStore) UnlinkIdentity(_ context.Context, userID uuid.UUID, provider, subject string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("UnlinkIdentity"); err != nil {
		return err
	}
	count, found := 0, false
	for _, i := range f.Identities {
		if i.UserID == userID {
			count++
			if i.Provider == provider && i.Subject == subject {
				found = true
			}
		}
	}
	if !found {
		return store.ErrNotFound
	}
	if count == 1 {
		return store.ErrLastIdentity
	}
	delete(f.Identities, key(provider, subject))
	f.event(userID, provider, subject, store.IdentityEventUnlinked, at)
	return nil
}

func (f *FakeStore) event(userID uuid.UUID, provider, subject, event string, at time.Time) {
	f.Events = append(f.Events, store.IdentityEvent{UserID: userID, Provider: provider, Subject: subject, Event: event, At: at})
}

func (f *FakeStore) CreateAuthAttempt(_ context.Context, a store.AuthAttempt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("CreateAuthAttempt"); err != nil {
		return err
	}
	f.Attempts[string(a.TokenHash)] = a
	return nil
}

func (f *FakeStore) ConsumeAuthAttempt(_ context.Context, hash []byte, now time.Time) (store.AuthAttempt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("ConsumeAuthAttempt"); err != nil {
		return store.AuthAttempt{}, err
	}
	a, ok := f.Attempts[string(hash)]
	if !ok {
		return store.AuthAttempt{}, store.ErrNotFound
	}
	delete(f.Attempts, string(hash))
	if !a.ExpiresAt.After(now) {
		return store.AuthAttempt{}, store.ErrNotFound
	}
	return a, nil
}

func (f *FakeStore) DeleteExpiredAuthAttempts(_ context.Context, now time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for k, a := range f.Attempts {
		if !a.ExpiresAt.After(now) {
			delete(f.Attempts, k)
			n++
		}
	}
	return n, nil
}

func (f *FakeStore) CreateSession(_ context.Context, s store.Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("CreateSession"); err != nil {
		return err
	}
	if _, ok := f.Sessions[string(s.TokenHash)]; ok {
		return errors.New("duplicate token hash")
	}
	f.Sessions[string(s.TokenHash)] = s
	return nil
}

func (f *FakeStore) GetSessionByTokenHash(_ context.Context, hash []byte, now time.Time) (store.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("GetSessionByTokenHash"); err != nil {
		return store.Session{}, err
	}
	s, ok := f.Sessions[string(hash)]
	if !ok || !s.ExpiresAt.After(now) {
		return store.Session{}, store.ErrNotFound
	}
	return s, nil
}

func (f *FakeStore) TouchSession(_ context.Context, id uuid.UUID, lastSeen time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("TouchSession"); err != nil {
		return err
	}
	for k, s := range f.Sessions {
		if s.ID == id {
			s.LastSeenAt = lastSeen
			f.Sessions[k] = s
		}
	}
	return nil
}

func (f *FakeStore) DeleteSession(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("DeleteSession"); err != nil {
		return err
	}
	for k, s := range f.Sessions {
		if s.ID == id {
			delete(f.Sessions, k)
		}
	}
	return nil
}

func (f *FakeStore) DeleteUserSessions(_ context.Context, userID uuid.UUID) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("DeleteUserSessions"); err != nil {
		return 0, err
	}
	var n int64
	for k, s := range f.Sessions {
		if s.UserID == userID {
			delete(f.Sessions, k)
			n++
		}
	}
	return n, nil
}

func (f *FakeStore) DeleteExpiredSessions(_ context.Context, now time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for k, s := range f.Sessions {
		if !s.ExpiresAt.After(now) {
			delete(f.Sessions, k)
			n++
		}
	}
	return n, nil
}

// SessionCount reports the live sessions (for tests).
func (f *FakeStore) SessionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Sessions)
}

// FakeProvider is an identity.Provider whose callback is driven by the "code" parameter:
// Accounts maps a code to the account it yields. Begin records the states it was given.
type FakeProvider struct {
	ProviderName string
	Display      string
	Accounts     map[string]identity.Account
	BeginErr     error
	CompleteErr  error

	mu     sync.Mutex
	States []string
}

func (p *FakeProvider) Name() string { return p.ProviderName }

func (p *FakeProvider) DisplayName() string {
	if p.Display != "" {
		return p.Display
	}
	return strings.ToUpper(p.ProviderName[:1]) + p.ProviderName[1:]
}

func (p *FakeProvider) Begin(_ context.Context, state string) (string, string, error) {
	if p.BeginErr != nil {
		return "", "", p.BeginErr
	}
	p.mu.Lock()
	p.States = append(p.States, state)
	p.mu.Unlock()
	return "https://" + p.ProviderName + ".example/authorize?state=" + url.QueryEscape(state), "psess:" + state, nil
}

func (p *FakeProvider) Complete(_ context.Context, session string, params url.Values) (identity.Account, error) {
	if p.CompleteErr != nil {
		return identity.Account{}, p.CompleteErr
	}
	if !strings.HasPrefix(session, "psess:") {
		return identity.Account{}, fmt.Errorf("%w: bad session", identity.ErrProviderFailed)
	}
	if params.Get("error") == "access_denied" {
		return identity.Account{}, fmt.Errorf("%w: access_denied", identity.ErrProviderDenied)
	}
	acct, ok := p.Accounts[params.Get("code")]
	if !ok {
		return identity.Account{}, fmt.Errorf("%w: unknown code", identity.ErrProviderFailed)
	}
	acct.Provider = p.ProviderName
	return acct, nil
}

// CallbackParams builds the query a provider callback carries for a code and state.
func CallbackParams(code, state string) url.Values {
	return url.Values{"code": {code}, "state": {state}}
}

// LastState returns the state of the most recent Begin.
func (p *FakeProvider) LastState() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.States) == 0 {
		return ""
	}
	return p.States[len(p.States)-1]
}
