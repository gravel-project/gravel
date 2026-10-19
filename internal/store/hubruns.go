package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// HubRun is one start of the hub: when it started, and the last time it was known to run
// (migration 37).
type HubRun struct {
	ID                 int64
	StartedAt, AliveAt time.Time
}

// StartHubRun records a start of the hub at at, and returns it with the run before it; ok is false
// when there was none (a new hub, or the first start since the hub kept its runs).
func (s *Store) StartHubRun(ctx context.Context, orgID uuid.UUID, at time.Time) (run, prev HubRun, ok bool, err error) {
	var prevID *int64
	var prevStarted, prevAlive *time.Time
	err = s.pool.QueryRow(ctx, `WITH prev AS (
		  SELECT id, started_at, alive_at FROM hub_runs WHERE organization_id = $1 ORDER BY id DESC LIMIT 1
		), run AS (
		  INSERT INTO hub_runs (organization_id, started_at, alive_at) VALUES ($1, $2, $2) RETURNING id
		)
		SELECT run.id, prev.id, prev.started_at, prev.alive_at FROM run LEFT JOIN prev ON true`,
		orgID, at).Scan(&run.ID, &prevID, &prevStarted, &prevAlive)
	if err != nil {
		return HubRun{}, HubRun{}, false, fmt.Errorf("store: start hub run: %w", err)
	}
	run.StartedAt, run.AliveAt = at, at
	if prevID != nil {
		prev, ok = HubRun{ID: *prevID, StartedAt: *prevStarted, AliveAt: *prevAlive}, true
	}
	return run, prev, ok, nil
}

// HubRunAlive says a run was still running at at; a time before the one it holds changes nothing.
func (s *Store) HubRunAlive(ctx context.Context, id int64, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `UPDATE hub_runs SET alive_at = greatest(alive_at, $2) WHERE id = $1`, id, at)
	if err != nil {
		return fmt.Errorf("store: hub run alive: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("store: hub run alive: run %d: %w", id, ErrNotFound)
	}
	return nil
}
