package hub

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/gravel-project/gravel/internal/backup"
	"github.com/gravel-project/gravel/internal/notify"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

type fakeArchiver struct{ st store.ArchiverStatus }

func (f fakeArchiver) Archiver(context.Context) (store.ArchiverStatus, error) { return f.st, nil }

func kinds(t *testing.T, a *alarmSource) []string {
	t.Helper()
	alarms, err := a.Alarms(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, x := range alarms {
		out = append(out, x.Kind)
	}
	return out
}

// The hub's own alarms (gravel#190): unclaimed, a failed or stale backup, WAL archiving failing;
// an idle archive is not one.
func TestHubAlarms(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { x := now.Add(-d); return &x }
	status := filepath.Join(t.TempDir(), "status.json")
	writeStatus := func(s backup.Status) {
		b, _ := json.Marshal(s)
		if err := os.WriteFile(status, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	owned := true
	a := &alarmSource{statusFile: status, now: func() time.Time { return now },
		owned: func(context.Context) (bool, time.Time, error) { return owned, now.Add(-time.Hour), nil }}

	if got := kinds(t, a); len(got) != 0 {
		t.Errorf("a claimed hub with no backup yet: %v", got)
	}
	owned = false
	if got := kinds(t, a); len(got) != 1 || got[0] != AlarmHubUnowned {
		t.Errorf("unclaimed: %v", got)
	}
	owned = true
	writeStatus(backup.Status{OK: true, FinishedAt: now.Add(-time.Hour)})
	if got := kinds(t, a); len(got) != 0 {
		t.Errorf("a fresh backup: %v", got)
	}
	writeStatus(backup.Status{OK: true, FinishedAt: now.Add(-27 * time.Hour)})
	if got := kinds(t, a); len(got) != 1 || got[0] != AlarmBackupStale {
		t.Errorf("a stale backup: %v", got)
	}
	writeStatus(backup.Status{OK: false, FinishedAt: now.Add(-time.Hour)})
	if got := kinds(t, a); len(got) != 1 || got[0] != AlarmBackupFailed {
		t.Errorf("a failed backup: %v", got)
	}
	writeStatus(backup.Status{OK: true, FinishedAt: now.Add(-time.Hour)})

	for _, c := range []struct {
		name string
		st   store.ArchiverStatus
		want bool
	}{
		{"never archived, never failed (archiving off)", store.ArchiverStatus{}, false},
		{"idle: archived an hour ago, never failed", store.ArchiverStatus{LastArchivedTime: ago(time.Hour)}, false},
		{"a failure, then archived again", store.ArchiverStatus{LastFailedTime: ago(2 * time.Hour), LastArchivedTime: ago(time.Hour)}, false},
		{"failing for five minutes", store.ArchiverStatus{LastArchivedTime: ago(5 * time.Minute), LastFailedTime: ago(time.Minute), FailedCount: 3}, false},
		{"failing for eleven minutes", store.ArchiverStatus{LastArchivedTime: ago(11 * time.Minute), LastFailedTime: ago(time.Minute), FailedCount: 9}, true},
		{"failing, never archived", store.ArchiverStatus{LastFailedTime: ago(time.Minute), FailedCount: 1}, true},
	} {
		a.archiver = fakeArchiver{c.st}
		got := kinds(t, a)
		if (len(got) == 1 && got[0] == AlarmWALFailing) != c.want || len(got) > 1 {
			t.Errorf("%s: %v", c.name, got)
		}
	}
	a.archiver = nil

	// A notifier failing for an hour (gravel#196).
	f := &fakeNotifiers{}
	a.notifiers = f
	if got := kinds(t, a); len(got) != 0 {
		t.Errorf("no failing notifier: %v", got)
	}
	f.failing = []notify.Failing{{Name: "john-phone", Since: now.Add(-2 * time.Hour), LastError: "answered 401 Unauthorized"}}
	all, err := a.Alarms(context.Background(), nil)
	if err != nil || len(all) != 1 || all[0].Kind != AlarmNotifierFailing || all[0].Severity != servers.AlarmWarning || !all[0].Since.Equal(now.Add(-2*time.Hour)) ||
		all[0].Detail != `the notifier "john-phone" has failed since 2026-10-13 10:00 UTC: answered 401 Unauthorized` {
		t.Errorf("a failing notifier: %+v %v", all, err)
	}
}

type fakeNotifiers struct{ failing []notify.Failing }

func (f *fakeNotifiers) Failing(context.Context, time.Time) ([]notify.Failing, error) {
	return f.failing, nil
}

// The gauges count the fleet by health and every alarm kind, at 0 while it is not open.
func TestFleetGauges(t *testing.T) {
	reg := newTestRegistry()
	g := newFleetGauges(reg)
	g.set([]servers.FleetServer{{Verdict: servers.Verdict{Health: servers.HealthDown}}, {Verdict: servers.Verdict{Health: servers.HealthUp}}},
		[]servers.Alarm{{Kind: servers.AlarmServerDown}})
	if v := gaugeValue(t, g.health, servers.HealthDown); v != 1 {
		t.Errorf("down: %v", v)
	}
	if v := gaugeValue(t, g.alarms, servers.AlarmServerDown); v != 1 {
		t.Errorf("server.down alarms: %v", v)
	}
	if v := gaugeValue(t, g.alarms, AlarmHubUnowned); v != 0 {
		t.Errorf("hub.unowned: %v", v)
	}
	g.set(nil, nil)
	if v := gaugeValue(t, g.alarms, servers.AlarmServerDown); v != 0 {
		t.Errorf("a closed alarm still counts: %v", v)
	}
}

func newTestRegistry() *prometheus.Registry { return prometheus.NewRegistry() }

func gaugeValue(t *testing.T, g *prometheus.GaugeVec, label string) float64 {
	t.Helper()
	return testutil.ToFloat64(g.WithLabelValues(label))
}

// The monitor's health events land in the events log, keyed so a replay writes nothing twice, and
// the last one is read back for a restarted worker (Postgres).
func TestHealthEventsInTheLog(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o, err := st.CreateBuiltinOrganization(ctx, uuid.New(), "HTG")
	if err != nil {
		t.Fatal(err)
	}
	h := healthEvents{st: st, orgID: o.ID}
	if last, err := h.LastHealthEvent(ctx, "wd-1"); err != nil || last != "" {
		t.Fatalf("an empty log: %q %v", last, err)
	}
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	down := servers.HealthEvent{ServerID: "wd-1", Kind: servers.EventUnreachable, At: at, Summary: "War Dogs #1 stopped answering its polls"}
	for range 2 {
		if err := h.AppendHealthEvent(ctx, down); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.AppendHealthEvent(ctx, servers.HealthEvent{ServerID: "wd-2", Kind: servers.EventCredentialRefused, At: at.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if last, _ := h.LastHealthEvent(ctx, "wd-1"); last != servers.EventUnreachable {
		t.Errorf("last for wd-1: %q", last)
	}
	if err := h.AppendHealthEvent(ctx, servers.HealthEvent{ServerID: "wd-1", Kind: servers.EventRecovered, At: at.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if last, _ := h.LastHealthEvent(ctx, "wd-1"); last != servers.EventRecovered {
		t.Errorf("last after the recovery: %q", last)
	}
	evs, _, err := st.ListEvents(ctx, o.ID, store.EventQuery{Limit: 10, ServerID: "wd-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[1].Kind != servers.EventUnreachable || evs[1].Severity != "warning" || evs[1].ServerID != "wd-1" {
		t.Errorf("wd-1's events: %+v", evs)
	}
}
