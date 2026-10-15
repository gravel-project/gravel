package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gravel-project/gravel/internal/events"
)

// Event is a row of the events log (gravel#188, ADR-0028).
type Event struct {
	ID             int64
	OrganizationID uuid.UUID // uuid.Nil on append: the subject member's organization
	At             time.Time
	Kind           string
	Severity       string // set on append from the registry
	ActorUserID    *uuid.UUID
	ActorAppID     *uuid.UUID
	OnBehalfProvider,
	OnBehalfSubject string
	ServerID      string // the game is the server's, read on append
	GameID        string
	SubjectUserID *uuid.UUID
	// Summary is one line; "{user}" is replaced on append with the subject member's display name.
	Summary   string
	Detail    map[string]any
	Source    string
	SourceID  string
	RequestID string
	// ActorUserName and ActorAppName are who acted, as the hub names them when the log is read
	// (ListEvents); never written.
	ActorUserName, ActorAppName string
}

// Actor is who made a change whose own row doesn't say: a member in person, an app, a member's
// token (both), or neither, from the hub's command line on the host.
type Actor struct {
	UserID *uuid.UUID
	AppID  *uuid.UUID
	// Via is how the change came: ViaAPI (the API, the pages through it included) or ViaCLI.
	Via       string
	RequestID string
}

// How a change came to the hub.
const (
	ViaAPI = "api"
	ViaCLI = "cli"
)

// event fills an event's actor and request from a, and records how it came in its detail.
func (a Actor) event(e Event) Event {
	e.ActorUserID, e.ActorAppID, e.RequestID = a.UserID, a.AppID, a.RequestID
	if e.Detail == nil {
		e.Detail = map[string]any{}
	}
	if a.Via != "" {
		e.Detail["via"] = a.Via
	}
	return e
}

// rowQuerier is a transaction or the pool.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// OnEvent tells f each event kind appended (the hub counts them). f runs inside the source's
// transaction, so a rolled-back write may count once too often.
func (s *Store) OnEvent(f func(kind string)) { s.onEvent = f }

// appendEvent writes an event in the caller's transaction, with the registry's severity: an
// unknown kind fails the transaction. An event whose (source, source_id) is already in the log is
// skipped, so a replayed source writes nothing twice.
func (s *Store) appendEvent(ctx context.Context, q rowQuerier, e Event) error {
	_, err := s.appendEventID(ctx, q, e)
	return err
}

// appendEventID is appendEvent, answering the event's id: 0 when the source row's event is in the
// log already.
func (s *Store) appendEventID(ctx context.Context, q rowQuerier, e Event) (int64, error) {
	sev, err := events.SeverityOf(e.Kind)
	if err != nil {
		return 0, err
	}
	detail, err := json.Marshal(nonNilAnyMap(e.Detail))
	if err != nil {
		return 0, fmt.Errorf("store: event detail: %w", err)
	}
	var org *uuid.UUID
	if e.OrganizationID != uuid.Nil {
		org = &e.OrganizationID
	}
	var id int64
	err = q.QueryRow(ctx, `
		WITH o AS (SELECT coalesce($1::uuid, (SELECT organization_id FROM users WHERE id = $10)) AS id)
		INSERT INTO events (organization_id, at, kind, severity, actor_user_id, actor_app_id, on_behalf_provider, on_behalf_subject,
		                    server_id, game_id, subject_user_id, summary, detail, source, source_id, request_id)
		SELECT o.id, $2, $3, $4, $5, $6, $7, $8, $9,
		       coalesce(nullif($11, ''), (SELECT game_id FROM managed_servers m WHERE m.organization_id = o.id AND m.id = $9), ''),
		       $10, replace($12, '{user}', coalesce((SELECT display_name FROM users WHERE id = $10), 'a member')), $13, $14, $15, $16
		  FROM o
		ON CONFLICT (source, source_id) WHERE source_id <> '' DO NOTHING
		RETURNING id`,
		org, e.At, e.Kind, string(sev), e.ActorUserID, e.ActorAppID, e.OnBehalfProvider, e.OnBehalfSubject, e.ServerID,
		e.SubjectUserID, e.GameID, e.Summary, detail, e.Source, e.SourceID, e.RequestID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil // already in the log
	}
	if err != nil {
		return 0, fmt.Errorf("store: append event %s: %w", e.Kind, err)
	}
	if s.onEvent != nil {
		s.onEvent(e.Kind)
	}
	return id, nil
}

// AppendEvent writes an event that has no source row of its own (a backup's status).
func (s *Store) AppendEvent(ctx context.Context, e Event) error {
	return s.appendEvent(ctx, s.pool, e)
}

func nonNilAnyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// EventQuery reads the log. With AfterID the events after it come oldest first (following the
// log); otherwise the newest come first, before BeforeID when it is set (paging back).
type EventQuery struct {
	AfterID, BeforeID int64
	Limit             int
	Kinds             []string // a kind ("host.running") or a group ("host")
	MinSeverity       events.Severity
	ServerID          string
	// Games, when not nil, limits the log to these games' events: a game-scoped reader's.
	Games []string
	// Servers, when not empty, limits the log to these servers' events (a notifier's filter).
	Servers []string
	// Follow reads oldest first after AfterID even when it is 0 (a notifier's cursor on a log that
	// was empty when it began).
	Follow bool
	// GameID, when set, is one game's events; with Games, both apply.
	GameID string
	// ActorUserID and ActorAppID, when set, are the events one member or one app did.
	ActorUserID, ActorAppID *uuid.UUID
}

const eventColumns = `e.id, e.organization_id, e.at, e.kind, e.severity, e.actor_user_id, e.actor_app_id, e.on_behalf_provider,
	e.on_behalf_subject, e.server_id, e.game_id, e.subject_user_id, e.summary, e.detail, e.source, e.source_id, e.request_id,
	coalesce(u.display_name, ''), coalesce(a.name, '')`

// ListEvents reads the log and its head, the newest id (0 for an empty log).
func (s *Store) ListEvents(ctx context.Context, orgID uuid.UUID, q EventQuery) ([]Event, int64, error) {
	var head int64
	if err := s.pool.QueryRow(ctx, `SELECT coalesce(max(id), 0) FROM events WHERE organization_id = $1`, orgID).Scan(&head); err != nil {
		return nil, 0, fmt.Errorf("store: events head: %w", err)
	}
	where := []string{"e.organization_id = $1"}
	args := []any{orgID}
	arg := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }
	order := "e.id DESC"
	switch {
	case q.AfterID > 0 || q.Follow:
		where = append(where, "e.id > "+arg(q.AfterID))
		order = "e.id ASC"
	case q.BeforeID > 0:
		where = append(where, "e.id < "+arg(q.BeforeID))
	}
	if len(q.Kinds) > 0 {
		where = append(where, "(e.kind = ANY("+arg(q.Kinds)+") OR split_part(e.kind, '.', 1) = ANY("+arg(q.Kinds)+"))")
	}
	if q.MinSeverity != "" {
		sev := []string{}
		for _, v := range events.AtLeast(q.MinSeverity) {
			sev = append(sev, string(v))
		}
		where = append(where, "e.severity = ANY("+arg(sev)+")")
	}
	if q.ServerID != "" {
		where = append(where, "e.server_id = "+arg(q.ServerID))
	}
	if len(q.Servers) > 0 {
		where = append(where, "e.server_id = ANY("+arg(q.Servers)+")")
	}
	if q.Games != nil {
		where = append(where, "e.game_id = ANY("+arg(q.Games)+")")
	}
	if q.GameID != "" {
		where = append(where, "e.game_id = "+arg(q.GameID))
	}
	if q.ActorUserID != nil {
		where = append(where, "e.actor_user_id = "+arg(*q.ActorUserID))
	}
	if q.ActorAppID != nil {
		where = append(where, "e.actor_app_id = "+arg(*q.ActorAppID))
	}
	limit := q.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT `+eventColumns+` FROM events e
		LEFT JOIN users u ON u.id = e.actor_user_id
		LEFT JOIN apps a ON a.id = e.actor_app_id
		WHERE `+strings.Join(where, " AND ")+` ORDER BY `+order+` LIMIT `+arg(limit), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: list events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var detail []byte
		if err := rows.Scan(&e.ID, &e.OrganizationID, &e.At, &e.Kind, &e.Severity, &e.ActorUserID, &e.ActorAppID, &e.OnBehalfProvider,
			&e.OnBehalfSubject, &e.ServerID, &e.GameID, &e.SubjectUserID, &e.Summary, &detail, &e.Source, &e.SourceID, &e.RequestID,
			&e.ActorUserName, &e.ActorAppName); err != nil {
			return nil, 0, fmt.Errorf("store: list events: %w", err)
		}
		if err := json.Unmarshal(detail, &e.Detail); err != nil {
			return nil, 0, fmt.Errorf("store: list events: detail: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: list events: %w", err)
	}
	return out, head, nil
}

// PruneEvents deletes the organization's events older than cutoff: the only delete the log allows.
func (s *Store) PruneEvents(ctx context.Context, orgID uuid.UUID, cutoff time.Time) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: prune events: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL gravel.events_prune = 'on'`); err != nil {
		return 0, fmt.Errorf("store: prune events: %w", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM events WHERE organization_id = $1 AND at < $2`, orgID, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: prune events: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: prune events: %w", err)
	}
	return tag.RowsAffected(), nil
}

// AuditEventKind is the event kind an audit log action writes: moderation.<action> for the
// moderation actions, action.<action> for the rest (the migration's backfill maps them alike).
func AuditEventKind(action string) string {
	switch action {
	case "kick", "ban", "unban", "message", "broadcast", "move_player":
		return "moderation." + action
	}
	return "action." + action
}

// HostEventKind is the event kind a host's transition or step writes (the backfill maps alike).
func HostEventKind(from, to, step string) string {
	switch {
	case from == "resizing" && to == "running":
		return "host.resized"
	case from != to:
		return "host." + to
	}
	switch step {
	case "expect", "snapshot", "scale", "scale_cancelled", "env_drift":
		return "host." + step
	}
	return "host.step"
}
