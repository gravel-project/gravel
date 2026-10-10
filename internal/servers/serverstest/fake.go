// Package serverstest holds an in-memory servers.Store for tests.
package serverstest

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/store"
)

// FakeStore keeps games and servers in memory, with the store's apply rules (insert, update
// where different, revive, mark the rest removed).
type FakeStore struct {
	mu      sync.Mutex
	games   map[string]store.Game
	servers map[string]store.ManagedServer
	// Applies counts ApplyServers calls.
	Applies int
}

// NewFakeStore is an empty store.
func NewFakeStore() *FakeStore {
	return &FakeStore{games: map[string]store.Game{}, servers: map[string]store.ManagedServer{}}
}

// ListGames returns the games by id.
func (f *FakeStore) ListGames(_ context.Context, _ uuid.UUID, includeRemoved bool) ([]store.Game, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Game
	for _, g := range f.games {
		if includeRemoved || g.RemovedAt == nil {
			out = append(out, g)
		}
	}
	slices.SortFunc(out, func(a, b store.Game) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// ListManagedServers returns the servers by id.
func (f *FakeStore) ListManagedServers(_ context.Context, _ uuid.UUID, includeRemoved bool) ([]store.ManagedServer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.ManagedServer
	for _, s := range f.servers {
		if includeRemoved || s.RemovedAt == nil {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b store.ManagedServer) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// ApplyServers applies like the real store.
func (f *FakeStore) ApplyServers(_ context.Context, orgID uuid.UUID, games []store.Game, servers []store.ManagedServer, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Applies++
	keepG := map[string]bool{}
	for _, g := range games {
		keepG[g.ID] = true
		g.OrganizationID = orgID
		if old, ok := f.games[g.ID]; ok && old.RemovedAt == nil && jsonEqual(old.Bands, g.Bands) {
			continue
		} else if ok {
			g.CreatedAt = old.CreatedAt
		} else {
			g.CreatedAt = now
		}
		g.UpdatedAt = now
		f.games[g.ID] = g
	}
	keepS := map[string]bool{}
	for _, s := range servers {
		keepS[s.ID] = true
		s.OrganizationID = orgID
		old, ok := f.servers[s.ID]
		if ok {
			cmp := old
			cmp.CreatedAt, cmp.UpdatedAt, cmp.RemovedAt = s.CreatedAt, s.UpdatedAt, s.RemovedAt
			if old.RemovedAt == nil && reflect.DeepEqual(cmp, s) {
				continue
			}
			s.CreatedAt = old.CreatedAt
		} else {
			s.CreatedAt = now
		}
		s.UpdatedAt, s.RemovedAt = now, nil
		f.servers[s.ID] = s
	}
	for id, s := range f.servers {
		if !keepS[id] && s.RemovedAt == nil {
			t := now
			s.RemovedAt, s.UpdatedAt = &t, now
			f.servers[id] = s
		}
	}
	for id, g := range f.games {
		if !keepG[id] && g.RemovedAt == nil {
			t := now
			g.RemovedAt, g.UpdatedAt = &t, now
			f.games[id] = g
		}
	}
	return nil
}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	_ = json.Unmarshal(a, &x)
	_ = json.Unmarshal(b, &y)
	return reflect.DeepEqual(x, y)
}
