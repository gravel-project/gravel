package serverstest

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/store"
)

// AuditStore keeps audit entries in memory with the database's rules: ids ascend, the outcome
// is written once, nothing else changes.
type AuditStore struct {
	mu      sync.Mutex
	entries []store.AuditEntry
	// FailInsert makes InsertAuditEntry fail (the database is down).
	FailInsert bool
}

// NewAuditStore is an empty audit store.
func NewAuditStore() *AuditStore { return &AuditStore{} }

// InsertAuditEntry appends an unfinished entry.
func (a *AuditStore) InsertAuditEntry(_ context.Context, e store.AuditEntry) (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.FailInsert {
		return 0, errors.New("serverstest: insert failed")
	}
	e.ID = int64(len(a.entries) + 1)
	e.Outcome, e.FinishedAt = "", nil
	a.entries = append(a.entries, e)
	return e.ID, nil
}

// FinishAuditEntry sets an unfinished entry's outcome.
func (a *AuditStore) FinishAuditEntry(_ context.Context, orgID uuid.UUID, id int64, outcome string, at time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if id < 1 || int(id) > len(a.entries) || a.entries[id-1].OrganizationID != orgID || a.entries[id-1].Outcome != "" {
		return store.ErrNotFound
	}
	a.entries[id-1].Outcome, a.entries[id-1].FinishedAt = outcome, &at
	return nil
}

// ListAuditEntries returns entries newest first.
func (a *AuditStore) ListAuditEntries(_ context.Context, orgID uuid.UUID, f store.AuditFilter) ([]store.AuditEntry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []store.AuditEntry
	for _, e := range slices.Backward(a.entries) {
		if e.OrganizationID != orgID || (f.ServerID != "" && e.ServerID != f.ServerID) || (f.BeforeID != 0 && e.ID >= f.BeforeID) {
			continue
		}
		if len(out) == f.Limit {
			break
		}
		out = append(out, e)
	}
	return out, nil
}

// Entries are every entry, oldest first.
func (a *AuditStore) Entries() []store.AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.entries)
}
