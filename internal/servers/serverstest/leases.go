package serverstest

import (
	"context"
	"slices"
	"sync"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/store"
)

// NewLeaseRows is an empty LeaseRows.
func NewLeaseRows() *LeaseRows { return &LeaseRows{rows: map[uuid.UUID]store.ServerLease{}} }

// Len is how many leases there are.
func (f *LeaseRows) Len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

// All are the leases, in no order.
func (f *LeaseRows) All() []store.ServerLease {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.ServerLease, 0, len(f.rows))
	for _, l := range f.rows {
		out = append(out, l)
	}
	return out
}

// LeaseRows is an in-memory servers.LeaseRows with the database's live-lease uniqueness.
type LeaseRows struct {
	mu   sync.Mutex
	rows map[uuid.UUID]store.ServerLease
}

func (f *LeaseRows) CreateServerLease(_ context.Context, l store.ServerLease) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if !r.Live() {
			continue
		}
		if r.ServerID == l.ServerID {
			return store.ErrLeaseServerTaken
		}
		if r.HolderKind == l.HolderKind && r.Holder == l.Holder && r.HolderRef == l.HolderRef {
			return store.ErrLeaseRefTaken
		}
	}
	f.rows[l.ID] = l
	return nil
}

func (f *LeaseRows) UpdateServerLease(_ context.Context, l store.ServerLease) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rows[l.ID]; !ok {
		return store.ErrNotFound
	}
	f.rows[l.ID] = l
	return nil
}

func (f *LeaseRows) GetServerLease(_ context.Context, _, id uuid.UUID) (store.ServerLease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.rows[id]
	if !ok {
		return l, store.ErrNotFound
	}
	return l, nil
}

func (f *LeaseRows) LiveLeaseForRef(_ context.Context, _ uuid.UUID, kind, holder, ref string) (store.ServerLease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.Live() && r.HolderKind == kind && r.Holder == holder && r.HolderRef == ref {
			return r, nil
		}
	}
	return store.ServerLease{}, store.ErrNotFound
}

func (f *LeaseRows) ListServerLeases(_ context.Context, _ uuid.UUID, serverID string, liveOnly bool, limit int) ([]store.ServerLease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.ServerLease
	for _, r := range f.rows {
		if (serverID == "" || r.ServerID == serverID) && (!liveOnly || r.Live()) {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b store.ServerLease) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out[:min(limit, len(out))], nil
}
