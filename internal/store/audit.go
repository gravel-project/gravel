package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AuditEntry is a row of audit_log: one moderation call through the hub (ADR-0010 §5). It is
// written before the call and finished once with its outcome; the database refuses any other
// change.
type AuditEntry struct {
	ID             int64
	OrganizationID uuid.UUID
	At             time.Time
	// Exactly one of ActorUserID and ActorAppID is set.
	ActorUserID *uuid.UUID
	ActorAppID  *uuid.UUID
	// ActorAppName is the app's name, read back with the row (not stored on it).
	ActorAppName     string
	OnBehalfProvider string
	OnBehalfSubject  string
	ServerID         string
	Action           string
	TargetProvider   string
	TargetSubject    string
	Reason           string
	// Detail is what else the action carried, as a JSON object.
	Detail    json.RawMessage
	RequestID string
	// Outcome is "" until the entry is finished.
	Outcome    string
	FinishedAt *time.Time
}

const auditColumns = `a.id, a.organization_id, a.at, a.actor_user_id, a.actor_app_id, COALESCE(p.name, ''), a.on_behalf_provider, a.on_behalf_subject,
	a.server_id, a.action, a.target_provider, a.target_subject, a.reason, a.detail, a.request_id, COALESCE(a.outcome, ''), a.finished_at`

func scanAudit(row pgx.Row) (AuditEntry, error) {
	var e AuditEntry
	err := row.Scan(&e.ID, &e.OrganizationID, &e.At, &e.ActorUserID, &e.ActorAppID, &e.ActorAppName, &e.OnBehalfProvider, &e.OnBehalfSubject,
		&e.ServerID, &e.Action, &e.TargetProvider, &e.TargetSubject, &e.Reason, &e.Detail, &e.RequestID, &e.Outcome, &e.FinishedAt)
	return e, err
}

// InsertAuditEntry records a call before it is made and returns the entry's id. The outcome
// and FinishedAt are ignored: FinishAuditEntry sets them.
func (s *Store) InsertAuditEntry(ctx context.Context, e AuditEntry) (int64, error) {
	detail := e.Detail
	if len(detail) == 0 {
		detail = json.RawMessage(`{}`)
	}
	var id int64
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO audit_log (organization_id, at, actor_user_id, actor_app_id, on_behalf_provider, on_behalf_subject,
		                       server_id, action, target_provider, target_subject, reason, detail, request_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) RETURNING id`,
		e.OrganizationID, e.At, e.ActorUserID, e.ActorAppID, e.OnBehalfProvider, e.OnBehalfSubject,
		e.ServerID, e.Action, e.TargetProvider, e.TargetSubject, e.Reason, detail, e.RequestID).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: insert audit entry: %w", err)
	}
	return id, nil
}

// FinishAuditEntry records an entry's outcome; ErrNotFound when the entry does not exist or is
// already finished.
func (s *Store) FinishAuditEntry(ctx context.Context, orgID uuid.UUID, id int64, outcome string, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `UPDATE audit_log SET outcome = $3, finished_at = $4 WHERE organization_id = $1 AND id = $2 AND outcome IS NULL`,
		orgID, id, outcome, at)
	if err != nil {
		return fmt.Errorf("store: finish audit entry: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// AuditFilter narrows ListAuditEntries.
type AuditFilter struct {
	// ServerID is one server's entries; "" is every server's.
	ServerID string
	// BeforeID pages backwards: entries with a smaller id; 0 starts at the newest.
	BeforeID int64
	// Limit is the page size.
	Limit int
}

// ListAuditEntries returns entries newest first.
func (s *Store) ListAuditEntries(ctx context.Context, orgID uuid.UUID, f AuditFilter) ([]AuditEntry, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+auditColumns+` FROM audit_log a LEFT JOIN apps p ON p.id = a.actor_app_id
		WHERE a.organization_id = $1 AND ($2 = '' OR a.server_id = $2) AND ($3 = 0 OR a.id < $3)
		ORDER BY a.id DESC LIMIT $4`, orgID, f.ServerID, f.BeforeID, f.Limit)
	if err != nil {
		return nil, fmt.Errorf("store: list audit entries: %w", err)
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		e, err := scanAudit(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list audit entries: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list audit entries: %w", err)
	}
	return out, nil
}
