// Package backup is the hub's view of its own backups: the status file the backup job writes
// after every base backup, and Postgres's archiver statistics, exposed as metrics so an
// operator alerts on a stale backup or a stalled WAL archive (ADR-0006). The backups
// themselves are taken by WAL-G beside Postgres (deploy/postgres); the hub only watches.
package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/gravel-project/gravel/internal/store"
)

// Status is what gravel-backup writes after a run (deploy/postgres/gravel-backup.sh).
type Status struct {
	OK         bool      `json:"ok"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Error      string    `json:"error"`
	Backup     string    `json:"backup"`
}

// ReadStatus reads a status file. A missing file is not an error: no backup has run yet.
func ReadStatus(path string) (Status, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Status{}, false, nil
	}
	if err != nil {
		return Status{}, false, fmt.Errorf("backup: status: %w", err)
	}
	var s Status
	if err := json.Unmarshal(b, &s); err != nil {
		return Status{}, false, fmt.Errorf("backup: status: %w", err)
	}
	return s, true, nil
}

// Archiver is what the collector needs from the database.
type Archiver interface {
	Archiver(ctx context.Context) (store.ArchiverStatus, error)
}

// Collector exposes the backup status and the archiver statistics at scrape time.
type Collector struct {
	statusFile string
	db         Archiver
	logger     *slog.Logger
	timeout    time.Duration

	lastSuccess  *prometheus.Desc
	lastRun      *prometheus.Desc
	lastRunOK    *prometheus.Desc
	statusOK     *prometheus.Desc
	archived     *prometheus.Desc
	lastArchived *prometheus.Desc
	failed       *prometheus.Desc
	lastFailed   *prometheus.Desc
	archiverOK   *prometheus.Desc
}

// NewCollector builds a collector over the status file (may be "" for none) and the database.
func NewCollector(statusFile string, db Archiver, logger *slog.Logger) *Collector {
	return &Collector{
		statusFile: statusFile, db: db, logger: logger, timeout: 2 * time.Second,
		lastSuccess:  prometheus.NewDesc("gravel_backup_last_success_timestamp_seconds", "When the last successful base backup finished (unix seconds); absent until one has run.", nil, nil),
		lastRun:      prometheus.NewDesc("gravel_backup_last_run_timestamp_seconds", "When the last base backup run finished, successful or not (unix seconds).", nil, nil),
		lastRunOK:    prometheus.NewDesc("gravel_backup_last_run_ok", "1 when the last base backup run succeeded, 0 when it failed.", nil, nil),
		statusOK:     prometheus.NewDesc("gravel_backup_status_readable", "1 when the backup status file was read, 0 when it is configured but unreadable.", nil, nil),
		archived:     prometheus.NewDesc("gravel_wal_archived_total", "WAL segments Postgres reports as archived (pg_stat_archiver.archived_count).", nil, nil),
		lastArchived: prometheus.NewDesc("gravel_wal_last_archived_timestamp_seconds", "When Postgres last archived a WAL segment (unix seconds); absent until one was archived.", nil, nil),
		failed:       prometheus.NewDesc("gravel_wal_archive_failed_total", "WAL archive attempts Postgres reports as failed (pg_stat_archiver.failed_count).", nil, nil),
		lastFailed:   prometheus.NewDesc("gravel_wal_last_failed_timestamp_seconds", "When a WAL archive attempt last failed (unix seconds); absent until one failed.", nil, nil),
		archiverOK:   prometheus.NewDesc("gravel_wal_archiver_readable", "1 when pg_stat_archiver was read at this scrape, 0 when the database did not answer.", nil, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.lastSuccess, c.lastRun, c.lastRunOK, c.statusOK, c.archived, c.lastArchived, c.failed, c.lastFailed, c.archiverOK} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	if c.statusFile != "" {
		s, found, err := ReadStatus(c.statusFile)
		switch {
		case err != nil:
			c.logger.Warn("backup status unreadable", "file", c.statusFile, "error", err.Error())
			ch <- prometheus.MustNewConstMetric(c.statusOK, prometheus.GaugeValue, 0)
		case !found:
			ch <- prometheus.MustNewConstMetric(c.statusOK, prometheus.GaugeValue, 1)
		default:
			ch <- prometheus.MustNewConstMetric(c.statusOK, prometheus.GaugeValue, 1)
			ch <- prometheus.MustNewConstMetric(c.lastRun, prometheus.GaugeValue, float64(s.FinishedAt.Unix()))
			ok := 0.0
			if s.OK {
				ok = 1
				ch <- prometheus.MustNewConstMetric(c.lastSuccess, prometheus.GaugeValue, float64(s.FinishedAt.Unix()))
			}
			ch <- prometheus.MustNewConstMetric(c.lastRunOK, prometheus.GaugeValue, ok)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	a, err := c.db.Archiver(ctx)
	if err != nil {
		c.logger.Warn("archiver status unreadable", "error", err.Error())
		ch <- prometheus.MustNewConstMetric(c.archiverOK, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.archiverOK, prometheus.GaugeValue, 1)
	ch <- prometheus.MustNewConstMetric(c.archived, prometheus.CounterValue, float64(a.ArchivedCount))
	ch <- prometheus.MustNewConstMetric(c.failed, prometheus.CounterValue, float64(a.FailedCount))
	if a.LastArchivedTime != nil {
		ch <- prometheus.MustNewConstMetric(c.lastArchived, prometheus.GaugeValue, float64(a.LastArchivedTime.Unix()))
	}
	if a.LastFailedTime != nil {
		ch <- prometheus.MustNewConstMetric(c.lastFailed, prometheus.GaugeValue, float64(a.LastFailedTime.Unix()))
	}
}
