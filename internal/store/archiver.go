package store

import (
	"context"
	"fmt"
	"time"
)

// ArchiverStatus is Postgres's own account of WAL archiving (pg_stat_archiver): how many
// segments reached the archive, which was the last and when, and the failures.
type ArchiverStatus struct {
	ArchivedCount    int64
	LastArchivedWAL  string
	LastArchivedTime *time.Time
	FailedCount      int64
	LastFailedWAL    string
	LastFailedTime   *time.Time
}

// Archiver reads pg_stat_archiver. With archive_mode off every field is zero.
func (s *Store) Archiver(ctx context.Context) (ArchiverStatus, error) {
	var a ArchiverStatus
	var lastWAL, failedWAL *string
	err := s.pool.QueryRow(ctx,
		`SELECT archived_count, last_archived_wal, last_archived_time, failed_count, last_failed_wal, last_failed_time FROM pg_stat_archiver`).
		Scan(&a.ArchivedCount, &lastWAL, &a.LastArchivedTime, &a.FailedCount, &failedWAL, &a.LastFailedTime)
	if err != nil {
		return ArchiverStatus{}, fmt.Errorf("store: archiver status: %w", err)
	}
	if lastWAL != nil {
		a.LastArchivedWAL = *lastWAL
	}
	if failedWAL != nil {
		a.LastFailedWAL = *failedWAL
	}
	return a, nil
}
