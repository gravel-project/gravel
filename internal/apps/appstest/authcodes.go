package appstest

import (
	"bytes"
	"context"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/store"
)

func (f *FakeStore) CreateAuthorizationCode(_ context.Context, c store.AuthorizationCode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("CreateAuthorizationCode"); err != nil {
		return err
	}
	f.Codes[c.ID] = c
	return nil
}

// RedeemAuthorizationCode has the real store's semantics under the store's lock: unknown is
// ErrNotFound, a used code deletes its tokens and answers ErrCodeReplayed, otherwise the code is
// spent and issue decides.
func (f *FakeStore) RedeemAuthorizationCode(_ context.Context, hash []byte, now time.Time, issue func(store.AuthorizationCode) (store.AppToken, error)) (store.AuthorizationCode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("RedeemAuthorizationCode"); err != nil {
		return store.AuthorizationCode{}, err
	}
	for id, c := range f.Codes {
		if !bytes.Equal(c.CodeHash, hash) {
			continue
		}
		if c.UsedAt != nil {
			for tid, t := range f.Tokens {
				if t.CodeID != nil && *t.CodeID == id {
					delete(f.Tokens, tid)
				}
			}
			return c, store.ErrCodeReplayed
		}
		used := now
		c.UsedAt = &used
		f.Codes[id] = c
		t, err := issue(c)
		if err != nil {
			return c, err
		}
		cid := id
		t.CodeID = &cid
		f.Tokens[t.ID] = t
		return c, nil
	}
	return store.AuthorizationCode{}, store.ErrNotFound
}

func (f *FakeStore) DeleteExpiredAuthorizationCodes(_ context.Context, before time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("DeleteExpiredAuthorizationCodes"); err != nil {
		return 0, err
	}
	var n int64
	for id, c := range f.Codes {
		if !c.ExpiresAt.After(before) {
			delete(f.Codes, id)
			n++
		}
	}
	return n, nil
}

func (f *FakeStore) GetAppGrant(_ context.Context, userID, appID uuid.UUID) (store.AppGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("GetAppGrant"); err != nil {
		return store.AppGrant{}, err
	}
	g, ok := f.Grants[GrantKey{userID, appID}]
	if !ok {
		return store.AppGrant{}, store.ErrNotFound
	}
	a := f.Apps[appID]
	g.ClientID, g.AppName = a.ClientID, a.Name
	return g, nil
}

func (f *FakeStore) SaveAppGrant(_ context.Context, g store.AppGrant) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("SaveAppGrant"); err != nil {
		return err
	}
	g.ClientID, g.AppName = "", ""
	f.Grants[GrantKey{g.UserID, g.AppID}] = g
	return nil
}

func (f *FakeStore) ListAppGrants(_ context.Context, userID uuid.UUID) ([]store.AppGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("ListAppGrants"); err != nil {
		return nil, err
	}
	var out []store.AppGrant
	for k, g := range f.Grants {
		a, ok := f.Apps[k.App]
		if k.User != userID || !ok || a.RevokedAt != nil {
			continue
		}
		g.ClientID, g.AppName = a.ClientID, a.Name
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].GrantedAt.Equal(out[j].GrantedAt) {
			return out[i].GrantedAt.Before(out[j].GrantedAt)
		}
		return out[i].ClientID < out[j].ClientID
	})
	return out, nil
}

func (f *FakeStore) DeleteAppGrant(_ context.Context, userID, appID uuid.UUID) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("DeleteAppGrant"); err != nil {
		return 0, err
	}
	k := GrantKey{userID, appID}
	if _, ok := f.Grants[k]; !ok {
		return 0, store.ErrNotFound
	}
	delete(f.Grants, k)
	var n int64
	for id, t := range f.Tokens {
		if t.AppID == appID && t.UserID != nil && *t.UserID == userID {
			delete(f.Tokens, id)
			n++
		}
	}
	for id, c := range f.Codes {
		if c.AppID == appID && c.UserID == userID {
			delete(f.Codes, id)
		}
	}
	return n, nil
}

// RevokeUserTokens deletes a member's tokens, as the real store's DeleteUserSessions does along
// with their sessions.
func (f *FakeStore) RevokeUserTokens(userID uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, t := range f.Tokens {
		if t.UserID != nil && *t.UserID == userID {
			delete(f.Tokens, id)
		}
	}
}
