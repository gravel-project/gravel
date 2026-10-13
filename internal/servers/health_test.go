package servers

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/drivers/cloudvm"
	"github.com/gravel-project/gravel/internal/servers/serverstest"
	"github.com/gravel-project/gravel/internal/store"
)

var healthNow = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

// fresh is an observation polled a moment ago, every 30 s.
func fresh() Observation {
	return Observation{ServerID: "s", State: StateOK, Reachable: true, ObservedAt: healthNow.Add(-10 * time.Second), PollInterval: 30 * time.Second,
		StateSince: healthNow.Add(-time.Hour), WatchedSince: healthNow.Add(-2 * time.Hour), LastPollDuration: 100 * time.Millisecond}
}

func with(f func(o *Observation)) Observation {
	o := fresh()
	f(&o)
	return o
}

func host(desired, state, code string, since, errSince time.Duration) *HostHealth {
	h := &HostHealth{Desired: desired, State: state, ErrorCode: code, StateSince: healthNow.Add(-since)}
	if code != "" {
		h.ErrorSince = healthNow.Add(-errSince)
	}
	return h
}

// The rule over every observation state, host state, staleness and miss count that decides it.
func TestHealth(t *testing.T) {
	for _, c := range []struct {
		name  string
		o     Observation
		host  *HostHealth
		lease string
		want  string
	}{
		{"answering", fresh(), nil, "", HealthUp},
		{"one miss", with(func(o *Observation) { o.State, o.Misses = StateUnreachable, 1 }), nil, "", HealthDegraded},
		{"two misses", with(func(o *Observation) { o.State, o.Misses, o.Reachable = StateUnreachable, 2, false }), nil, "", HealthDown},
		{"an error, once", with(func(o *Observation) { o.State, o.Misses = StateError, 1 }), nil, "", HealthDegraded},
		{"an error, twice", with(func(o *Observation) { o.State, o.Misses, o.Reachable = StateError, 2, false }), nil, "", HealthDown},
		{"credential refused", with(func(o *Observation) { o.State, o.Misses = StateCredentialRefused, 1 }), nil, "", HealthDown},
		{"credential missing", with(func(o *Observation) { o.State = StateCredentialMissing }), nil, "", HealthDown},
		{"rate limited", with(func(o *Observation) { o.State = StateRateLimited }), nil, "", HealthDegraded},
		{"stale", with(func(o *Observation) { o.ObservedAt = healthNow.Add(-91 * time.Second) }), nil, "", HealthDegraded},
		{"three intervals is not stale yet", with(func(o *Observation) { o.ObservedAt = healthNow.Add(-90 * time.Second) }), nil, "", HealthUp},
		{"slow", with(func(o *Observation) { o.LastPollDuration = 2500 * time.Millisecond }), nil, "", HealthDegraded},
		{"a lease starting", fresh(), nil, store.LeaseStarting, HealthDegraded},
		{"a lease ready", fresh(), nil, store.LeaseReady, HealthUp},
		{"not polled yet", Observation{State: StateUnknown, PollInterval: 30 * time.Second, WatchedSince: healthNow.Add(-time.Minute)}, nil, "", HealthUnknown},
		{"never answered", Observation{State: StateUnknown, PollInterval: 30 * time.Second, WatchedSince: healthNow.Add(-91 * time.Second)}, nil, "", HealthDown},
		{"parked, no host known", with(func(o *Observation) { o.State = StateParked }), nil, "", HealthIdle},
		{"provisioning, no host known", with(func(o *Observation) { o.State = StateProvisioning }), nil, "", HealthDegraded},
		{"host running", fresh(), host(cloudvm.DesiredRunning, cloudvm.StateRunning, "", time.Hour, 0), "", HealthUp},
		{"host running, poll failing", with(func(o *Observation) { o.State, o.Misses, o.Reachable = StateUnreachable, 2, false }),
			host(cloudvm.DesiredRunning, cloudvm.StateRunning, "", time.Hour, 0), "", HealthDown},
		{"parked as asked", with(func(o *Observation) { o.State = StateParked }), host(cloudvm.DesiredParked, cloudvm.StateParked, "", time.Hour, 0), "", HealthIdle},
		{"absent as asked", with(func(o *Observation) { o.State = StateParked }), host(cloudvm.DesiredAbsent, cloudvm.StateAbsent, "", time.Hour, 0), "", HealthIdle},
		{"parking as asked", with(func(o *Observation) { o.State = StateProvisioning }), host(cloudvm.DesiredParked, cloudvm.StateParking, "", time.Minute, 0), "", HealthIdle},
		{"booting", with(func(o *Observation) { o.State = StateProvisioning }), host(cloudvm.DesiredRunning, cloudvm.StateBooting, "", time.Minute, 0), "", HealthDegraded},
		{"parked, should run, in grace", with(func(o *Observation) { o.State = StateParked }), host(cloudvm.DesiredRunning, cloudvm.StateParked, "", 9*time.Minute, 0), "", HealthDegraded},
		{"parked, should run, past grace", with(func(o *Observation) { o.State = StateParked }), host(cloudvm.DesiredRunning, cloudvm.StateParked, "", 10*time.Minute, 0), "", HealthDown},
		{"host error, fresh", with(func(o *Observation) { o.State = StateProvisioning }), host(cloudvm.DesiredRunning, cloudvm.StateBooting, "ssh_failed", time.Minute, 5*time.Minute), "", HealthDegraded},
		{"host error, lasting", fresh(), host(cloudvm.DesiredRunning, cloudvm.StateRunning, "push_failed", time.Hour, 10*time.Minute), "", HealthDown},
		{"host error while parking as asked", with(func(o *Observation) { o.State = StateProvisioning }),
			host(cloudvm.DesiredParked, cloudvm.StateParking, "snapshot_failed", time.Hour, time.Hour), "", HealthIdle},
	} {
		got := Health(c.o, c.host, c.lease, healthNow)
		if got.Health != c.want || got.Reason == "" {
			t.Errorf("%s: %+v, want %s", c.name, got, c.want)
		}
	}
}

// A run of failures is dated from its first: the down alarm's two minutes count from there.
func TestHealthSince(t *testing.T) {
	o := with(func(o *Observation) {
		o.State, o.Misses, o.Reachable, o.FailingSince = StateUnreachable, 4, false, healthNow.Add(-3*time.Minute)
	})
	if v := Health(o, nil, "", healthNow); !v.Since.Equal(healthNow.Add(-3 * time.Minute)) {
		t.Errorf("since: %+v", v)
	}
	if a := ServerAlarms([]FleetServer{{Server: Server{ID: "s"}, Observation: o, Verdict: Health(o, nil, "", healthNow)}}, healthNow); len(a) != 1 || a[0].Kind != AlarmServerDown {
		t.Errorf("down three minutes: %+v", a)
	}
	o.FailingSince = healthNow.Add(-time.Minute)
	if a := ServerAlarms([]FleetServer{{Server: Server{ID: "s"}, Observation: o, Verdict: Health(o, nil, "", healthNow)}}, healthNow); len(a) != 0 {
		t.Errorf("down one minute alarmed: %+v", a)
	}
	// A refused credential alarms at once, under its own kind and no other.
	o = with(func(o *Observation) { o.State, o.FailingSince = StateCredentialRefused, healthNow })
	if a := ServerAlarms([]FleetServer{{Server: Server{ID: "s"}, Observation: o, Verdict: Health(o, nil, "", healthNow)}}, healthNow); len(a) != 1 || a[0].Kind != AlarmCredentialRefused {
		t.Errorf("refused: %+v", a)
	}
}

type fakeHosts map[string]cloudvm.Host

func (f fakeHosts) Host(id string) (cloudvm.Host, bool) { h, ok := f[id]; return h, ok }

type fakeLeases map[string]string

func (f fakeLeases) LiveLeaseState(id string) (string, bool) { s, ok := f[id]; return s, ok }

// The snapshot: trouble first, then players; idle servers' slots are no capacity; a host's error
// counts from when the fleet first saw it; a removed server takes its alarms with it.
func TestFleetSnapshot(t *testing.T) {
	ctx := context.Background()
	svc := New(serverstest.NewFakeStore(), uuid.New(), []string{"wardogs", "cloudvm"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m := Manifest{Games: []GameRef{{ID: "wardogs"}}}
	for i, name := range []string{"Busy", "Quiet", "Broken", "Parked"} {
		m.Servers = append(m.Servers, Server{ID: fmt.Sprintf("wd-%d", i), Name: name, Game: "wardogs", Driver: "wardogs", Location: "slc",
			Endpoint: "http://127.0.0.1:1", CredentialFile: "/dev/null", Trust: TrustOfficial})
	}
	if _, err := svc.Apply(ctx, m.Normalized(), ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	mon := &Monitor{obs: map[string]Observation{}, workers: map[string]*worker{}}
	set := func(id string, o Observation) { o.ServerID = id; mon.obs[id] = o }
	busy, quiet := fresh(), fresh()
	busy.Status.Players, busy.Status.MaxPlayers, quiet.Status.Players, quiet.Status.MaxPlayers = 30, 80, 2, 80
	set("wd-0", busy)
	set("wd-1", quiet)
	set("wd-2", with(func(o *Observation) {
		o.State, o.Misses, o.Reachable, o.FailingSince = StateUnreachable, 5, false, healthNow.Add(-time.Hour)
	}))
	parked := with(func(o *Observation) { o.State = StateParked })
	parked.Status.MaxPlayers = 10
	set("wd-3", parked)
	hosts := fakeHosts{"wd-3": {ServerID: "wd-3", Intent: cloudvm.Intent{Desired: cloudvm.DesiredParked}, State: cloudvm.StateParked, StateSince: healthNow.Add(-time.Hour)}}
	now := healthNow
	f := NewFleet(svc, mon, hosts, fakeLeases{}).WithClock(func() time.Time { return now })
	snap, err := f.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, s := range snap {
		order = append(order, s.Server.Name+":"+s.Health)
	}
	if fmt.Sprint(order) != "[Broken:down Busy:up Quiet:up Parked:idle]" {
		t.Errorf("order: %v", order)
	}
	if tot := Totals(snap); tot.Servers != 4 || tot.Up != 2 || tot.Down != 1 || tot.Idle != 1 || tot.Players != 32 || tot.Capacity != 160 {
		t.Errorf("totals: %+v", tot)
	}

	// The parked server should be running and its host keeps failing: down after the grace, from
	// when the fleet first saw the error.
	hosts["wd-3"] = cloudvm.Host{ServerID: "wd-3", Intent: cloudvm.Intent{Desired: cloudvm.DesiredRunning}, State: cloudvm.StateBooting, StateSince: now, ErrorCode: "ssh_failed"}
	health := func() string {
		snap, err := f.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range snap {
			if s.Server.ID == "wd-3" {
				return s.Health
			}
		}
		return "gone"
	}
	if h := health(); h != HealthDegraded {
		t.Errorf("a fresh host error: %s", h)
	}
	now = now.Add(HostGrace)
	if h := health(); h != HealthDown {
		t.Errorf("a host error past the grace: %s", h)
	}
	snap, _ = f.Snapshot(ctx)
	if a := ServerAlarms(snap, now); len(a) != 3 { // wd-2 down, wd-3 down (from the host), wd-3's host failing
		t.Errorf("alarms: %+v", a)
	}

	// Removed: gone from the snapshot, and so from the alarms.
	m.Servers = m.Servers[:2]
	if _, err := svc.Apply(ctx, m.Normalized(), ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	snap, _ = f.Snapshot(ctx)
	if a := ServerAlarms(snap, now); len(snap) != 2 || len(a) != 0 {
		t.Errorf("after removing the broken servers: %d servers, alarms %+v", len(snap), a)
	}
	if len(f.errorSince) != 0 {
		t.Errorf("a removed host's error is remembered: %v", f.errorSince)
	}
}
