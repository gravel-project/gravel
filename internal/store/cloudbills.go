package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CloudBill is one cloud location's bill so far as read on one day (ADR-0034).
type CloudBill struct {
	OrganizationID uuid.UUID
	LocationID     string
	TakenOn        time.Time // the day, in the organization's timezone (only the date counts)
	Month          time.Time // the bill's month: its first day (only the date counts)
	GeneratedAt    *time.Time
	TakenAt        time.Time
	Lines          []CloudBillLine
}

// CloudBillLine is one charge on a bill, with the server it is for ("" for none).
type CloudBillLine struct {
	ResourceID  string
	Name        string
	Product     string
	Kind        string
	Description string
	USD         float64
	ServerID    string
}

// SaveCloudBill replaces a location's snapshot for its day, lines and all, in one transaction.
func (s *Store) SaveCloudBill(ctx context.Context, b CloudBill) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: save cloud bill: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	day, month := dateOnly(b.TakenOn), dateOnly(b.Month)
	if _, err := tx.Exec(ctx, `DELETE FROM cloud_bills WHERE organization_id = $1 AND location_id = $2 AND taken_on = $3`,
		b.OrganizationID, b.LocationID, day); err != nil {
		return fmt.Errorf("store: save cloud bill: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO cloud_bills (organization_id, location_id, taken_on, month, generated_at, taken_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, b.OrganizationID, b.LocationID, day, month, b.GeneratedAt, b.TakenAt); err != nil {
		return fmt.Errorf("store: save cloud bill: %w", err)
	}
	batch := &pgx.Batch{}
	for i, l := range b.Lines {
		batch.Queue(`INSERT INTO cloud_bill_lines (organization_id, location_id, taken_on, line, resource_id, name, product, kind, description, usd, server_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			b.OrganizationID, b.LocationID, day, i, l.ResourceID, l.Name, l.Product, l.Kind, l.Description, l.USD, l.ServerID)
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("store: save cloud bill: lines: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: save cloud bill: %w", err)
	}
	return nil
}

// CloudBills are every snapshot of a month's bills, by location then day, with their lines.
func (s *Store) CloudBills(ctx context.Context, orgID uuid.UUID, month time.Time) ([]CloudBill, error) {
	rows, err := s.pool.Query(ctx, `SELECT b.location_id, b.taken_on, b.month, b.generated_at, b.taken_at,
		       l.line, l.resource_id, l.name, l.product, l.kind, l.description, l.usd::float8, l.server_id
		  FROM cloud_bills b LEFT JOIN cloud_bill_lines l
		    ON l.organization_id = b.organization_id AND l.location_id = b.location_id AND l.taken_on = b.taken_on
		 WHERE b.organization_id = $1 AND b.month = $2
		 ORDER BY b.location_id, b.taken_on, l.line`, orgID, dateOnly(month))
	if err != nil {
		return nil, fmt.Errorf("store: cloud bills: %w", err)
	}
	defer rows.Close()
	var out []CloudBill
	for rows.Next() {
		var b CloudBill
		var line *int
		var l struct {
			resourceID, name, product, kind, description, serverID *string
			usd                                                    *float64
		}
		if err := rows.Scan(&b.LocationID, &b.TakenOn, &b.Month, &b.GeneratedAt, &b.TakenAt, &line, &l.resourceID, &l.name, &l.product, &l.kind,
			&l.description, &l.usd, &l.serverID); err != nil {
			return nil, fmt.Errorf("store: cloud bills: %w", err)
		}
		b.OrganizationID = orgID
		if n := len(out); n == 0 || out[n-1].LocationID != b.LocationID || !out[n-1].TakenOn.Equal(b.TakenOn) {
			out = append(out, b)
		}
		if line != nil {
			last := &out[len(out)-1]
			last.Lines = append(last.Lines, CloudBillLine{ResourceID: *l.resourceID, Name: *l.name, Product: *l.product, Kind: *l.kind,
				Description: *l.description, USD: *l.usd, ServerID: *l.serverID})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: cloud bills: %w", err)
	}
	return out, nil
}

// DayCost is what one server's host cost on one day.
type DayCost struct {
	ServerID string
	Day      time.Time // the date, in the timezone asked for
	USD      float64
}

// RunningCostByDay is every cloud-VM server's instance cost from from to to, day by day in
// timezone, by the host events (ADR-0034's estimate): each run at the hourly price its
// transition into running recorded, cut at midnight, as RunningCostSince counts one server. A
// run with no price recorded counts nothing; a day a server didn't run is absent.
func (s *Store) RunningCostByDay(ctx context.Context, orgID uuid.UUID, from, to time.Time, timezone string) ([]DayCost, error) {
	if !from.Before(to) {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		WITH ev AS (
		  SELECT server_id, at, to_state, price_hourly_usd, lead(at) OVER (PARTITION BY server_id ORDER BY id) AS next_at
		    FROM server_host_events WHERE organization_id = $1 AND to_state <> from_state
		), runs AS (
		  SELECT server_id, greatest(at, $2) AS s, least(coalesce(next_at, $3), $3) AS e, coalesce(price_hourly_usd, 0)::float8 AS p
		    FROM ev WHERE to_state = 'running' AND coalesce(next_at, $3) > $2 AND at < $3
		), days AS (
		  SELECT d::date AS day, d AT TIME ZONE $4 AS ds, (d + interval '1 day') AT TIME ZONE $4 AS de
		    FROM generate_series(($2::timestamptz AT TIME ZONE $4)::date::timestamp, (($3::timestamptz - interval '1 microsecond') AT TIME ZONE $4)::date::timestamp,
		                         interval '1 day') AS d
		)
		SELECT r.server_id, days.day, sum(extract(epoch FROM least(r.e, days.de) - greatest(r.s, days.ds)) / 3600 * r.p)::float8
		  FROM runs r JOIN days ON r.s < days.de AND r.e > days.ds
		 GROUP BY r.server_id, days.day
		 ORDER BY days.day, r.server_id`, orgID, from, to, timezone)
	if err != nil {
		return nil, fmt.Errorf("store: running cost by day: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (DayCost, error) {
		var d DayCost
		err := r.Scan(&d.ServerID, &d.Day, &d.USD)
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: running cost by day: %w", err)
	}
	return out, nil
}
