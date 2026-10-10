package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// IngestBatch is a row of ingest_batches: one request a pushing source made, as received
// (ADR-0011). Body is never changed; State moves from pending to parsed or failed.
type IngestBatch struct {
	ID             int64
	OrganizationID uuid.UUID
	ServerID       string
	Source         string
	ReceivedAt     time.Time
	Body           []byte
	// Headers are the request headers the source defines, as a JSON object.
	Headers    json.RawMessage
	BodySHA256 []byte
	State      string
}

// Ingest batch states.
const (
	IngestPending = "pending"
	IngestParsed  = "parsed"
	IngestFailed  = "failed"
)

const ingestColumns = `id, organization_id, server_id, source, received_at, body, headers, body_sha256, state`

func scanIngestBatch(row pgx.Row) (IngestBatch, error) {
	var b IngestBatch
	err := row.Scan(&b.ID, &b.OrganizationID, &b.ServerID, &b.Source, &b.ReceivedAt, &b.Body, &b.Headers, &b.BodySHA256, &b.State)
	return b, err
}

// InsertIngestBatch stores a batch as pending and returns its id. The body's hash is computed
// here, so a stored row always matches its body.
func (s *Store) InsertIngestBatch(ctx context.Context, b IngestBatch) (int64, error) {
	headers := b.Headers
	if len(headers) == 0 {
		headers = json.RawMessage(`{}`)
	}
	sum := sha256.Sum256(b.Body)
	var id int64
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO ingest_batches (organization_id, server_id, source, received_at, body, headers, body_sha256)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		b.OrganizationID, b.ServerID, b.Source, b.ReceivedAt, b.Body, headers, sum[:]).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: insert ingest batch: %w", err)
	}
	return id, nil
}

// ListIngestBatches returns up to limit of a server's batches with an id above afterID, oldest
// first.
func (s *Store) ListIngestBatches(ctx context.Context, orgID uuid.UUID, serverID string, afterID int64, limit int) ([]IngestBatch, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+ingestColumns+` FROM ingest_batches
		WHERE organization_id = $1 AND server_id = $2 AND id > $3 ORDER BY id LIMIT $4`, orgID, serverID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list ingest batches: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (IngestBatch, error) { return scanIngestBatch(r) })
	if err != nil {
		return nil, fmt.Errorf("store: list ingest batches: %w", err)
	}
	return out, nil
}

// DeleteIngestBatchesBefore deletes the batches received before cutoff and reports how many.
func (s *Store) DeleteIngestBatchesBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM ingest_batches WHERE received_at < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: delete ingest batches: %w", err)
	}
	return tag.RowsAffected(), nil
}
