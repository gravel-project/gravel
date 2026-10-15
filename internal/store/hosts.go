package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ServerHost is a row of server_hosts: the hub's record of the VM a cloud-VM server runs on
// (ADR-0003). The cloud is the truth about what exists; the row is what the hub wants (Desired,
// TargetSize, the destroy flags) and where it got to (State, Step, Operation). One process writes
// it, the hub's cloud-VM manager, which serializes its writes.
type ServerHost struct {
	OrganizationID    uuid.UUID
	ServerID          string
	LocationID        string
	Desired           string
	State             string
	Step              string
	Size              string
	TargetSize        string
	DestroyVolume     bool
	ReleaseAddress    bool
	DiskGiB           int
	InstanceID        string
	InstanceAddress   string
	VolumeID          string
	Address           string
	AddressReserved   bool
	FirewallID        string
	Egress            []string
	Operation         json.RawMessage
	HostKey           string
	EnvSHA256         []byte
	Attempts          int
	NextAttemptAt     *time.Time
	ErrorCode         string
	LastError         string
	RequestAuditID    *int64
	StateSince        time.Time
	InstanceCreatedAt *time.Time
	EmptySince        *time.Time
	BootSeconds       *int
	ReadySeconds      *int
	// The scaling policy's (migration 00018): a size the owner pinned, a decision waiting for a
	// boundary, and when the last resize began.
	PinnedSize    string
	PendingSize   string
	PendingReason string
	PendingSince  *time.Time
	LastResizeAt  *time.Time
	// The expected population (migration 00019): players, until when, and why; 0 players for none.
	ExpectedPlayers int
	ExpectedUntil   *time.Time
	ExpectedReason  string
	// Reboot is a reboot asked and not yet started (migration 00030, gravel#202).
	Reboot    bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ServerHostEvent is a row of server_host_events: one transition of a host.
type ServerHostEvent struct {
	ID             int64
	OrganizationID uuid.UUID
	ServerID       string
	At             time.Time
	FromState      string
	ToState        string
	Step           string
	Detail         string
	Size           string
	PriceHourlyUSD *float64
	AuditID        *int64
}

const hostColumns = `organization_id, server_id, location_id, desired, state, step, size, target_size, destroy_volume, release_address,
	disk_gib, instance_id, instance_address, volume_id, address, address_reserved, firewall_id, egress, operation, host_key,
	env_sha256, attempts, next_attempt_at, error_code, last_error, request_audit_id, state_since, instance_created_at,
	empty_since, boot_seconds, ready_seconds, pinned_size, pending_size, pending_reason, pending_since, last_resize_at,
	expected_players, expected_until, expected_reason, reboot, created_at, updated_at`

func scanHost(row pgx.Row) (ServerHost, error) {
	var h ServerHost
	var egress json.RawMessage
	err := row.Scan(&h.OrganizationID, &h.ServerID, &h.LocationID, &h.Desired, &h.State, &h.Step, &h.Size, &h.TargetSize,
		&h.DestroyVolume, &h.ReleaseAddress, &h.DiskGiB, &h.InstanceID, &h.InstanceAddress, &h.VolumeID, &h.Address,
		&h.AddressReserved, &h.FirewallID, &egress, &h.Operation, &h.HostKey, &h.EnvSHA256, &h.Attempts, &h.NextAttemptAt,
		&h.ErrorCode, &h.LastError, &h.RequestAuditID, &h.StateSince, &h.InstanceCreatedAt, &h.EmptySince, &h.BootSeconds,
		&h.ReadySeconds, &h.PinnedSize, &h.PendingSize, &h.PendingReason, &h.PendingSince, &h.LastResizeAt,
		&h.ExpectedPlayers, &h.ExpectedUntil, &h.ExpectedReason, &h.Reboot, &h.CreatedAt, &h.UpdatedAt)
	if err != nil {
		return h, err
	}
	if err := json.Unmarshal(egress, &h.Egress); err != nil {
		return h, err
	}
	if len(h.Egress) == 0 {
		h.Egress = nil
	}
	return h, nil
}

// GetServerHost is a server's host row, or ErrNotFound.
func (s *Store) GetServerHost(ctx context.Context, orgID uuid.UUID, serverID string) (ServerHost, error) {
	h, err := scanHost(s.pool.QueryRow(ctx, `SELECT `+hostColumns+` FROM server_hosts WHERE organization_id = $1 AND server_id = $2`, orgID, serverID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ServerHost{}, ErrNotFound
	}
	if err != nil {
		return ServerHost{}, fmt.Errorf("store: get server host: %w", err)
	}
	return h, nil
}

// ListServerHosts is every host row of an organization, by server id.
func (s *Store) ListServerHosts(ctx context.Context, orgID uuid.UUID) ([]ServerHost, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+hostColumns+` FROM server_hosts WHERE organization_id = $1 ORDER BY server_id`, orgID)
	if err != nil {
		return nil, fmt.Errorf("store: list server hosts: %w", err)
	}
	defer rows.Close()
	var out []ServerHost
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list server hosts: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list server hosts: %w", err)
	}
	return out, nil
}

// SaveServerHost writes a host's whole row (insert or replace); updated_at is now.
func (s *Store) SaveServerHost(ctx context.Context, h ServerHost, now time.Time) error {
	egress, err := json.Marshal(nonNilStrings(h.Egress))
	if err != nil {
		return fmt.Errorf("store: save server host: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: save server host: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// A step that starts failing (an error code where there was none) or stops: one event each.
	var before string
	if err := tx.QueryRow(ctx, `SELECT error_code FROM server_hosts WHERE organization_id = $1 AND server_id = $2 FOR UPDATE`,
		h.OrganizationID, h.ServerID).Scan(&before); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("store: save server host: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO server_hosts (`+hostColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24,
		        $25, $26, $27, $28, $29, $30, $31, $32, $33, $34, $35, $36, $37, $38, $39, $40, $41, $41)
		ON CONFLICT (organization_id, server_id) DO UPDATE SET
		  location_id = excluded.location_id, desired = excluded.desired, state = excluded.state, step = excluded.step,
		  size = excluded.size, target_size = excluded.target_size, destroy_volume = excluded.destroy_volume,
		  release_address = excluded.release_address, disk_gib = excluded.disk_gib, instance_id = excluded.instance_id,
		  instance_address = excluded.instance_address, volume_id = excluded.volume_id, address = excluded.address,
		  address_reserved = excluded.address_reserved, firewall_id = excluded.firewall_id, egress = excluded.egress,
		  operation = excluded.operation, host_key = excluded.host_key, env_sha256 = excluded.env_sha256,
		  attempts = excluded.attempts, next_attempt_at = excluded.next_attempt_at, error_code = excluded.error_code,
		  last_error = excluded.last_error, request_audit_id = excluded.request_audit_id, state_since = excluded.state_since,
		  instance_created_at = excluded.instance_created_at, empty_since = excluded.empty_since,
		  boot_seconds = excluded.boot_seconds, ready_seconds = excluded.ready_seconds, pinned_size = excluded.pinned_size,
		  pending_size = excluded.pending_size, pending_reason = excluded.pending_reason,
		  pending_since = excluded.pending_since, last_resize_at = excluded.last_resize_at,
		  expected_players = excluded.expected_players, expected_until = excluded.expected_until,
		  expected_reason = excluded.expected_reason, reboot = excluded.reboot, updated_at = $41`,
		h.OrganizationID, h.ServerID, h.LocationID, h.Desired, h.State, h.Step, h.Size, h.TargetSize, h.DestroyVolume,
		h.ReleaseAddress, h.DiskGiB, h.InstanceID, h.InstanceAddress, h.VolumeID, h.Address, h.AddressReserved, h.FirewallID,
		egress, nullJSON(h.Operation), h.HostKey, h.EnvSHA256, h.Attempts, h.NextAttemptAt, h.ErrorCode, h.LastError,
		h.RequestAuditID, h.StateSince, h.InstanceCreatedAt, h.EmptySince, h.BootSeconds, h.ReadySeconds, h.PinnedSize,
		h.PendingSize, h.PendingReason, h.PendingSince, h.LastResizeAt, h.ExpectedPlayers, h.ExpectedUntil, h.ExpectedReason, h.Reboot, now)
	if err != nil {
		return fmt.Errorf("store: save server host: %w", err)
	}
	switch {
	case before == "" && h.ErrorCode != "":
		err = s.appendEvent(ctx, tx, Event{OrganizationID: h.OrganizationID, At: now, Kind: "host.failing", ServerID: h.ServerID,
			Summary: h.ServerID + ": " + h.Step + " keeps failing (" + h.ErrorCode + ")", Detail: map[string]any{"step": h.Step, "error_code": h.ErrorCode, "state": h.State}})
	case before != "" && h.ErrorCode == "":
		err = s.appendEvent(ctx, tx, Event{OrganizationID: h.OrganizationID, At: now, Kind: "host.recovered", ServerID: h.ServerID,
			Summary: h.ServerID + ": " + before + " cleared", Detail: map[string]any{"error_code": before, "state": h.State}})
	}
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: save server host: %w", err)
	}
	return nil
}

// InsertServerHostEvent appends one transition.
func (s *Store) InsertServerHostEvent(ctx context.Context, e ServerHostEvent) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: insert server host event: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id int64
	err = tx.QueryRow(ctx, `
		INSERT INTO server_host_events (organization_id, server_id, at, from_state, to_state, step, detail, size, price_hourly_usd, audit_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`,
		e.OrganizationID, e.ServerID, e.At, e.FromState, e.ToState, e.Step, e.Detail, e.Size, e.PriceHourlyUSD, e.AuditID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: insert server host event: %w", err)
	}
	what := e.FromState + " → " + e.ToState
	if e.FromState == e.ToState {
		what = e.Step
		if what == "" {
			what = "step"
		}
	}
	if err := s.appendEvent(ctx, tx, Event{OrganizationID: e.OrganizationID, At: e.At, Kind: HostEventKind(e.FromState, e.ToState, e.Step),
		ServerID: e.ServerID, Summary: e.ServerID + ": " + what,
		Detail: map[string]any{"from": e.FromState, "to": e.ToState, "step": e.Step, "detail": e.Detail, "size": e.Size},
		Source: "server_host_events", SourceID: fmt.Sprint(id)}); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: insert server host event: %w", err)
	}
	return id, nil
}

// ListServerHostEvents is a page of a host's transitions, newest first: at most limit before the
// id (0 = the newest).
func (s *Store) ListServerHostEvents(ctx context.Context, orgID uuid.UUID, serverID string, beforeID int64, limit int) ([]ServerHostEvent, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, organization_id, server_id, at, from_state, to_state, step, detail, size, price_hourly_usd::float8, audit_id
		  FROM server_host_events
		 WHERE organization_id = $1 AND server_id = $2 AND ($3 = 0 OR id < $3)
		 ORDER BY id DESC LIMIT $4`, orgID, serverID, beforeID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list server host events: %w", err)
	}
	defer rows.Close()
	var out []ServerHostEvent
	for rows.Next() {
		var e ServerHostEvent
		if err := rows.Scan(&e.ID, &e.OrganizationID, &e.ServerID, &e.At, &e.FromState, &e.ToState, &e.Step, &e.Detail, &e.Size, &e.PriceHourlyUSD, &e.AuditID); err != nil {
			return nil, fmt.Errorf("store: list server host events: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list server host events: %w", err)
	}
	return out, nil
}

// RunningCostSince is what a host's instance cost from a time to now, by its events: each run at
// the hourly price its transition into running recorded, so the rungs of a scaling ladder add up
// at their own prices. A run with no price recorded counts nothing.
func (s *Store) RunningCostSince(ctx context.Context, orgID uuid.UUID, serverID string, from, now time.Time) (float64, error) {
	var usd float64
	err := s.pool.QueryRow(ctx, `
		WITH ev AS (
		  SELECT at, to_state, price_hourly_usd, lead(at) OVER (ORDER BY id) AS next_at
		    FROM server_host_events WHERE organization_id = $1 AND server_id = $2 AND to_state <> from_state
		)
		SELECT coalesce(sum(extract(epoch FROM (least(coalesce(next_at, $4), $4) - greatest(at, $3))) / 3600
		                    * coalesce(price_hourly_usd, 0)), 0)::float8
		  FROM ev
		 WHERE to_state = 'running' AND coalesce(next_at, $4) > $3 AND at < $4`,
		orgID, serverID, from, now).Scan(&usd)
	if err != nil {
		return 0, fmt.Errorf("store: running cost: %w", err)
	}
	return usd, nil
}

// RunningSecondsSince is how long a host ran (its state was running) from a time to now, by its
// events: each run is from a transition into running to the next transition, or now.
func (s *Store) RunningSecondsSince(ctx context.Context, orgID uuid.UUID, serverID string, from, now time.Time) (int64, error) {
	var secs float64
	err := s.pool.QueryRow(ctx, `
		WITH ev AS (
		  SELECT at, to_state, lead(at) OVER (ORDER BY id) AS next_at
		    FROM server_host_events WHERE organization_id = $1 AND server_id = $2
		)
		SELECT coalesce(sum(extract(epoch FROM (least(coalesce(next_at, $4), $4) - greatest(at, $3)))), 0)
		  FROM ev
		 WHERE to_state = 'running' AND coalesce(next_at, $4) > $3 AND at < $4`,
		orgID, serverID, from, now).Scan(&secs)
	if err != nil {
		return 0, fmt.Errorf("store: running seconds: %w", err)
	}
	return int64(secs), nil
}
