package hub

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/gravel-project/gravel/internal/backup"
	"github.com/gravel-project/gravel/internal/notify"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/store"
)

// The hub's own alarms (gravel#190), beside the servers' (servers.ServerAlarms).
const (
	AlarmBackupStale  = "backup.stale"
	AlarmBackupFailed = "backup.failed"
	AlarmWALFailing   = "wal.failing"
	AlarmHubUnowned   = "hub.unowned"
	// AlarmNotifierFailing is a notifier that has failed for notify.FailingAlarmAfter: the hub
	// may be telling nobody what is wrong.
	AlarmNotifierFailing = "notifier.failing"
	// WALFailingAfter is how long WAL archiving may fail, with no segment archived since, before
	// it alarms. An idle database archives nothing and is not an alarm: only failures count.
	WALFailingAfter = 10 * time.Minute
	// FleetEvery is how often the fleet's health and the alarms are counted into their gauges.
	FleetEvery = 15 * time.Second
)

// alarmSource computes the alarms from the fleet and the hub's state on every read: nothing is
// stored, so an alarm lasts exactly as long as its cause.
type alarmSource struct {
	owned      func(ctx context.Context) (bool, time.Time, error)
	statusFile string
	archiver   backup.Archiver
	notifiers  interface {
		Failing(ctx context.Context, now time.Time) ([]notify.Failing, error)
	}
	now func() time.Time
}

// Alarms are the servers' alarms from snap and the hub's own: a stale or failed backup, WAL
// archiving failing, a hub nobody has claimed, and a notifier that keeps failing.
func (a *alarmSource) Alarms(ctx context.Context, snap []servers.FleetServer) ([]servers.Alarm, error) {
	now := a.now()
	out := servers.ServerAlarms(snap, now)
	if a.owned != nil {
		owned, since, err := a.owned(ctx)
		if err != nil {
			return nil, err
		}
		if !owned {
			out = append(out, servers.Alarm{Kind: AlarmHubUnowned, Severity: servers.AlarmWarning, Since: since,
				Detail: "nobody has claimed this hub: log in and claim it with the token it printed at start"})
		}
	}
	if a.statusFile != "" {
		st, found, err := backup.ReadStatus(a.statusFile)
		switch {
		case err != nil:
			out = append(out, servers.Alarm{Kind: AlarmBackupFailed, Severity: servers.AlarmCritical, Since: now, Detail: "the backup status file cannot be read"})
		case found && !st.OK:
			out = append(out, servers.Alarm{Kind: AlarmBackupFailed, Severity: servers.AlarmCritical, Since: st.FinishedAt, Detail: "the last base backup failed"})
		case found && now.Sub(st.FinishedAt) > backup.StaleAfter:
			out = append(out, servers.Alarm{Kind: AlarmBackupStale, Severity: servers.AlarmWarning, Since: st.FinishedAt.Add(backup.StaleAfter),
				Detail: "no base backup has succeeded since " + st.FinishedAt.UTC().Format("2006-01-02 15:04 UTC")})
		}
	}
	if a.archiver != nil {
		if w, ok, err := walFailing(ctx, a.archiver, now); err == nil && ok {
			out = append(out, w)
		}
	}
	if a.notifiers != nil {
		failing, err := a.notifiers.Failing(ctx, now)
		if err != nil {
			return nil, err
		}
		for _, f := range failing {
			out = append(out, servers.Alarm{Kind: AlarmNotifierFailing, Severity: servers.AlarmWarning, Since: f.Since,
				Detail: fmt.Sprintf("the notifier %q has failed since %s: %s", f.Name, f.Since.UTC().Format("2006-01-02 15:04 UTC"), f.LastError)})
		}
	}
	return out, nil
}

// walFailing is an alarm when the archiver's last attempt failed, nothing has been archived since,
// and that has lasted WALFailingAfter.
func walFailing(ctx context.Context, db backup.Archiver, now time.Time) (servers.Alarm, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	st, err := db.Archiver(ctx)
	if err != nil || st.LastFailedTime == nil {
		return servers.Alarm{}, false, err
	}
	since := time.Time{}
	if st.LastArchivedTime != nil {
		if !st.LastFailedTime.After(*st.LastArchivedTime) {
			return servers.Alarm{}, false, nil
		}
		since = *st.LastArchivedTime
	}
	if !since.IsZero() && now.Sub(since) < WALFailingAfter {
		return servers.Alarm{}, false, nil
	}
	if since.IsZero() {
		since = *st.LastFailedTime
	}
	return servers.Alarm{Kind: AlarmWALFailing, Severity: servers.AlarmCritical, Since: since,
		Detail: fmt.Sprintf("WAL archiving is failing (%d failures); point-in-time recovery stops at the last archived segment", st.FailedCount)}, true, nil
}

// fleetGauges counts the fleet by health and the alarms by kind, for gravel_server_health and
// gravel_alarms.
type fleetGauges struct {
	health *prometheus.GaugeVec
	alarms *prometheus.GaugeVec
}

func newFleetGauges(reg prometheus.Registerer) *fleetGauges {
	g := &fleetGauges{
		health: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gravel_server_health", Help: "Servers by health: up, degraded, down, idle or unknown (docs/hub.md, Fleet health).",
		}, []string{"health"}),
		alarms: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gravel_alarms", Help: "Alarms open now, by kind: something that needs a person (docs/hub.md, Alarms).",
		}, []string{"kind"}),
	}
	reg.MustRegister(g.health, g.alarms)
	return g
}

func (g *fleetGauges) set(snap []servers.FleetServer, alarms []servers.Alarm) {
	t := servers.Totals(snap)
	for h, n := range map[string]int{servers.HealthUp: t.Up, servers.HealthDegraded: t.Degraded, servers.HealthDown: t.Down,
		servers.HealthIdle: t.Idle, servers.HealthUnknown: t.Unknown} {
		g.health.WithLabelValues(h).Set(float64(n))
	}
	for _, k := range alarmKinds {
		g.alarms.WithLabelValues(k).Set(0)
	}
	for _, a := range alarms {
		g.alarms.WithLabelValues(a.Kind).Inc()
	}
}

// alarmKinds are every alarm, so each has a series at 0 while it is not open.
var alarmKinds = []string{servers.AlarmServerDown, servers.AlarmCredentialRefused, servers.AlarmCredentialMissing, servers.AlarmHostFailing,
	AlarmBackupStale, AlarmBackupFailed, AlarmWALFailing, AlarmHubUnowned, AlarmNotifierFailing}

// healthEvents writes the monitor's server health events into the events log.
type healthEvents struct {
	st    *store.Store
	orgID uuid.UUID
}

func (h healthEvents) AppendHealthEvent(ctx context.Context, e servers.HealthEvent) error {
	return h.st.AppendEvent(ctx, store.Event{OrganizationID: h.orgID, At: e.At, Kind: e.Kind, ServerID: e.ServerID, Summary: e.Summary,
		Detail: e.Detail, Source: "server_health", SourceID: fmt.Sprintf("%s:%s:%d", e.ServerID, e.Kind, e.At.UnixMicro())})
}

func (h healthEvents) LastHealthEvent(ctx context.Context, serverID string) (string, error) {
	evs, _, err := h.st.ListEvents(ctx, h.orgID, store.EventQuery{Limit: 1, ServerID: serverID,
		Kinds: []string{servers.EventUnreachable, servers.EventRecovered, servers.EventCredentialRefused, servers.EventCredentialMissing}})
	if err != nil || len(evs) == 0 {
		return "", err
	}
	return evs[0].Kind, nil
}
