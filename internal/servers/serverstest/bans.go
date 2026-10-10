package serverstest

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/store"
)

// BanStore keeps ban lists in memory with the database's rules: one adoption per server, one
// active ban per identity and server, lifting marks a ban lifted.
type BanStore struct {
	mu      sync.Mutex
	adopted map[string]time.Time
	bans    []store.Ban
}

// NewBanStore is an empty ban store.
func NewBanStore() *BanStore { return &BanStore{adopted: map[string]time.Time{}} }

// BanListAdopted reports an adoption.
func (b *BanStore) BanListAdopted(_ context.Context, orgID uuid.UUID, serverID string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.adopted[orgID.String()+"/"+serverID]
	return ok, nil
}

// AdoptBanList records an adoption and imports bans once.
func (b *BanStore) AdoptBanList(_ context.Context, orgID uuid.UUID, serverID string, imported []store.Ban, at time.Time) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	k := orgID.String() + "/" + serverID
	if _, ok := b.adopted[k]; ok {
		return false, nil
	}
	b.adopted[k] = at
	for _, x := range imported {
		if b.activeLocked(orgID, serverID, x.Provider, x.Subject) < 0 {
			x.ID, x.OrganizationID, x.ServerID, x.BannedAt = int64(len(b.bans)+1), orgID, serverID, at
			b.bans = append(b.bans, x)
		}
	}
	return true, nil
}

// ActiveBans are the active bans, oldest first.
func (b *BanStore) ActiveBans(_ context.Context, orgID uuid.UUID, serverID string) ([]store.Ban, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []store.Ban
	for _, x := range b.bans {
		if x.OrganizationID == orgID && x.ServerID == serverID && x.LiftedAt == nil {
			out = append(out, x)
		}
	}
	return out, nil
}

// AddBan records an active ban.
func (b *BanStore) AddBan(_ context.Context, x store.Ban) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.activeLocked(x.OrganizationID, x.ServerID, x.Provider, x.Subject) >= 0 {
		return 0, store.ErrBanExists
	}
	x.ID = int64(len(b.bans) + 1)
	b.bans = append(b.bans, x)
	return x.ID, nil
}

// LiftBan marks an active ban lifted.
func (b *BanStore) LiftBan(_ context.Context, orgID uuid.UUID, serverID, provider, subject string, liftAuditID *int64, at time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	i := b.activeLocked(orgID, serverID, provider, subject)
	if i < 0 {
		return store.ErrNotFound
	}
	b.bans[i].LiftedAt, b.bans[i].LiftAuditID = &at, liftAuditID
	return nil
}

// All are every ban, lifted ones included, oldest first.
func (b *BanStore) All() []store.Ban {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.bans)
}

func (b *BanStore) activeLocked(orgID uuid.UUID, serverID, provider, subject string) int {
	return slices.IndexFunc(b.bans, func(x store.Ban) bool {
		return x.OrganizationID == orgID && x.ServerID == serverID && x.Provider == provider && x.Subject == subject && x.LiftedAt == nil
	})
}
