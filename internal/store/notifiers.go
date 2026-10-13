package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// NotifierCursor is where a notifier is in the events log (gravel#196, ADR-0028 §5).
type NotifierCursor struct {
	Name          string
	AfterID       int64
	LastSentAt    *time.Time
	LastError     string
	FailingSince  *time.Time
	Failures      int
	NextAttemptAt *time.Time
	CreatedAt     time.Time
}

const notifierCursorColumns = `name, after_id, last_sent_at, last_error, failing_since, failures, next_attempt_at, created_at`

func scanNotifierCursor(row interface{ Scan(...any) error }) (NotifierCursor, error) {
	var c NotifierCursor
	err := row.Scan(&c.Name, &c.AfterID, &c.LastSentAt, &c.LastError, &c.FailingSince, &c.Failures, &c.NextAttemptAt, &c.CreatedAt)
	return c, err
}

// NotifierCursor returns a notifier's cursor, made at the log's head (the newest event's id) when
// the notifier has none: a new notifier starts from now, and one that had a cursor before keeps it.
func (s *Store) NotifierCursor(ctx context.Context, orgID uuid.UUID, name string, now time.Time) (NotifierCursor, error) {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO notifier_cursors (organization_id, name, after_id, created_at)
		VALUES ($1, $2, (SELECT coalesce(max(id), 0) FROM events WHERE organization_id = $1), $3)
		ON CONFLICT (organization_id, name) DO NOTHING`, orgID, name, now); err != nil {
		return NotifierCursor{}, fmt.Errorf("store: notifier cursor %s: %w", name, err)
	}
	c, err := scanNotifierCursor(s.pool.QueryRow(ctx, `SELECT `+notifierCursorColumns+` FROM notifier_cursors WHERE organization_id = $1 AND name = $2`, orgID, name))
	if err != nil {
		return NotifierCursor{}, fmt.Errorf("store: notifier cursor %s: %w", name, err)
	}
	return c, nil
}

// NotifierCursors are every notifier's cursor, by name, including those of notifiers the settings
// no longer name.
func (s *Store) NotifierCursors(ctx context.Context, orgID uuid.UUID) ([]NotifierCursor, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+notifierCursorColumns+` FROM notifier_cursors WHERE organization_id = $1 ORDER BY name`, orgID)
	if err != nil {
		return nil, fmt.Errorf("store: notifier cursors: %w", err)
	}
	defer rows.Close()
	var out []NotifierCursor
	for rows.Next() {
		c, err := scanNotifierCursor(rows)
		if err != nil {
			return nil, fmt.Errorf("store: notifier cursors: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: notifier cursors: %w", err)
	}
	return out, nil
}

// AdvanceNotifier moves a notifier's cursor to afterID and clears its failures. sent is when a
// batch was delivered; nil when the cursor only passed over events the notifier's filter left out.
// The cursor never moves back.
func (s *Store) AdvanceNotifier(ctx context.Context, orgID uuid.UUID, name string, afterID int64, sent *time.Time) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE notifier_cursors
		   SET after_id = greatest(after_id, $3), last_sent_at = coalesce($4, last_sent_at),
		       last_error = '', failing_since = NULL, failures = 0, next_attempt_at = NULL
		 WHERE organization_id = $1 AND name = $2`, orgID, name, afterID, sent); err != nil {
		return fmt.Errorf("store: advance notifier %s: %w", name, err)
	}
	return nil
}

// NotifierFailed records a failed delivery: the cursor stays, the failure is counted, and the
// next attempt waits until next.
func (s *Store) NotifierFailed(ctx context.Context, orgID uuid.UUID, name, msg string, at, next time.Time) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE notifier_cursors
		   SET last_error = $3, failing_since = coalesce(failing_since, $4), failures = failures + 1, next_attempt_at = $5
		 WHERE organization_id = $1 AND name = $2`, orgID, name, msg, at, next); err != nil {
		return fmt.Errorf("store: notifier %s failed: %w", name, err)
	}
	return nil
}
