package servers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/gravel-project/gravel/drivers/cloudvm"
	"github.com/gravel-project/gravel/internal/servers/serverstest"
	"github.com/gravel-project/gravel/internal/store"
)

type leaseRig struct {
	*hostsRig
	leases *Leases
	lrows  *serverstest.LeaseRows
	now    time.Time
	ctx    context.Context
	app    Actor
}

const leaseKey = "0123456789abcdef0123456789abcdef"

// newLeaseRig is a running CS2 server in the pool "pug", leasable with start_map, its game
// offering the plugin set "matchzy" with a match file.
func newLeaseRig(t *testing.T) *leaseRig {
	t.Helper()
	r := &leaseRig{now: time.Date(2026, 10, 14, 1, 0, 0, 0, time.UTC), ctx: context.Background(), lrows: serverstest.NewLeaseRows()}
	clock := func() time.Time { return r.now }
	r.hostsRig = newHostsRigWith(t, func(m *Manifest) {
		m.Servers[0].VM.Lease = &LeaseSpec{Pool: "pug", Settings: []string{"start_map"}}
	}, clock)
	spec := r.svc.specs["cs2"]
	vm := *spec.CloudVM
	vm.Lease = &cloudvm.LeaseProfile{MatchFile: "game/csgo/gravel-match.json", MatchFormat: "test", LoadCommand: "matchzy_loadmatch gravel-match.json",
		Connect: "connect <address>; password <password>",
		Plugins: map[string]cloudvm.PluginSet{"matchzy": {Check: "css_plugins list", Expect: "MatchZy",
			Files: []cloudvm.PluginFile{{URL: "https://example.com/mz.zip", SHA256: strings.Repeat("a", 64), Kind: "zip", Dest: "."}},
			Env:   map[string]string{"GRAVEL_MATCHZY_EVENTS_URL": "{hub}/api/ingest/matchzy", "GRAVEL_MATCHZY_TOKEN": "{token}"}}}}
	spec.CloudVM = &vm
	r.svc.specs["cs2"] = spec
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r.leases = NewLeases(r.svc, r.hosts, r.lrows, r.audit, r.svc.orgID, []byte(leaseKey), prometheus.NewRegistry(), logger).WithClock(clock)
	r.leases.SetMatchCheck("test", func(b []byte) error {
		if !strings.HasPrefix(string(b), "{") {
			return errors.New("not JSON")
		}
		return nil
	})
	app := uuid.New()
	r.app = Actor{AppID: &app}
	r.up()
	return r
}

// up runs the server: the Manager reads the specs and ticks until it is running.
func (r *leaseRig) up() {
	for range 6 {
		if err := r.mgr.Reconcile(r.ctx); err != nil {
			panic(err)
		}
		_, _ = r.mgr.Status(r.ctx, "htg-cs2-1") // who is on, before the tick decides anything
		_ = r.mgr.Tick(r.ctx, "htg-cs2-1")
		if h, _ := r.mgr.Host("htg-cs2-1"); h.State == cloudvm.StateRunning && h.Step == "" && r.mgr.EnvCurrent("htg-cs2-1") {
			_, _ = r.mgr.Status(r.ctx, "htg-cs2-1")
			return
		}
	}
}

// step moves the clock, lets the Manager catch up and runs the lease job.
func (r *leaseRig) step(t *testing.T, d time.Duration) {
	t.Helper()
	r.now = r.now.Add(d)
	r.up()
	if err := r.leases.Run(r.ctx); err != nil {
		t.Fatal(err)
	}
}

func (r *leaseRig) request(t *testing.T, req LeaseRequest) Lease {
	t.Helper()
	l, err := r.leases.Request(r.ctx, r.app, req)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func (r *leaseRig) row(t *testing.T, id uuid.UUID) store.ServerLease {
	t.Helper()
	l, err := r.lrows.GetServerLease(r.ctx, r.svc.orgID, id)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestALeaseTakesAServerAndGivesItBack(t *testing.T) {
	r := newLeaseRig(t)
	l := r.request(t, LeaseRequest{Pool: "pug", HolderRef: "session-1", ExpectedPlayers: 10, Settings: map[string]string{"start_map": "de_nuke"}, Reason: "PUG 3"})
	if l.State != store.LeaseStarting || l.ServerID != "htg-cs2-1" || len(l.Password) != 10 || l.Password == "join-pw" ||
		l.Connect != "connect "+l.Address+"; password "+l.Password || !strings.HasSuffix(l.Address, ":27015") {
		t.Fatalf("lease %+v", l)
	}
	if h, _ := r.mgr.Host("htg-cs2-1"); h.ExpectedPlayers != 10 || !h.ExpectedUntil.Equal(l.MaxUntil) {
		t.Errorf("the expectation: %d until %s", h.ExpectedPlayers, h.ExpectedUntil)
	}
	if e := r.audit.Entries(); len(e) != 1 || e[0].Action != ActionLeaseRequest || e[0].Outcome != OutcomeAccepted || e[0].Reason != "PUG 3" {
		t.Errorf("audit %+v", e)
	}
	// The lease's password and settings reach the game before it is ready.
	r.step(t, time.Second)
	got := r.row(t, l.ID)
	h, _ := r.mgr.Host("htg-cs2-1")
	env := r.envOn(t, h.InstanceAddress)
	if got.State != store.LeaseReady || got.ReadyAt == nil || !strings.Contains(env, "CS2_PW="+l.Password+"\n") || !strings.Contains(env, "CS2_STARTMAP=de_nuke\n") {
		t.Fatalf("ready: %s, env:\n%s", got.State, env)
	}
	// Asking again with the same reference answers the same lease, password included; another
	// reference finds the pool taken.
	again := r.request(t, LeaseRequest{Pool: "pug", HolderRef: "session-1"})
	if again.ID != l.ID || again.Password != l.Password {
		t.Errorf("again: %+v", again)
	}
	if _, err := r.leases.Request(r.ctx, r.app, LeaseRequest{Pool: "pug", HolderRef: "session-2"}); !errors.Is(err, ErrNoFreeServer) {
		t.Errorf("a second session: %v", err)
	}
	// Given back: parked, and the server's own password at its next start.
	out, err := r.leases.Release(r.ctx, r.app, l.ID, "done early")
	if err != nil || out.State != store.LeaseReleased || out.Password != "" || !strings.Contains(out.Reason, "released by its holder: done early") {
		t.Fatalf("release: %+v %v", out, err)
	}
	if h, _ := r.mgr.Host("htg-cs2-1"); h.Desired != cloudvm.DesiredParked || h.ExpectedPlayers != 0 {
		t.Errorf("after the release: desired %s, expecting %d", h.Desired, h.ExpectedPlayers)
	}
	if spec := r.source(t); spec.JoinPassword != "join-pw" || spec.Settings["start_map"] == "de_nuke" || spec.ExtraEnv != nil {
		t.Errorf("the spec after the release still has the lease: %q %v", spec.JoinPassword, spec.Settings)
	}
	if _, err := r.leases.Release(r.ctx, r.app, l.ID, ""); !errors.Is(err, ErrLeaseFinished) {
		t.Errorf("a second release: %v", err)
	}
	if e := r.audit.Entries(); len(e) != 2 || e[1].Action != ActionLeaseRelease {
		t.Errorf("audit %+v", e)
	}
}

// source is the server's spec as the Manager reads it, secrets included.
func (r *leaseRig) source(t *testing.T) cloudvm.ServerSpec {
	t.Helper()
	specs, err := r.hosts.CloudVMServers(r.ctx)
	if err != nil || len(specs) != 1 {
		t.Fatalf("specs %v %v", specs, err)
	}
	return specs[0]
}

func (r *leaseRig) envOn(t *testing.T, address string) string {
	t.Helper()
	env, ok := r.remote.EnvOn(address)
	if !ok {
		t.Fatalf("no env on %s", address)
	}
	return env
}

func TestALeaseIsRefused(t *testing.T) {
	r := newLeaseRig(t)
	for name, req := range map[string]LeaseRequest{
		"both":                {ServerID: "htg-cs2-1", Pool: "pug", HolderRef: "x"},
		"neither":             {HolderRef: "x"},
		"no reference":        {Pool: "pug"},
		"a setting not lent":  {Pool: "pug", HolderRef: "x", Settings: map[string]string{"game_alias": "casual"}},
		"a bad value":         {Pool: "pug", HolderRef: "x", Settings: map[string]string{"start_map": "../x"}},
		"an unknown plugin":   {Pool: "pug", HolderRef: "x", Plugins: []string{"sourcemod"}},
		"a match, no plugins": {Pool: "pug", HolderRef: "x", MatchConfig: []byte("{}")},
		"a bad match":         {Pool: "pug", HolderRef: "x", Plugins: []string{"matchzy"}, MatchConfig: []byte("nope")},
		"too many players":    {Pool: "pug", HolderRef: "x", ExpectedPlayers: MaxLeasePlayers + 1},
	} {
		if _, err := r.leases.Request(r.ctx, r.app, req); !errors.Is(err, ErrInvalidLease) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := r.leases.Request(r.ctx, r.app, LeaseRequest{Pool: "nope", HolderRef: "x"}); !errors.Is(err, ErrNotLeasable) {
		t.Errorf("an empty pool: %v", err)
	}
	if _, err := r.leases.Request(r.ctx, Actor{}, LeaseRequest{Pool: "pug", HolderRef: "x"}); !errors.Is(err, ErrInvalidLease) {
		t.Errorf("no actor: %v", err)
	}
	off := NewLeases(r.svc, r.hosts, r.lrows, r.audit, r.svc.orgID, nil, prometheus.NewRegistry(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := off.Request(r.ctx, r.app, LeaseRequest{Pool: "pug", HolderRef: "x"}); !errors.Is(err, ErrLeasesOff) {
		t.Errorf("no key: %v", err)
	}
	// Players on the server: it is busy.
	r.game.SetPlayers(cloudvm.GamePlayer{UserID: 2})
	if _, err := r.mgr.Status(r.ctx, "htg-cs2-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.leases.Request(r.ctx, r.app, LeaseRequest{ServerID: "htg-cs2-1", HolderRef: "x"}); !errors.Is(err, ErrServerBusy) {
		t.Errorf("players on: %v", err)
	}
	if r.lrows.Len() != 0 {
		t.Errorf("refused requests left rows: %d", r.lrows.Len())
	}
}

func TestALeaseWithPluginsAndAMatch(t *testing.T) {
	r := newLeaseRig(t)
	l := r.request(t, LeaseRequest{ServerID: "htg-cs2-1", HolderRef: "s", Plugins: []string{"matchzy"}, MatchConfig: []byte(`{"matchid":"1"}`)})
	r.step(t, time.Second)
	h, _ := r.mgr.Host("htg-cs2-1")
	env := r.envOn(t, h.InstanceAddress)
	if !strings.Contains(env, "GRAVEL_PLUGINS=matchzy\n") || !strings.Contains(env, "GRAVEL_PLUGIN_FILES=https://example.com/mz.zip|"+strings.Repeat("a", 64)+"|zip|.\n") ||
		!strings.Contains(env, "GRAVEL_MATCHZY_TOKEN=feed-now\n") || !strings.Contains(env, "GRAVEL_MATCHZY_EVENTS_URL=https://ingest.example.com/api/ingest/matchzy\n") {
		t.Errorf("env:\n%s", env)
	}
	if f, ok := r.remote.FileOn(h.InstanceAddress, "~/cs2/game/csgo/gravel-match.json"); !ok || f != `{"matchid":"1"}` {
		t.Errorf("the match file: %q %v", f, ok)
	}
	if got := r.row(t, l.ID); got.State != store.LeaseStarting {
		t.Fatalf("ready before its plugins: %s", got.State)
	}
	r.game.Answer("css_plugins list", "[MatchZy] 0.9.1 loaded")
	r.step(t, time.Second)
	got := r.row(t, l.ID)
	if got.State != store.LeaseReady || got.PluginState != "loaded" || !got.MatchLoaded || !slices.Contains(r.game.Commands(), "matchzy_loadmatch gravel-match.json") {
		t.Errorf("ready with its match: %+v, commands %v", got, r.game.Commands())
	}
}

func TestALeaseFallsBackToVanilla(t *testing.T) {
	r := newLeaseRig(t)
	l := r.request(t, LeaseRequest{ServerID: "htg-cs2-1", HolderRef: "s", Plugins: []string{"matchzy"}, MatchConfig: []byte(`{}`)})
	r.step(t, time.Second)
	r.step(t, PluginWait)
	got := r.row(t, l.ID)
	if got.PluginState != "vanilla" || !strings.Contains(got.PluginReason, "css_plugins list") {
		t.Fatalf("after %s without the plugins: %+v", PluginWait, got)
	}
	r.step(t, time.Second) // the Manager restarts the game without the set
	h, _ := r.mgr.Host("htg-cs2-1")
	got = r.row(t, l.ID)
	if env := r.envOn(t, h.InstanceAddress); strings.Contains(env, "GRAVEL_PLUGINS") || got.State != store.LeaseReady || got.MatchLoaded {
		t.Errorf("vanilla: %+v, env:\n%s", got, env)
	}
}

func TestALeaseEnds(t *testing.T) {
	cases := []struct {
		name  string
		after func(r *leaseRig, l Lease)
		state string
		why   string
	}{
		{"nobody on", func(r *leaseRig, _ Lease) { r.step(t, DefaultLeaseIdleAfter) }, store.LeaseReleased, "nobody on for 15m0s"},
		{"the match is over", func(r *leaseRig, _ Lease) {
			r.game.SetPlayers(cloudvm.GamePlayer{UserID: 2})
			r.leases.MatchEnded(r.ctx, "htg-cs2-1")
			r.step(t, DefaultLeaseReleaseAfter-time.Second)
			if got := r.row(t, r.only(t)); got.State != store.LeaseEnding {
				t.Errorf("before release_after: %s", got.State)
			}
			r.step(t, time.Second)
		}, store.LeaseReleased, "the match is over"},
		{"the most", func(r *leaseRig, _ Lease) {
			r.game.SetPlayers(cloudvm.GamePlayer{UserID: 2})
			r.step(t, DefaultLeaseMax)
		}, store.LeaseReleased, "the lease reached its most, 4h0m0s"},
		{"a moderator parks it", func(r *leaseRig, _ Lease) {
			owner := Actor{UserID: &r.owner}
			if _, err := r.hosts.SetDesired(r.ctx, owner, "htg-cs2-1", cloudvm.DesiredParked, ""); err != nil {
				t.Fatal(err)
			}
			r.step(t, time.Second)
		}, store.LeaseReleased, "the server was parked"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newLeaseRig(t)
			l := r.request(t, LeaseRequest{Pool: "pug", HolderRef: "s"})
			r.step(t, time.Second)
			c.after(r, l)
			got := r.row(t, l.ID)
			if got.State != c.state || got.Reason != c.why || got.ReleasedAt == nil {
				t.Errorf("%s: %s %q", c.name, got.State, got.Reason)
			}
			if h, _ := r.mgr.Host("htg-cs2-1"); h.Desired != cloudvm.DesiredParked {
				t.Errorf("not parked: %s", h.Desired)
			}
		})
	}
}

func (r *leaseRig) only(t *testing.T) uuid.UUID {
	t.Helper()
	for _, l := range r.lrows.All() {
		return l.ID
	}
	t.Fatal("no lease")
	return uuid.Nil
}

func TestALeaseNeverReadyFails(t *testing.T) {
	r := newLeaseRig(t)
	r.game.Up(false)
	owner := Actor{UserID: &r.owner}
	if _, err := r.hosts.SetDesired(r.ctx, owner, "htg-cs2-1", cloudvm.DesiredParked, ""); err != nil {
		t.Fatal(err)
	}
	r.step(t, time.Second)
	l := r.request(t, LeaseRequest{Pool: "pug", HolderRef: "s"})
	r.step(t, DefaultLeaseReadyWithin)
	got := r.row(t, l.ID)
	if got.State != store.LeaseFailed || !strings.HasPrefix(got.Reason, "not ready within 15m0s") {
		t.Errorf("never ready: %s %q", got.State, got.Reason)
	}
}

func TestALeaseOutlivesARestart(t *testing.T) {
	r := newLeaseRig(t)
	l := r.request(t, LeaseRequest{Pool: "pug", HolderRef: "s"})
	again := NewLeases(r.svc, r.hosts, r.lrows, r.audit, r.svc.orgID, []byte(leaseKey), prometheus.NewRegistry(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := again.Load(r.ctx); err != nil {
		t.Fatal(err)
	}
	r.leases = again.WithClock(func() time.Time { return r.now })
	r.step(t, time.Second)
	if spec := r.source(t); spec.JoinPassword != l.Password {
		t.Errorf("after a restart the password is %q, want the lease's", spec.JoinPassword)
	}
	if got := r.row(t, l.ID); got.State != store.LeaseReady {
		t.Errorf("after a restart: %s", got.State)
	}
}

func TestALeaseIsWatched(t *testing.T) {
	r := newLeaseRig(t)
	l := r.request(t, LeaseRequest{Pool: "pug", HolderRef: "s"})
	ch, cancel := r.leases.Subscribe(l.ID)
	defer cancel()
	r.step(t, time.Second)
	select {
	case <-ch:
	default:
		t.Error("no change was told")
	}
	if got, err := r.leases.Get(r.ctx, l.ID); err != nil || got.Password != "" || got.State != store.LeaseReady {
		t.Errorf("get: %+v %v", got, err)
	}
	if !HeldBy(l, r.app) || HeldBy(l, Actor{UserID: &r.owner}) {
		t.Error("HeldBy")
	}
}
