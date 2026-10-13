package servers

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/gravel-project/gravel/drivers/cloudvm"
	"github.com/gravel-project/gravel/internal/store"
)

// A server's health in the fleet (gravel#190): what an owner needs to know at a glance, from the
// monitor's observation, its host (a cloud-VM server's) and its lease.
const (
	HealthUp       = "up"       // answering, fresh, quick
	HealthDegraded = "degraded" // answering, but something is off: a missed poll, stale, slow, starting
	HealthDown     = "down"     // not answering, refusing the credential, or its host keeps failing
	HealthIdle     = "idle"     // parked or absent, as asked: nobody expects it to answer
	HealthUnknown  = "unknown"  // watched too briefly to say
)

// The health rule's thresholds.
const (
	// StaleIntervals is how many poll intervals without a good poll make a server degraded, and
	// how many without any good poll since it was first watched make it down.
	StaleIntervals = 3
	// SlowPoll is how long a poll may take before the server counts as degraded.
	SlowPoll = 2 * time.Second
	// HostGrace is how long a host that should be running may be in transition, or keep failing a
	// step, before its server counts as down.
	HostGrace = 10 * time.Minute
)

// HostHealth is what the health rule reads of a cloud-VM server's host.
type HostHealth struct {
	Desired, State, ErrorCode string
	StateSince                time.Time
	// ErrorSince is when the hub first saw the host's current error code.
	ErrorSince time.Time
}

// Verdict is a server's health, one line saying why, and since when that holds (zero when not
// known).
type Verdict struct {
	Health, Reason string
	Since          time.Time
}

// Health judges a server: o is the monitor's observation, host its host (nil for a server whose
// host gravel doesn't make) and lease its live lease's state ("" for none).
func Health(o Observation, host *HostHealth, lease string, now time.Time) Verdict {
	if host != nil {
		asked := host.Desired == cloudvm.DesiredParked || host.Desired == cloudvm.DesiredAbsent
		switch {
		case asked && (host.State == cloudvm.StateParked || host.State == cloudvm.StateAbsent):
			return Verdict{HealthIdle, host.State + ", as asked", host.StateSince}
		case asked:
			return Verdict{HealthIdle, "going " + host.Desired + " (" + host.State + ")", host.StateSince}
		case host.ErrorCode != "" && !host.ErrorSince.IsZero() && now.Sub(host.ErrorSince) >= HostGrace:
			return Verdict{HealthDown, "the host keeps failing: " + host.ErrorCode, host.ErrorSince}
		case host.State != cloudvm.StateRunning && now.Sub(host.StateSince) >= HostGrace:
			return Verdict{HealthDown, "the host should be running but is " + host.State, host.StateSince}
		case host.State != cloudvm.StateRunning:
			return Verdict{HealthDegraded, "the host is " + host.State, host.StateSince}
		}
	}
	interval := max(o.PollInterval, time.Second)
	stale := StaleIntervals * interval
	switch o.State {
	case StateCredentialRefused:
		return Verdict{HealthDown, "the server refuses the hub's credential", o.FailingSince}
	case StateCredentialMissing:
		return Verdict{HealthDown, "the credential file is missing or empty", o.FailingSince}
	case StateParked:
		return Verdict{HealthIdle, "parked", o.StateSince}
	case StateProvisioning:
		return Verdict{HealthDegraded, "the host is starting", o.StateSince}
	case StateUnreachable, StateError:
		what := "not answering"
		if o.State == StateError {
			what = "failing its polls"
		}
		if !o.Reachable || o.Misses >= MissesUnreachable {
			return Verdict{HealthDown, what, o.FailingSince}
		}
		return Verdict{HealthDegraded, "a poll failed", o.FailingSince}
	}
	if o.ObservedAt.IsZero() {
		if !o.WatchedSince.IsZero() && now.Sub(o.WatchedSince) >= stale {
			return Verdict{HealthDown, "never answered", o.WatchedSince.Add(stale)}
		}
		return Verdict{HealthUnknown, "not polled yet", o.WatchedSince}
	}
	switch {
	case o.State == StateRateLimited:
		return Verdict{HealthDegraded, "the server asked the hub to poll less", o.StateSince}
	case now.Sub(o.ObservedAt) > stale:
		return Verdict{HealthDegraded, fmt.Sprintf("last answered %s ago", now.Sub(o.ObservedAt).Round(time.Second)), o.ObservedAt}
	case lease == store.LeaseStarting:
		return Verdict{HealthDegraded, "a lease is getting it ready", o.StateSince}
	case o.LastPollDuration > SlowPoll:
		return Verdict{HealthDegraded, fmt.Sprintf("slow to answer (%s)", o.LastPollDuration.Round(10*time.Millisecond)), o.ObservedAt}
	}
	return Verdict{HealthUp, "answering", o.StateSince}
}

// healthRank orders the fleet trouble first: down, degraded, unknown, up, idle.
func healthRank(h string) int {
	return map[string]int{HealthDown: 0, HealthDegraded: 1, HealthUnknown: 2, HealthUp: 3, HealthIdle: 4}[h]
}

// FleetServer is one server in the fleet's snapshot.
type FleetServer struct {
	Server      Server
	GameName    string
	Observation Observation
	// Host is the server's host, nil for one whose host gravel doesn't make (or hasn't yet).
	Host       *HostHealth
	LeaseState string
	Verdict
}

// Fleet is every live server with its health: what the fleet summary, the alarms and the health
// gauge read. Snapshots are taken on demand; the only thing it remembers is when it first saw each
// host's current error code, so a failing step turns its server down after HostGrace.
type Fleet struct {
	svc    *Service
	mon    *Monitor
	hosts  HostSource
	leases LeaseSource
	now    func() time.Time

	mu         sync.Mutex
	errorSince map[string]hostError
}

type hostError struct {
	code  string
	since time.Time
}

// HostSource is the hosts gravel makes, as the cloud-VM manager holds them.
type HostSource interface {
	Host(serverID string) (cloudvm.Host, bool)
}

// LeaseSource is a server's live lease state ("starting", "ready", "ending").
type LeaseSource interface {
	LiveLeaseState(serverID string) (string, bool)
}

// NewFleet reads the servers from svc, their observations from mon, hosts from hosts and leases
// from leases; hosts and leases may be nil.
func NewFleet(svc *Service, mon *Monitor, hosts HostSource, leases LeaseSource) *Fleet {
	return &Fleet{svc: svc, mon: mon, hosts: hosts, leases: leases, now: time.Now, errorSince: map[string]hostError{}}
}

// WithClock sets the fleet's clock (tests).
func (f *Fleet) WithClock(now func() time.Time) *Fleet {
	f.now = now
	return f
}

// Snapshot is every live server with its health, trouble first (down, degraded, unknown, up,
// idle), then by players, most first, then by name.
func (f *Fleet) Snapshot(ctx context.Context) ([]FleetServer, error) {
	srvs, err := f.svc.Servers(ctx)
	if err != nil {
		return nil, err
	}
	games, err := f.svc.Games(ctx)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, g := range games {
		names[g.ID] = g.Name
	}
	now := f.now()
	out := make([]FleetServer, 0, len(srvs))
	seen := map[string]bool{}
	for _, srv := range srvs {
		seen[srv.ID] = true
		fs := FleetServer{Server: srv, GameName: cmp.Or(names[srv.Game], srv.Game)}
		if o, ok := f.mon.Observation(srv.ID); ok {
			fs.Observation = o
		} else {
			fs.Observation = Observation{ServerID: srv.ID, State: StateUnknown}
		}
		if f.hosts != nil {
			if h, ok := f.hosts.Host(srv.ID); ok {
				fs.Host = &HostHealth{Desired: h.Desired, State: h.State, ErrorCode: h.ErrorCode, StateSince: h.StateSince, ErrorSince: f.errorSeen(srv.ID, h.ErrorCode, now)}
			}
		}
		if f.leases != nil {
			fs.LeaseState, _ = f.leases.LiveLeaseState(srv.ID)
		}
		fs.Verdict = Health(fs.Observation, fs.Host, fs.LeaseState, now)
		out = append(out, fs)
	}
	f.mu.Lock()
	for id := range f.errorSince {
		if !seen[id] {
			delete(f.errorSince, id)
		}
	}
	f.mu.Unlock()
	slices.SortStableFunc(out, func(a, b FleetServer) int {
		return cmp.Or(cmp.Compare(healthRank(a.Health), healthRank(b.Health)),
			cmp.Compare(b.Observation.Status.Players, a.Observation.Status.Players),
			cmp.Compare(a.Server.Name, b.Server.Name), cmp.Compare(a.Server.ID, b.Server.ID))
	})
	return out, nil
}

// errorSeen is when the fleet first saw a host's current error code; zero for none.
func (f *Fleet) errorSeen(id, code string, now time.Time) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	if code == "" {
		delete(f.errorSince, id)
		return time.Time{}
	}
	e, ok := f.errorSince[id]
	if !ok || e.code != code {
		e = hostError{code: code, since: now}
		f.errorSince[id] = e
	}
	return e.since
}

// FleetTotals counts a snapshot: servers by health, and players and capacity over the servers
// that are not idle (a parked server's slots are not capacity).
type FleetTotals struct {
	Servers, Up, Degraded, Down, Idle, Unknown int
	Players, Capacity                          int
}

// Totals counts a snapshot.
func Totals(snap []FleetServer) FleetTotals {
	var t FleetTotals
	for _, s := range snap {
		t.Servers++
		switch s.Health {
		case HealthUp:
			t.Up++
		case HealthDegraded:
			t.Degraded++
		case HealthDown:
			t.Down++
		case HealthIdle:
			t.Idle++
		default:
			t.Unknown++
		}
		if s.Health == HealthIdle || s.Observation.ObservedAt.IsZero() {
			continue
		}
		t.Players += s.Observation.Status.Players
		t.Capacity += s.Observation.Status.MaxPlayers
	}
	return t
}

// Alarm is something wrong that needs a person, for as long as it lasts (gravel#190): computed
// from the fleet and the hub's own state on every read, never stored, so a removed server's alarm
// is gone with it.
type Alarm struct {
	Kind, Severity string
	// ServerID is the server it is about; "" for the hub itself.
	ServerID string
	Since    time.Time
	Detail   string
}

// The alarms about servers, and how long a server is down before it alarms.
const (
	AlarmServerDown        = "server.down"
	AlarmCredentialRefused = "server.credential_refused"
	AlarmCredentialMissing = "server.credential_missing"
	AlarmHostFailing       = "host.failing"
	DownAlarmAfter         = 2 * time.Minute
)

// An alarm's severity, as an event's (ADR-0028).
const (
	AlarmCritical = "critical"
	AlarmWarning  = "warning"
)

// ServerAlarms are a snapshot's alarms: a server down for DownAlarmAfter (but for a refused or
// missing credential, which alarm at once under their own kind), and a host failing a step.
func ServerAlarms(snap []FleetServer, now time.Time) []Alarm {
	var out []Alarm
	for _, s := range snap {
		o := s.Observation
		switch {
		case o.State == StateCredentialRefused:
			out = append(out, Alarm{Kind: AlarmCredentialRefused, Severity: AlarmCritical, ServerID: s.Server.ID, Since: o.FailingSince, Detail: s.Reason})
		case o.State == StateCredentialMissing:
			out = append(out, Alarm{Kind: AlarmCredentialMissing, Severity: AlarmCritical, ServerID: s.Server.ID, Since: o.FailingSince, Detail: s.Reason})
		case s.Health == HealthDown && !s.Since.IsZero() && now.Sub(s.Since) >= DownAlarmAfter:
			out = append(out, Alarm{Kind: AlarmServerDown, Severity: AlarmCritical, ServerID: s.Server.ID, Since: s.Since, Detail: s.Reason})
		}
		if s.Host != nil && s.Host.ErrorCode != "" {
			out = append(out, Alarm{Kind: AlarmHostFailing, Severity: AlarmWarning, ServerID: s.Server.ID, Since: s.Host.ErrorSince,
				Detail: "a step on the host keeps failing: " + s.Host.ErrorCode})
		}
	}
	return out
}
