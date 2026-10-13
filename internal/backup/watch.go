package backup

import (
	"context"
	"time"

	"github.com/gravel-project/gravel/internal/store"
)

// StaleAfter is how old the last good base backup may be before the events log says so.
const StaleAfter = 26 * time.Hour

// Watcher turns the backup status file into events (ADR-0028): backup.failed when the last run
// failed, backup.stale when the last good one is older than StaleAfter, backup.recovered when a
// run succeeds after either. Each event is keyed by its state and the run it is about, so a
// restarted hub never writes one twice.
type Watcher struct {
	path   string
	base   func(context.Context) (store.Event, error) // an event with the organization filled in
	append func(context.Context, store.Event) error
	last   string
}

// NewWatcher watches path; base gives the organization's id on an empty event; append writes one.
func NewWatcher(path string, base func(context.Context) (store.Event, error), append func(context.Context, store.Event) error) *Watcher {
	return &Watcher{path: path, base: base, append: append}
}

// Check reads the status and writes an event when the state changed since the last check (or,
// on the first check, when it is bad).
func (w *Watcher) Check(ctx context.Context, now time.Time) error {
	st, ok, err := ReadStatus(w.path)
	if err != nil || !ok {
		return err
	}
	state := "ok"
	switch {
	case !st.OK:
		state = "failed"
	case now.Sub(st.FinishedAt) > StaleAfter:
		state = "stale"
	}
	prev := w.last
	w.last = state
	if state == prev || (state == "ok" && prev == "") {
		return nil
	}
	e, err := w.base(ctx)
	if err != nil {
		w.last = prev
		return err
	}
	e.At, e.Source, e.SourceID = now, "backup", state+":"+st.FinishedAt.UTC().Format(time.RFC3339)
	switch state {
	case "failed":
		e.Kind, e.Summary, e.Detail = "backup.failed", "the base backup of "+st.FinishedAt.UTC().Format("2006-01-02 15:04 UTC")+" failed", map[string]any{"error": st.Error}
	case "stale":
		e.Kind, e.Summary = "backup.stale", "no base backup has succeeded since "+st.FinishedAt.UTC().Format("2006-01-02 15:04 UTC")
	default:
		e.Kind, e.Summary, e.Detail = "backup.recovered", "a base backup succeeded: "+st.Backup, map[string]any{"backup": st.Backup}
	}
	if err := w.append(ctx, e); err != nil {
		w.last = prev
		return err
	}
	return nil
}
