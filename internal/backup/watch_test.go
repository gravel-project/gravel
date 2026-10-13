package backup_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gravel-project/gravel/internal/backup"
	"github.com/gravel-project/gravel/internal/store"
)

// The watcher writes an event when the backup state changes, keyed by state and run so a restart
// writes nothing twice, and says nothing about a healthy first look.
func TestBackupWatcher(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var got []store.Event
	w := backup.NewWatcher(path, func(context.Context) (store.Event, error) { return store.Event{}, nil },
		func(_ context.Context, e store.Event) error { got = append(got, e); return nil })
	ctx := context.Background()
	now := time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC)
	check := func(at time.Time) {
		t.Helper()
		if err := w.Check(ctx, at); err != nil {
			t.Fatal(err)
		}
	}
	check(now) // no file: no backup has run, nothing to say
	write(`{"ok": true, "finished_at": "2026-10-13T05:00:00Z", "backup": "base_1"}`)
	check(now)
	check(now.Add(time.Hour))
	if len(got) != 0 {
		t.Fatalf("a healthy first look: %+v", got)
	}
	check(now.Add(27 * time.Hour)) // stale
	write(`{"ok": false, "finished_at": "2026-10-14T09:00:00Z", "error": "s3: access denied"}`)
	check(now.Add(28 * time.Hour))
	check(now.Add(28*time.Hour + time.Minute))
	write(`{"ok": true, "finished_at": "2026-10-14T10:00:00Z", "backup": "base_2"}`)
	check(now.Add(29 * time.Hour))
	var kinds []string
	for _, e := range got {
		kinds = append(kinds, e.Kind+"@"+e.SourceID)
	}
	want := "backup.stale@stale:2026-10-13T05:00:00Z backup.failed@failed:2026-10-14T09:00:00Z backup.recovered@ok:2026-10-14T10:00:00Z"
	if strings.Join(kinds, " ") != want {
		t.Errorf("events:\n got %s\nwant %s", strings.Join(kinds, " "), want)
	}
	if got[1].Detail["error"] != "s3: access denied" {
		t.Errorf("failed detail: %+v", got[1])
	}
	// A new watcher (a restart) on a failed backup writes it again with the same key: the store's
	// (source, source_id) index makes it a no-op.
	write(`{"ok": false, "finished_at": "2026-10-14T09:00:00Z"}`)
	var again []store.Event
	w2 := backup.NewWatcher(path, func(context.Context) (store.Event, error) { return store.Event{}, nil },
		func(_ context.Context, e store.Event) error { again = append(again, e); return nil })
	if err := w2.Check(ctx, now.Add(30*time.Hour)); err != nil || len(again) != 1 || again[0].SourceID != got[1].SourceID {
		t.Errorf("after a restart: %+v %v", again, err)
	}
}
