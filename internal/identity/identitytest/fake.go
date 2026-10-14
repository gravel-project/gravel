// Package identitytest holds an in-memory identity and session store and a fake provider for
// tests in other packages.
package identitytest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
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
	// RoleChanges are the grants and revocations SetUserRole made, in order.
	RoleChanges []RoleChange
	// Locals are local accounts' credentials by username (ADR-0026), removed with the identity.
	Locals map[string]store.LocalCredential
	// Links are password links (gravel#187), oldest first.
	Links []store.PasswordLink
	// Grants are role grants (ADR-0027), in the order made; Games, when set, are the games a
	// grant may name (store.ErrUnknownGame otherwise).
	Grants []store.RoleGrant
	Games  []string
}

// NewFakeStore returns an empty store.
func NewFakeStore() *FakeStore {
	return &FakeStore{
		Users: map[uuid.UUID]store.User{}, Identities: map[string]store.Identity{},
		Sessions: map[string]store.Session{}, Attempts: map[string]store.AuthAttempt{},
		Locals: map[string]store.LocalCredential{},
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
	f.keepCredential(&ident)
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
	f.keepCredential(&ident)
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
	if provider == store.ProviderLocal {
		delete(f.Locals, subject) // ON DELETE CASCADE
	}
	f.event(userID, provider, subject, store.IdentityEventUnlinked, at)
	return nil
}

func (f *FakeStore) event(userID uuid.UUID, provider, subject, event string, at time.Time) {
	f.Events = append(f.Events, store.IdentityEvent{ID: int64(len(f.Events) + 1), UserID: userID, Provider: provider, Subject: subject, Event: event, At: at})
}

func (f *FakeStore) ListUsers(_ context.Context, orgID uuid.UUID, after *store.UserCursor, limit int) ([]store.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("ListUsers"); err != nil {
		return nil, err
	}
	var all []store.User
	for _, u := range f.Users {
		if u.OrganizationID != orgID {
			continue
		}
		if after != nil {
			later := u.CreatedAt.After(after.CreatedAt) || (u.CreatedAt.Equal(after.CreatedAt) && u.ID.String() > after.ID.String())
			if !later {
				continue
			}
		}
		all = append(all, u)
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].CreatedAt.Before(all[j].CreatedAt)
		}
		return all[i].ID.String() < all[j].ID.String()
	})
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

func (f *FakeStore) ListIdentitiesForUsers(_ context.Context, userIDs []uuid.UUID) ([]store.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("ListIdentitiesForUsers"); err != nil {
		return nil, err
	}
	var out []store.Identity
	for _, id := range f.Identities {
		if slices.Contains(userIDs, id.UserID) {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UserID != out[j].UserID {
			return out[i].UserID.String() < out[j].UserID.String()
		}
		return out[i].LinkedAt.Before(out[j].LinkedAt)
	})
	return out, nil
}

func (f *FakeStore) ListIdentityEventsAfter(_ context.Context, afterID int64, limit int) ([]store.IdentityEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("ListIdentityEventsAfter"); err != nil {
		return nil, err
	}
	var out []store.IdentityEvent
	for _, e := range f.Events {
		if e.ID > afterID {
			out = append(out, e)
		}
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *FakeStore) IdentityEventsHead(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("IdentityEventsHead"); err != nil {
		return 0, err
	}
	if len(f.Events) == 0 {
		return 0, nil
	}
	return f.Events[len(f.Events)-1].ID, nil
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

// RevokeUserSessions is DeleteUserSessions done by staff; the real store also records it
// (sessions.revoked). ErrNotFound for a member the store doesn't have.
func (f *FakeStore) RevokeUserSessions(ctx context.Context, userID uuid.UUID, _ store.Actor, _ time.Time) (int64, error) {
	if _, err := f.GetUser(ctx, userID); err != nil {
		return 0, err
	}
	return f.DeleteUserSessions(ctx, userID)
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

// FakePublisher is a FakeProvider that is also an identity.Publisher. BeginPublish marks its
// session, so CompletePublish proves the flow began as a publish; the function it returns records
// each Profile, or fails with PublishErr.
type FakePublisher struct {
	FakeProvider
	PublishErr error

	pubMu     sync.Mutex
	published []identity.Profile
}

// BeginPublish is Begin with a publish session.
func (p *FakePublisher) BeginPublish(ctx context.Context, state string) (string, string, error) {
	authURL, _, err := p.Begin(ctx, state)
	if err != nil {
		return "", "", err
	}
	return authURL + "&publish=1", "psess:publish:" + state, nil
}

// CompletePublish is Complete for a session BeginPublish made.
func (p *FakePublisher) CompletePublish(ctx context.Context, session string, params url.Values) (identity.Account, func(context.Context, identity.Profile) error, error) {
	if !strings.HasPrefix(session, "psess:publish:") {
		return identity.Account{}, nil, fmt.Errorf("%w: not a publish session", identity.ErrProviderFailed)
	}
	acct, err := p.Complete(ctx, session, params)
	if err != nil {
		return identity.Account{}, nil, err
	}
	return acct, func(_ context.Context, prof identity.Profile) error {
		if p.PublishErr != nil {
			return p.PublishErr
		}
		p.pubMu.Lock()
		defer p.pubMu.Unlock()
		p.published = append(p.published, prof)
		return nil
	}, nil
}

// Published returns the profiles published so far.
func (p *FakePublisher) Published() []identity.Profile {
	p.pubMu.Lock()
	defer p.pubMu.Unlock()
	return slices.Clone(p.published)
}

// SetRoleGrant grants or revokes a role, as the real store does: the user's Roles follow their
// instance-wide grants, and RoleChanges records each change.
func (f *FakeStore) SetRoleGrant(_ context.Context, userID uuid.UUID, role, gameID string, granted bool, ch store.RoleChange) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("SetRoleGrant"); err != nil {
		return false, err
	}
	u, ok := f.Users[userID]
	if !ok {
		return false, store.ErrNotFound
	}
	if gameID != "" && f.Games != nil && !slices.Contains(f.Games, gameID) {
		return false, store.ErrUnknownGame
	}
	i := slices.IndexFunc(f.Grants, func(g store.RoleGrant) bool { return g.UserID == userID && g.Role == role && g.GameID == gameID })
	switch {
	case granted && i < 0:
		f.Grants = append(f.Grants, store.RoleGrant{UserID: userID, Role: role, GameID: gameID, GrantedBy: ch.By, GrantedByApp: ch.ByApp, GrantedAt: ch.At})
	case !granted && i >= 0:
		f.Grants = slices.Delete(f.Grants, i, i+1)
	default:
		return false, nil
	}
	u.Roles = nil
	for _, g := range f.Grants {
		if g.UserID == userID && g.GameID == "" {
			u.Roles = append(u.Roles, g.Role)
		}
	}
	slices.Sort(u.Roles)
	f.Users[userID] = u
	f.RoleChanges = append(f.RoleChanges, RoleChange{UserID: userID, Role: role, GameID: gameID, Granted: granted, By: ch.By, ByApp: ch.ByApp, At: ch.At})
	return true, nil
}

// RoleGrants returns the grants of the given members (all when empty).
func (f *FakeStore) RoleGrants(_ context.Context, userIDs []uuid.UUID) ([]store.RoleGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("RoleGrants"); err != nil {
		return nil, err
	}
	var out []store.RoleGrant
	for _, g := range f.Grants {
		if len(userIDs) == 0 || slices.Contains(userIDs, g.UserID) {
			out = append(out, g)
		}
	}
	return out, nil
}

// RoleChange is one recorded grant or revocation.
type RoleChange struct {
	UserID  uuid.UUID
	Role    string
	GameID  string
	Granted bool
	By      *uuid.UUID
	ByApp   *uuid.UUID
	At      time.Time
}

// Changes are the recorded role changes, read under the store's lock.
func (f *FakeStore) Changes() []RoleChange {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.RoleChanges)
}

// keepCredential moves a local account's credential off the identity into Locals, as the store
// writes it to local_credentials.
func (f *FakeStore) keepCredential(ident *store.Identity) {
	if ident.Credential == nil {
		return
	}
	c := *ident.Credential
	c.Subject = ident.Subject
	f.Locals[ident.Subject] = c
	ident.Credential = nil
}

func (f *FakeStore) GetLocalCredential(_ context.Context, subject string) (store.LocalCredential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("GetLocalCredential"); err != nil {
		return store.LocalCredential{}, err
	}
	c, ok := f.Locals[subject]
	if !ok {
		return store.LocalCredential{}, store.ErrNotFound
	}
	return c, nil
}

// RecordLocalFailure is the store's statement in Go: the failure that reaches the threshold
// locks the account for Base·2^lockouts, at most Max.
func (f *FakeStore) RecordLocalFailure(_ context.Context, subject string, now time.Time, l store.Lockout) (store.LocalCredential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("RecordLocalFailure"); err != nil {
		return store.LocalCredential{}, err
	}
	c, ok := f.Locals[subject]
	if !ok {
		return store.LocalCredential{}, store.ErrNotFound
	}
	if c.FailedAttempts+1 >= l.Threshold {
		d := l.Base << min(c.Lockouts, 30)
		if d > l.Max || d <= 0 {
			d = l.Max
		}
		until := now.Add(d)
		c.FailedAttempts, c.Lockouts, c.LockedUntil = 0, c.Lockouts+1, &until
	} else {
		c.FailedAttempts++
	}
	f.Locals[subject] = c
	return c, nil
}

func (f *FakeStore) RecordLocalSuccess(_ context.Context, subject, rehash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("RecordLocalSuccess"); err != nil {
		return err
	}
	c, ok := f.Locals[subject]
	if !ok {
		return store.ErrNotFound
	}
	c.FailedAttempts, c.Lockouts, c.LockedUntil = 0, 0, nil
	if rehash != "" {
		c.PasswordHash = rehash
	}
	f.Locals[subject] = c
	return nil
}

// Password links (gravel#187), as the store keeps them.

func (f *FakeStore) CreatePasswordLink(_ context.Context, l store.PasswordLink) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("CreatePasswordLink"); err != nil {
		return err
	}
	ident, ok := f.Identities[key(store.ProviderLocal, l.Subject)]
	if !ok {
		return store.ErrNotFound
	}
	f.Links = slices.DeleteFunc(f.Links, func(o store.PasswordLink) bool { return o.Subject == l.Subject && o.UsedAt == nil })
	f.Links = append(f.Links, l)
	f.event(ident.UserID, store.ProviderLocal, l.Subject, store.IdentityEventPasswordLinkIssued, l.IssuedAt)
	return nil
}

func (f *FakeStore) GetPasswordLink(_ context.Context, tokenHash []byte, now time.Time) (store.PasswordLink, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range f.Links {
		if bytes.Equal(l.TokenHash, tokenHash) && l.UsedAt == nil && l.ExpiresAt.After(now) {
			return l, nil
		}
	}
	return store.PasswordLink{}, store.ErrNotFound
}

func (f *FakeStore) UsePasswordLink(_ context.Context, tokenHash []byte, now time.Time, passwordHash string) (store.PasswordLink, uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, l := range f.Links {
		if !bytes.Equal(l.TokenHash, tokenHash) || l.UsedAt != nil || !l.ExpiresAt.After(now) {
			continue
		}
		at := now
		f.Links[i].UsedAt = &at
		c := f.Locals[l.Subject]
		c.PasswordHash, c.PasswordUpdatedAt, c.FailedAttempts, c.Lockouts, c.LockedUntil = passwordHash, now, 0, 0, nil
		f.Locals[l.Subject] = c
		ident := f.Identities[key(store.ProviderLocal, l.Subject)]
		event := store.IdentityEventPasswordReset
		if l.Purpose == store.PasswordLinkSet {
			event = store.IdentityEventPasswordSet
		}
		f.event(ident.UserID, store.ProviderLocal, l.Subject, event, now)
		return f.Links[i], ident.UserID, nil
	}
	return store.PasswordLink{}, uuid.Nil, store.ErrNotFound
}

func (f *FakeStore) DeleteExpiredPasswordLinks(_ context.Context, cutoff time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	before := len(f.Links)
	f.Links = slices.DeleteFunc(f.Links, func(l store.PasswordLink) bool { return l.ExpiresAt.Before(cutoff) })
	return int64(before - len(f.Links)), nil
}
