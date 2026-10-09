package backup

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/gravel-project/gravel/internal/store"
)

type fakeArchiver struct {
	st  store.ArchiverStatus
	err error
}

func (f fakeArchiver) Archiver(context.Context) (store.ArchiverStatus, error) { return f.st, f.err }

func gather(t *testing.T, c prometheus.Collector) string {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			v := m.GetGauge().GetValue() + m.GetCounter().GetValue()
			b.WriteString(mf.GetName())
			b.WriteString(" ")
			b.WriteString(strconv.FormatFloat(v, 'g', -1, 64))
			b.WriteString("\n")
		}
	}
	return b.String()
}

func TestReadStatus(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "status.json")
	if _, found, err := ReadStatus(path); err != nil || found {
		t.Errorf("missing file: found=%v err=%v", found, err)
	}
	if err := os.WriteFile(path, []byte(`{"ok":true,"started_at":"2026-10-09T02:00:00Z","finished_at":"2026-10-09T02:00:41Z","error":"","backup":"base_000000010000000000000003"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, found, err := ReadStatus(path)
	if err != nil || !found || !s.OK || s.Backup != "base_000000010000000000000003" || !s.FinishedAt.Equal(time.Date(2026, 10, 9, 2, 0, 41, 0, time.UTC)) {
		t.Errorf("read: %+v found=%v err=%v", s, found, err)
	}
	if err := os.WriteFile(path, []byte(`{not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadStatus(path); err == nil {
		t.Error("corrupt file must error")
	}
}

func TestCollector(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	path := filepath.Join(dir, "status.json")
	archived := time.Date(2026, 10, 9, 3, 0, 0, 0, time.UTC)
	failed := archived.Add(-time.Hour)
	db := fakeArchiver{st: store.ArchiverStatus{ArchivedCount: 12, LastArchivedTime: &archived, FailedCount: 1, LastFailedTime: &failed}}

	unix := func(ts time.Time) string { return strconv.FormatFloat(float64(ts.Unix()), 'g', -1, 64) }

	// No status file yet: readable (nothing to read), archiver metrics present.
	out := gather(t, NewCollector(path, db, logger))
	for _, want := range []string{"gravel_backup_status_readable 1", "gravel_wal_archiver_readable 1", "gravel_wal_archived_total 12", "gravel_wal_archive_failed_total 1", "gravel_wal_last_archived_timestamp_seconds " + unix(archived), "gravel_wal_last_failed_timestamp_seconds " + unix(failed)} {
		if !strings.Contains(out, want) {
			t.Errorf("want %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "gravel_backup_last_success") || strings.Contains(out, "gravel_backup_last_run") {
		t.Errorf("no run metrics before a run:\n%s", out)
	}

	// A successful run.
	if err := os.WriteFile(path, []byte(`{"ok":true,"started_at":"2026-10-09T02:00:00Z","finished_at":"2026-10-09T02:00:41Z","error":"","backup":"b1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	finished := time.Date(2026, 10, 9, 2, 0, 41, 0, time.UTC)
	out = gather(t, NewCollector(path, db, logger))
	for _, want := range []string{"gravel_backup_last_success_timestamp_seconds " + unix(finished), "gravel_backup_last_run_timestamp_seconds " + unix(finished), "gravel_backup_last_run_ok 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("want %q in:\n%s", want, out)
		}
	}

	// A failed run keeps last_success absent and says ok 0.
	if err := os.WriteFile(path, []byte(`{"ok":false,"started_at":"2026-10-10T02:00:00Z","finished_at":"2026-10-10T02:00:05Z","error":"backup-push failed","backup":""}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out = gather(t, NewCollector(path, db, logger))
	if !strings.Contains(out, "gravel_backup_last_run_ok 0") || strings.Contains(out, "gravel_backup_last_success") {
		t.Errorf("failed run:\n%s", out)
	}

	// An unreadable file and a database that does not answer are both visible.
	if err := os.WriteFile(path, []byte(`{`), 0o644); err != nil {
		t.Fatal(err)
	}
	out = gather(t, NewCollector(path, fakeArchiver{err: errors.New("down")}, logger))
	if !strings.Contains(out, "gravel_backup_status_readable 0") || !strings.Contains(out, "gravel_wal_archiver_readable 0") || strings.Contains(out, "gravel_wal_archived_total") {
		t.Errorf("failure modes:\n%s", out)
	}

	// No status file configured: only the archiver.
	out = gather(t, NewCollector("", db, logger))
	if strings.Contains(out, "gravel_backup_status_readable") || !strings.Contains(out, "gravel_wal_archived_total 12") {
		t.Errorf("no status file configured:\n%s", out)
	}
}
