package cloudvm_test

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"gopkg.in/yaml.v3"

	"github.com/gravel-project/gravel/cloud"
	"github.com/gravel-project/gravel/cloud/fake"
	"github.com/gravel-project/gravel/drivers"
	"github.com/gravel-project/gravel/drivers/cloudvm"
	"github.com/gravel-project/gravel/drivers/cloudvm/cloudvmtest"
	"github.com/gravel-project/gravel/games/cs2"
	"github.com/gravel-project/gravel/internal/jobs"
)

func profile(t testing.TB) cloudvm.Profile {
	t.Helper()
	var spec struct {
		CloudVM cloudvm.Profile `yaml:"cloudvm"`
	}
	if err := yaml.Unmarshal(cs2.GameSpec, &spec); err != nil {
		t.Fatal(err)
	}
	return spec.CloudVM
}

// source is a Source a test edits.
type source struct {
	mu    sync.Mutex
	specs []cloudvm.ServerSpec
}

func (s *source) CloudVMServers(context.Context) ([]cloudvm.ServerSpec, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.specs), nil
}

func (s *source) edit(f func(*cloudvm.ServerSpec)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.specs[0])
}

type rig struct {
	t      *testing.T
	m      *cloudvm.Manager
	p      *fake.Provider
	st     *cloudvmtest.Store
	remote *cloudvmtest.Remote
	game   *cloudvmtest.Game
	src    *source
	egress []string
	reg    *prometheus.Registry
	now    time.Time
	nowMu  sync.Mutex
}

const serverID = "cs2-1"

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, p: fake.New(), st: cloudvmtest.NewStore(), remote: cloudvmtest.NewRemote(), game: cloudvmtest.NewGame(),
		egress: []string{"203.0.113.4"}, now: time.Date(2026, 10, 12, 18, 0, 0, 0, time.UTC)}
	r.src = &source{specs: []cloudvm.ServerSpec{{
		ID: serverID, Name: "HTG CS2", Game: "cs2", GameName: "Counter-Strike 2", IdentityProvider: "steam",
		Location: cloudvm.Location{ID: "loc", Provider: "fake", Region: "fake-1", Token: "token", SSHKey: cloudvmtest.Signer(t),
			ExtraSSHKeys: []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGravelTestKeyGravelTestKeyGravelTestKey000 op"}},
		Profile: profile(t), Size: "dedicated-2", VolumeGiB: 100, AddressReserved: true, ParkAfter: 30 * time.Minute,
		Settings: map[string]string{"start_map": "de_mirage"}, RCONPassword: "rcon-secret", JoinPassword: "join-secret",
		FeedURL: cloudvm.FeedURL("https://ingest.example.com", "feed-secret"),
	}}}
	r.remote.DiskOf = cloudvmtest.HomeOnVolume(r.p)
	r.m = r.manager(r.st)
	return r
}

// env is the environment file on a host (its home is on the server's volume).
func (r *rig) env(address string) string {
	e, _ := r.remote.EnvOn(address)
	return e
}

func (r *rig) manager(st cloudvm.HostStore) *cloudvm.Manager {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := prometheus.NewRegistry()
	r.reg = reg
	m := cloudvm.NewManager(r.src, st, map[string]cloudvm.GameFactory{"cs2": r.game.Factory()}, cloudvm.Options{
		Providers: func(context.Context, cloudvm.Location) (cloud.Provider, error) { return r.p, nil },
		Remote:    r.remote,
		Egress: func(context.Context) ([]string, error) {
			r.nowMu.Lock()
			defer r.nowMu.Unlock()
			return slices.Clone(r.egress), nil
		},
		DefaultImages: map[string]string{"fake": "fedora-44"},
		Runner:        jobs.New(logger, reg),
		Logger:        logger,
		Registerer:    reg,
		Now:           r.clock,
		Sleep:         func(context.Context, time.Duration) {},
		Wake:          func(string) {},
	})
	if err := m.Reconcile(context.Background()); err != nil {
		r.t.Fatal(err)
	}
	return m
}

func (r *rig) clock() time.Time {
	r.nowMu.Lock()
	defer r.nowMu.Unlock()
	return r.now
}

func (r *rig) advance(d time.Duration) {
	r.nowMu.Lock()
	r.now = r.now.Add(d)
	r.nowMu.Unlock()
}

func (r *rig) tick() error { return r.m.Tick(context.Background(), serverID) }

func (r *rig) host() cloudvm.Host {
	h, ok := r.m.Host(serverID)
	if !ok {
		r.t.Fatal("no host")
	}
	return h
}

// up runs the host to running: the host boots, the game answers.
func (r *rig) up() cloudvm.Host {
	r.t.Helper()
	r.remote.BootAll()
	r.game.Up(true)
	if err := r.tick(); err != nil {
		r.t.Fatal(err)
	}
	h := r.host()
	if h.State != cloudvm.StateRunning {
		r.t.Fatalf("state %s/%s (%s), want running", h.State, h.Step, h.LastError)
	}
	return h
}

// world is what the fake cloud holds.
type world struct {
	Instances []cloud.Instance
	Volumes   []cloud.Volume
	Firewalls []cloud.Firewall
	Addresses []cloud.Address
}

func (r *rig) world() world {
	r.t.Helper()
	ctx := context.Background()
	var w world
	var err error
	if w.Instances, err = r.p.Instances(ctx, cloud.Filter{}); err != nil {
		r.t.Fatal(err)
	}
	if w.Volumes, err = r.p.Volumes(ctx, cloud.Filter{}); err != nil {
		r.t.Fatal(err)
	}
	if w.Firewalls, err = r.p.Firewalls(ctx, cloud.Filter{}); err != nil {
		r.t.Fatal(err)
	}
	if w.Addresses, err = r.p.Addresses(ctx, cloud.Filter{}); err != nil {
		r.t.Fatal(err)
	}
	return w
}

// counter is a counter's or gauge's value for one label set, 0 when it was never touched.
func (r *rig) counter(name string, labels map[string]string) float64 {
	r.t.Helper()
	mfs, err := r.reg.Gather()
	if err != nil {
		r.t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	metrics:
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if v, ok := labels[lp.GetName()]; ok && v != lp.GetValue() {
					continue metrics
				}
			}
			return m.GetCounter().GetValue() + m.GetGauge().GetValue()
		}
	}
	return 0
}

// driftEvents counts the host's env_drift events.
func (r *rig) driftEvents() int {
	n := 0
	for _, e := range r.st.Events() {
		if e.Detail == cloudvm.DetailEnvDrift {
			n++
		}
	}
	return n
}

func envPushes(r *cloudvmtest.Remote) int {
	n := 0
	for _, run := range r.Runs() {
		if strings.Contains(run.Command, "/env.tmp") {
			n++
		}
	}
	return n
}

func mutating(calls []string) []string {
	var out []string
	for _, c := range calls {
		for _, p := range []string{"Create", "Delete", "Ensure", "Allocate", "Assign", "Release", "Start", "Stop", "Resize", "Attach", "Detach"} {
			if strings.HasPrefix(c, p) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

func TestCreateFromNothing(t *testing.T) {
	r := newRig(t)
	h := r.up()

	if got := r.st.Transitions(); !slices.Equal(got, []string{"absent>creating", "creating>booting", "booting>starting", "starting>running"}) {
		t.Errorf("transitions %v", got)
	}
	snap := r.world()
	if len(snap.Instances) != 1 || len(snap.Volumes) != 1 || len(snap.Firewalls) != 1 || len(snap.Addresses) != 1 {
		t.Fatalf("made %+v, want one of each", snap)
	}
	inst := snap.Instances[0]
	if inst.Name != serverID || inst.Size != "dedicated-2" || !slices.Contains(inst.Tags, cloudvm.Tag(serverID)) || !slices.Contains(inst.VolumeIDs, h.VolumeID) {
		t.Errorf("instance %+v", inst)
	}
	if v := snap.Volumes[0]; v.Name != cloudvm.VolumeName(serverID) || v.SizeGiB != 100 || !slices.Contains(v.Tags, cloudvm.Tag(serverID)) {
		t.Errorf("volume %+v", v)
	}
	if a := snap.Addresses[0]; a.IP != h.Address || a.InstanceID != inst.ID || !h.AddressReserved {
		t.Errorf("address %+v, host %s", a, h.Address)
	}
	fw := snap.Firewalls[0]
	want := map[string][]string{"udp/27015": cloud.AnyAddress, "tcp/27015": {"203.0.113.4/32"}, "tcp/22": {"203.0.113.4/32"}}
	if len(fw.Inbound) != len(want) {
		t.Errorf("inbound %+v (no GOTV port with tv off)", fw.Inbound)
	}
	for _, rule := range fw.Inbound {
		if w, ok := want[rule.Protocol+"/"+rule.Ports]; !ok || !slices.Equal(rule.Addresses, w) {
			t.Errorf("rule %+v", rule)
		}
	}
	env := r.env(h.InstanceAddress)
	for _, line := range []string{"CS2_RCONPW=rcon-secret", "CS2_PW=join-secret", "CS2_LOG_HTTP_URL=https://ingest.example.com/api/ingest/log/feed-secret",
		"CS2_STARTMAP=de_mirage", "CS2_MAXPLAYERS=10", "TV_ENABLE=0", "CS2_SERVER_HIBERNATE=0", "CS2_SERVERNAME=HTG CS2"} {
		if !strings.Contains(env, line+"\n") {
			t.Errorf("env lacks %q:\n%s", line, env)
		}
	}
	if strings.Contains(env, "SRCDS_TOKEN") {
		t.Error("an absent publisher token was written")
	}
	if h.HostKey == "" || h.InstanceAddress == "" || h.Size != "dedicated-2" || h.DiskGiB != 25 || len(h.EnvSHA256) == 0 {
		t.Errorf("host %+v", h)
	}
	for _, run := range r.remote.Runs() {
		if run.User != "gravel" {
			t.Errorf("ran as %s", run.User)
		}
	}
	if got := r.game.Addrs(); len(got) == 0 || got[0] != h.InstanceAddress+":27015" {
		t.Errorf("game control made for %v, want the instance's address (RCON is not on the reserved one's way out)", got)
	}
}

func TestTickTwiceChangesNothing(t *testing.T) {
	r := newRig(t)
	r.up()
	before := len(mutating(r.p.Calls()))
	saves := r.st.Saves
	for range 3 {
		r.advance(time.Minute)
		if err := r.tick(); err != nil {
			t.Fatal(err)
		}
	}
	if got := mutating(r.p.Calls())[before:]; len(got) != 0 {
		t.Errorf("a settled host made calls %v", got)
	}
	if r.st.Saves != saves {
		t.Errorf("a settled host was saved %d more times", r.st.Saves-saves)
	}
}

func TestWaitsForBootAndInstall(t *testing.T) {
	r := newRig(t)
	if err := r.tick(); err != nil {
		t.Fatal(err)
	}
	if h := r.host(); h.State != cloudvm.StateBooting {
		t.Fatalf("state %s, want booting until the host answers", h.State)
	}
	r.advance(time.Minute)
	r.remote.BootAll()
	if err := r.tick(); err != nil {
		t.Fatal(err)
	}
	if h := r.host(); h.State != cloudvm.StateStarting || h.BootSeconds != 60 {
		t.Fatalf("state %s, boot %d s: want starting, 60", h.State, h.BootSeconds)
	}
	r.advance(10 * time.Minute)
	if err := r.tick(); err != nil {
		t.Fatal(err)
	}
	if h := r.host(); h.State != cloudvm.StateStarting {
		t.Fatalf("state %s: the game is still downloading", h.State)
	}
	r.game.Up(true)
	if err := r.tick(); err != nil {
		t.Fatal(err)
	}
	if h := r.host(); h.State != cloudvm.StateRunning || h.ReadySeconds != 11*60 {
		t.Errorf("state %s, ready %d s; want running, 660", h.State, h.ReadySeconds)
	}
	if n := len(r.remote.Env); n != 1 {
		t.Errorf("env pushed to %d hosts", n)
	}
}

func TestBootAndInstallTimeouts(t *testing.T) {
	r := newRig(t)
	_ = r.tick()
	r.advance(cloudvm.BootTimeout + time.Minute)
	if err := r.tick(); err == nil || r.host().ErrorCode != cloudvm.CodeBootTimeout {
		t.Errorf("a host that never boots: %v, %q", err, r.host().ErrorCode)
	}
	r.remote.BootAll()
	r.advance(time.Hour)
	_ = r.tick()
	r.advance(time.Duration(profile(t).ReadyTimeout) + time.Minute)
	if err := r.tick(); err == nil || r.host().ErrorCode != cloudvm.CodeInstallTimeout {
		t.Errorf("a game that never answers: %v, %q", err, r.host().ErrorCode)
	}
}

// failingStore fails the save that records an instance, as a hub that stopped right after the
// cloud made it would.
type failingStore struct {
	*cloudvmtest.Store
	failed bool
}

func (f *failingStore) SaveHost(ctx context.Context, h cloudvm.Host) error {
	if h.InstanceID != "" && !f.failed {
		f.failed = true
		return errors.New("the hub stopped")
	}
	return f.Store.SaveHost(ctx, h)
}

func TestResumesAfterRestart(t *testing.T) {
	r := newRig(t)
	fs := &failingStore{Store: r.st}
	r.m = r.manager(fs)
	if err := r.tick(); err == nil {
		t.Fatal("the save failed and the tick said nothing")
	}
	if n := len(r.world().Instances); n != 1 {
		t.Fatalf("%d instances after the crash", n)
	}
	// A new hub on the same store finds the instance by its tag and finishes.
	r.m = r.manager(r.st)
	r.up()
	if n := len(r.world().Instances); n != 1 {
		t.Errorf("%d instances after the restart, want the one", n)
	}
	if r.p.CallCount("CreateInstance") != 1 || r.p.CallCount("CreateVolume") != 1 || r.p.CallCount("AllocateAddress") != 1 {
		t.Errorf("made again: %v", mutating(r.p.Calls()))
	}
}

func TestHangingOperationIsPickedUpNextTick(t *testing.T) {
	r := newRig(t)
	r.p = fake.New(fake.WithClock(fake.NewManualClock(r.clock())), fake.WithOperationDuration(time.Hour))
	r.m = r.manager(r.st)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = r.m.Tick(ctx, serverID)
	if h := r.host(); h.State != cloudvm.StateCreating || h.Operation == nil {
		t.Fatalf("state %s/%s op %v, want creating with the operation recorded", h.State, h.Step, h.Operation)
	}
}

func TestFailureBacksOff(t *testing.T) {
	r := newRig(t)
	r.p.FailNext("CreateVolume", errors.New("boom"))
	if err := r.tick(); err == nil {
		t.Fatal("no error")
	}
	h := r.host()
	if h.ErrorCode != cloudvm.CodeCloudFailed || h.Attempts != 1 || !h.NextAttemptAt.After(r.clock()) || h.State != cloudvm.StateCreating {
		t.Fatalf("host %+v", h)
	}
	calls := len(r.p.Calls())
	if err := r.tick(); err != nil || len(r.p.Calls()) != calls {
		t.Errorf("a tick inside the backoff made calls: %v", err)
	}
	r.advance(time.Hour)
	r.up()
	if h := r.host(); h.ErrorCode != "" || h.Attempts != 0 {
		t.Errorf("recovered host keeps %q / %d", h.ErrorCode, h.Attempts)
	}
	r.p.RateLimitNext("Instances")
	r.advance(cloudvm.DriftEvery)
	if err := r.tick(); err == nil || r.host().ErrorCode != cloudvm.CodeCloudRateLimited {
		t.Errorf("rate limited: %v %q", err, r.host().ErrorCode)
	}
}

func TestDuplicateInstancesNeedAHuman(t *testing.T) {
	r := newRig(t)
	for range 2 {
		if _, _, err := r.p.CreateInstance(context.Background(), cloud.InstanceSpec{Name: serverID, Region: "fake-1", Size: "dedicated-2", Image: "x",
			Tags: []string{cloudvm.Tag(serverID)}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.tick(); err == nil || r.host().ErrorCode != cloudvm.CodeDuplicateInstances {
		t.Errorf("%v %q", err, r.host().ErrorCode)
	}
}

func TestParkAndUnpark(t *testing.T) {
	r := newRig(t)
	first := r.up()
	r.game.SetPlayers(cloudvm.GamePlayer{UserID: 2, Name: "a"})
	if _, err := r.m.Status(context.Background(), serverID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.SetDesired(context.Background(), serverID, cloudvm.DesiredParked, 7); err != nil {
		t.Fatal(err)
	}
	if err := r.tick(); err != nil {
		t.Fatal(err)
	}
	h := r.host()
	if h.State != cloudvm.StateParked || h.InstanceID != "" || h.HostKey != "" || h.Address != first.Address || h.VolumeID != first.VolumeID {
		t.Fatalf("parked host %+v", h)
	}
	snap := r.world()
	if len(snap.Instances) != 0 || len(snap.Volumes) != 1 || len(snap.Addresses) != 1 || snap.Addresses[0].InstanceID != "" {
		t.Errorf("after the park %+v: the volume and the address stay, the instance goes (it bills while stopped)", snap)
	}
	if got := r.game.Commands(); len(got) < 2 || !strings.HasPrefix(got[0], "say ") || got[1] != "quit" {
		t.Errorf("game commands %v: warn the players, then quit", got)
	}
	ev := r.st.Events()
	if last := ev[len(ev)-1]; last.To != cloudvm.StateParked || ev[len(ev)-2].AuditID != 7 {
		t.Errorf("events %+v: the request's audit row is on its transition", ev[len(ev)-2:])
	}
	if _, err := r.m.Status(context.Background(), serverID); !errors.Is(err, drivers.ErrHostNotRunning) {
		t.Errorf("status of a parked host: %v", err)
	}
	pushes := envPushes(r.remote)

	if _, err := r.m.SetDesired(context.Background(), serverID, cloudvm.DesiredRunning, 8); err != nil {
		t.Fatal(err)
	}
	r.advance(time.Minute)
	h = r.up()
	if h.Address != first.Address || h.VolumeID != first.VolumeID || h.InstanceID == first.InstanceID {
		t.Errorf("unparked %+v: same address and volume, a new instance", h)
	}
	if n := envPushes(r.remote); n != pushes {
		t.Errorf("the unchanged env was pushed again (%d pushes, was %d): it is on the volume", n, pushes)
	}
	if r.st.Transitions()[4] != "running>parking" || !slices.Contains(r.st.Transitions(), "parked>creating") {
		t.Errorf("transitions %v", r.st.Transitions())
	}
}

func TestAutoParkAfterIdle(t *testing.T) {
	r := newRig(t)
	r.up()
	read := func() {
		if _, err := r.m.Status(context.Background(), serverID); err != nil {
			t.Fatal(err)
		}
	}
	read()
	r.advance(29 * time.Minute)
	read()
	_ = r.tick()
	if s := r.host().State; s != cloudvm.StateRunning {
		t.Fatalf("parked after 29 minutes: %s", s)
	}
	r.game.SetPlayers(cloudvm.GamePlayer{UserID: 2, Name: "a"})
	read()
	r.advance(10 * time.Minute)
	r.game.SetPlayers()
	read()
	r.advance(29 * time.Minute)
	_ = r.tick()
	if s := r.host().State; s != cloudvm.StateRunning {
		t.Fatalf("a player reset the idle clock, but it parked: %s", s)
	}
	r.advance(time.Minute)
	if err := r.tick(); err != nil {
		t.Fatal(err)
	}
	h := r.host()
	if h.State != cloudvm.StateParked || h.Desired != cloudvm.DesiredParked {
		t.Fatalf("after 30 empty minutes: %s, desired %s", h.State, h.Desired)
	}
	ev := r.st.Events()
	if !slices.ContainsFunc(ev, func(e cloudvm.Event) bool { return e.Detail == "idle" && e.AuditID == 0 }) {
		t.Errorf("no idle park event in %+v", ev)
	}
	if got := r.game.Commands(); slices.ContainsFunc(got, func(c string) bool { return strings.HasPrefix(c, "say ") }) {
		t.Errorf("an empty server was warned: %v", got)
	}
}

func TestResize(t *testing.T) {
	r := newRig(t)
	r.up()
	ctx := context.Background()
	r.game.SetPlayers(cloudvm.GamePlayer{UserID: 2, Name: "a"})
	if _, err := r.m.Status(ctx, serverID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Resize(ctx, serverID, "dedicated-4", false, false, 0); !errors.Is(err, cloudvm.ErrPlayersOn) {
		t.Errorf("with a player on: %v", err)
	}
	if _, err := r.m.Resize(ctx, serverID, "medium", true, false, 0); !errors.Is(err, cloudvm.ErrInvalidSize) {
		t.Errorf("another family: %v", err)
	}
	sizes, _ := r.m.Sizes(ctx, serverID)
	if len(sizes) != 1 || sizes[0].Slug != "dedicated-4" {
		t.Errorf("sizes %v", sizes)
	}
	if _, err := r.m.Resize(ctx, serverID, "dedicated-4", true, false, 9); err != nil {
		t.Fatal(err)
	}
	r.advance(time.Minute)
	if err := r.tick(); err != nil {
		t.Fatal(err)
	}
	h := r.host()
	if h.State != cloudvm.StateRunning || h.Size != "dedicated-4" || h.TargetSize != "" || h.DiskGiB != 25 {
		t.Fatalf("after the resize %+v", h)
	}
	if got := mutating(r.p.Calls()); !slices.Contains(got, "StopInstance") || !slices.Contains(got, "ResizeInstance") {
		t.Errorf("calls %v", got)
	}
	if slices.Contains(r.p.StopModes(), false) {
		t.Error("a forced stop: the game deserves a graceful shutdown")
	}
	// And back: the root disk stayed the smaller size's, so the way down is open.
	r.game.SetPlayers()
	_, _ = r.m.Status(ctx, serverID)
	if _, err := r.m.Resize(ctx, serverID, "dedicated-2", false, false, 0); err != nil {
		t.Fatal(err)
	}
	r.p.FailOperationNext("ResizeInstance")
	if err := r.tick(); err != nil {
		t.Fatal(err)
	}
	h = r.host()
	if h.State != cloudvm.StateRunning || h.Size != "dedicated-4" || h.TargetSize != "" {
		t.Errorf("after a failed resize %+v: running again at the old size", h)
	}
	if !slices.ContainsFunc(r.st.Events(), func(e cloudvm.Event) bool { return e.Detail == cloudvm.CodeResizeFailed }) {
		t.Error("the failed resize was not recorded")
	}
	if _, err := r.m.Resize(ctx, serverID, "dedicated-2", false, false, 0); err != nil {
		t.Fatal(err)
	}
	_ = r.tick()
	if h := r.host(); h.Size != "dedicated-2" {
		t.Errorf("the way down: %s", h.Size)
	}
}

// A reboot (gravel#202) stops the same instance gracefully and starts it again: no resize, no new
// instance, the game warned first; players on refuse it unless forced, and a host that isn't
// running refuses it. The request survives a restart of the hub.
func TestReboot(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if _, err := r.m.Reboot(ctx, serverID, true, 0); !errors.Is(err, cloudvm.ErrNotRunning) {
		t.Errorf("rebooting a host that isn't running: %v", err)
	}
	before := r.up()
	r.game.SetPlayers(cloudvm.GamePlayer{UserID: 2, Name: "a"})
	if _, err := r.m.Status(ctx, serverID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Reboot(ctx, serverID, false, 0); !errors.Is(err, cloudvm.ErrPlayersOn) {
		t.Errorf("with a player on: %v", err)
	}
	if _, err := r.m.Reboot(ctx, serverID, true, 12); err != nil {
		t.Fatal(err)
	}
	if h := r.host(); !h.Reboot || h.RequestAuditID != 12 {
		t.Fatalf("the request was not kept: %+v", h)
	}
	// The hub restarts before its next tick: the new manager finds the request in the store.
	r.m = r.manager(r.st)
	if err := r.m.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	// Its poll reads the game, so it knows who is on to warn.
	if _, err := r.m.Status(ctx, serverID); err != nil {
		t.Fatal(err)
	}
	calls := len(r.p.Calls())
	r.advance(time.Minute)
	if err := r.tick(); err != nil {
		t.Fatal(err)
	}
	h := r.host()
	if h.State != cloudvm.StateRunning || h.Reboot || h.InstanceID != before.InstanceID || h.Size != before.Size {
		t.Fatalf("after the reboot %+v", h)
	}
	got := mutating(r.p.Calls()[calls:])
	if !slices.Contains(got, "StopInstance") || !slices.Contains(got, "StartInstance") || slices.Contains(got, "ResizeInstance") || slices.Contains(got, "CreateInstance") {
		t.Errorf("calls %v", got)
	}
	if slices.Contains(r.p.StopModes(), false) {
		t.Error("a forced stop: the game deserves a graceful shutdown")
	}
	if !slices.ContainsFunc(r.st.Events(), func(e cloudvm.Event) bool {
		return e.From == cloudvm.StateRunning && e.To == cloudvm.StateRebooting && e.AuditID == 12
	}) {
		t.Errorf("no running → rebooting transition carrying the request: %+v", r.st.Events())
	}
	if !slices.ContainsFunc(r.game.Commands(), func(c string) bool {
		return strings.HasPrefix(c, "say ") && strings.Contains(c, "restarts in 10 seconds")
	}) {
		t.Errorf("the players were not warned: %v", r.game.Commands())
	}
}

func TestDestroy(t *testing.T) {
	r := newRig(t)
	r.up()
	ctx := context.Background()
	if _, err := r.m.Destroy(ctx, serverID, true, true, 0); !errors.Is(err, cloudvm.ErrNotParked) {
		t.Errorf("destroying a running host: %v", err)
	}
	_, _ = r.m.SetDesired(ctx, serverID, cloudvm.DesiredParked, 0)
	_ = r.tick()
	if _, err := r.m.Destroy(ctx, serverID, true, true, 11); err != nil {
		t.Fatal(err)
	}
	if err := r.tick(); err != nil {
		t.Fatal(err)
	}
	h := r.host()
	if h.State != cloudvm.StateAbsent || h.VolumeID != "" || h.Address != "" || h.FirewallID != "" {
		t.Errorf("destroyed host %+v", h)
	}
	snap := r.world()
	if len(snap.Instances)+len(snap.Volumes)+len(snap.Addresses)+len(snap.Firewalls) != 0 {
		t.Errorf("left %+v", snap)
	}
}

func TestDestroyKeepsTheVolumeUnlessAsked(t *testing.T) {
	r := newRig(t)
	r.up()
	ctx := context.Background()
	r.src.edit(func(s *cloudvm.ServerSpec) { s.Removed = true })
	if err := r.m.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.tick(); err != nil {
		t.Fatal(err)
	}
	if s := r.host().State; s != cloudvm.StateParked {
		t.Fatalf("a removed server's host is %s, want parked", s)
	}
	if _, err := r.m.SetDesired(ctx, serverID, cloudvm.DesiredRunning, 0); !errors.Is(err, cloudvm.ErrRemoved) {
		t.Errorf("running a removed server: %v", err)
	}
	if _, err := r.m.Destroy(ctx, serverID, false, false, 0); err != nil {
		t.Fatal(err)
	}
	_ = r.tick()
	if snap := r.world(); len(snap.Volumes) != 1 || len(snap.Addresses) != 1 || len(snap.Firewalls) != 0 {
		t.Errorf("kept %+v: the volume and address stay unless asked", snap)
	}
}

func TestLostInstanceIsRecreatedOnTheVolume(t *testing.T) {
	r := newRig(t)
	h := r.up()
	if _, err := r.p.DeleteInstance(context.Background(), h.InstanceID); err != nil {
		t.Fatal(err)
	}
	r.advance(cloudvm.DriftEvery)
	if err := r.tick(); err != nil {
		t.Fatal(err)
	}
	got := r.host()
	if got.State != cloudvm.StateRunning || got.VolumeID != h.VolumeID || got.InstanceID == h.InstanceID {
		t.Errorf("after a lost instance %+v", got)
	}
}

func TestStoppedInstanceIsStarted(t *testing.T) {
	r := newRig(t)
	h := r.up()
	if _, err := r.p.StopInstance(context.Background(), h.InstanceID, true); err != nil {
		t.Fatal(err)
	}
	r.advance(cloudvm.DriftEvery)
	if err := r.tick(); err != nil {
		t.Fatal(err)
	}
	if got := r.host(); got.State != cloudvm.StateRunning || got.InstanceID != h.InstanceID {
		t.Errorf("after someone stopped it %+v", got)
	}
}

func TestEgressChangeRepinsTheFirewall(t *testing.T) {
	r := newRig(t)
	r.up()
	r.nowMu.Lock()
	r.egress = []string{"198.51.100.77"}
	r.nowMu.Unlock()
	r.advance(cloudvm.EgressEvery + time.Second)
	if err := r.m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.tick(); err != nil {
		t.Fatal(err)
	}
	fw := r.world().Firewalls[0]
	for _, rule := range fw.Inbound {
		if rule.Protocol == "tcp" && !slices.Equal(rule.Addresses, []string{"198.51.100.77/32"}) {
			t.Errorf("rule %+v still pins the old egress", rule)
		}
	}
	if h := r.host(); !slices.Equal(h.Egress, []string{"198.51.100.77/32"}) {
		t.Errorf("egress recorded %v", h.Egress)
	}
}

func TestEnvRotationWaitsForEmpty(t *testing.T) {
	r := newRig(t)
	h := r.up()
	ctx := context.Background()
	r.game.SetPlayers(cloudvm.GamePlayer{UserID: 2})
	_, _ = r.m.Status(ctx, serverID)
	r.src.edit(func(s *cloudvm.ServerSpec) { s.RCONPassword = "rotated" })
	if err := r.m.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.tick(); err != nil {
		t.Fatal(err)
	}
	if got := r.host(); got.Step != "env_pending" || strings.Contains(r.env(h.InstanceAddress), "rotated") {
		t.Fatalf("pushed with a player on: step %q", got.Step)
	}
	if r.game.Password() != "rotated" {
		t.Error("the game control kept the old password")
	}
	r.game.SetPlayers()
	_, _ = r.m.Status(ctx, serverID)
	if err := r.tick(); err != nil {
		t.Fatal(err)
	}
	if got := r.host(); got.State != cloudvm.StateRunning || !strings.Contains(r.env(h.InstanceAddress), "CS2_RCONPW=rotated\n") {
		t.Errorf("after the room emptied: %s, env:\n%s", got.State, r.env(h.InstanceAddress))
	}
}

func TestHostKeyMismatchIsAnError(t *testing.T) {
	r := newRig(t)
	h := r.up()
	r.remote.HostKeys[h.InstanceAddress] = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIImpostorImpostorImpostorImpostor00000"
	r.src.edit(func(s *cloudvm.ServerSpec) { s.JoinPassword = "new" })
	_ = r.m.Reconcile(context.Background())
	if err := r.tick(); err == nil || r.host().ErrorCode != cloudvm.CodeHostKeyChanged {
		t.Errorf("%v %q", err, r.host().ErrorCode)
	}
	if strings.Contains(r.env(h.InstanceAddress), "CS2_PW=new") {
		t.Error("the env went to a host with another key")
	}
}

func TestDocumentHoldsNoSecret(t *testing.T) {
	r := newRig(t)
	s := r.src.specs[0]
	doc, err := cloudvm.Document(s, s.GameName, "/dev/disk/by-id/scsi-0FAKE_Volume_cs2-1-data", []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGravelTestKeyGravelTestKeyGravelTestKey000 hub"}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"rcon-secret", "join-secret", "feed-secret", "token"} {
		if strings.Contains(doc, secret) {
			t.Errorf("the document holds %q", secret)
		}
	}
	for _, want := range []string{"ConditionPathExists=%h/.config/cs2/env", "UserNS=keep-id:uid=1000,gid=1000", "Volume=%h/cs2:/home/steam/cs2-dedicated:Z",
		"EnvironmentFile=%h/.config/cs2/env", "Image=ghcr.io/joedwards32/cs2:5.0.0@sha256:", "Network=host", "/var/lib/gravel/boot-done"} {
		if !strings.Contains(doc, want) {
			t.Errorf("the document lacks %q", want)
		}
	}
}

var update = flag.Bool("update", false, "rewrite the golden files in testdata/")

func TestDocumentGolden(t *testing.T) {
	r := newRig(t)
	s := r.src.specs[0]
	doc, err := cloudvm.Document(s, s.GameName, "/dev/disk/by-id/scsi-0DO_Volume_cs2-1-data", []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGravelTestKeyGravelTestKeyGravelTestKey000 gravel-hub"}, "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "cs2.golden.yaml")
	if *update {
		if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if doc != string(want) {
		t.Errorf("the document differs from %s (go test -update after a deliberate change)", path)
	}
}

// TestUnitDryRun runs podman's quadlet generator over the rendered unit, when it is installed.
func TestUnitDryRun(t *testing.T) {
	gen := "/usr/libexec/podman/quadlet"
	if _, err := os.Stat(gen); err != nil {
		t.Skip("no quadlet generator")
	}
	r := newRig(t)
	s := r.src.specs[0]
	doc, err := cloudvm.Document(s, s.GameName, "/dev/x", []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGravelTestKeyGravelTestKeyGravelTestKey000 x"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		WriteFiles []struct {
			Path    string `yaml:"path"`
			Content string `yaml:"content"`
		} `yaml:"write_files"`
	}
	if err := yaml.Unmarshal([]byte(doc), &parsed); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, f := range parsed.WriteFiles {
		if strings.HasSuffix(f.Path, ".container") {
			if err := os.WriteFile(filepath.Join(dir, "cs2.container"), []byte(f.Content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	cmd := exec.CommandContext(t.Context(), gen, "-dryrun", "-user")
	cmd.Env = append(os.Environ(), "QUADLET_UNIT_DIRS="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "cs2.service") || !strings.Contains(string(out), "--env-file") {
		t.Errorf("quadlet: %v\n%s", err, out)
	}
}

func TestRenderEnv(t *testing.T) {
	r := newRig(t)
	s := r.src.specs[0]
	s.Settings = map[string]string{"tv": "true", "game_type": "1", "game_mode": "2"}
	env, err := cloudvm.RenderEnv(s, s.Size)
	if err != nil {
		t.Fatal(err)
	}
	e := string(env)
	for _, line := range []string{"TV_ENABLE=1", "CS2_GAMETYPE=1", "CS2_GAMEMODE=2"} {
		if !strings.Contains(e, line+"\n") {
			t.Errorf("env lacks %q", line)
		}
	}
	if strings.Contains(e, "CS2_GAMEALIAS") {
		t.Error("game_alias defaulted beside game_type")
	}
	lines := strings.Split(strings.TrimSpace(e), "\n")
	if !slices.IsSorted(lines) {
		t.Error("env lines are not sorted (a stable hash needs them sorted)")
	}
	s.JoinPassword = "a\nb"
	if _, err := cloudvm.RenderEnv(s, s.Size); err == nil {
		t.Error("a value with a line break was written")
	}
}

// TestEnvScriptQueuesTheRestart holds the fix for HTG's first live run: a blocking restart waited
// on the image pull and podman's copy for the user namespace, was cut at remoteRunTimeout, and
// each retry restarted the copy.
func TestEnvScriptQueuesTheRestart(t *testing.T) {
	script := cloudvm.EnvScript("cs2", nil, true)
	if !strings.Contains(script, "systemctl --user restart --no-block cs2.service\n") {
		t.Errorf("the restart is not queued with --no-block:\n%s", script)
	}
	for _, line := range strings.Split(script, "\n") {
		if strings.Contains(line, "systemctl") && strings.Contains(line, " restart ") && !strings.Contains(line, "--no-block") {
			t.Errorf("a blocking restart: %q", line)
		}
	}
	if !strings.Contains(script, "mv -f ~/.config/cs2/env.tmp ~/.config/cs2/env\n") {
		t.Errorf("the file is not written in one move:\n%s", script)
	}
	if strings.Contains(cloudvm.EnvScript("cs2", nil, false), "systemctl") {
		t.Error("without restart the script touches the unit")
	}
}

// TestBootMarkerIsOnTheRootDisk holds the fix for HTG's first warm start: the marker in the
// runtime user's home was on the data volume, so a replacement instance showed the previous
// instance's marker and was taken as booted before its own first boot ran.
func TestBootMarkerIsOnTheRootDisk(t *testing.T) {
	if strings.HasPrefix(cloudvm.BootMarker, "~") || strings.HasPrefix(cloudvm.BootMarker, "/home/") || !strings.HasPrefix(cloudvm.BootMarker, "/") {
		t.Fatalf("BootMarker %q is not an absolute path off the volume", cloudvm.BootMarker)
	}
	r := newRig(t)
	r.remote.BootAll()
	for i := 0; i < 4; i++ {
		_ = r.tick()
	}
	var checked bool
	for _, run := range r.remote.Runs() {
		if strings.Contains(run.Command, "$HOME/.gravel-boot-done") || strings.Contains(run.Command, "~/.gravel-boot-done") {
			t.Errorf("the hub checked the marker on the volume: %q", run.Command)
		}
		if run.Command == "test -e "+cloudvm.BootMarker {
			checked = true
		}
	}
	if !checked {
		t.Error("the hub never checked the marker on the root disk")
	}
}

// TestReservedEgressInTheDocument holds the fix for HTG's first live run: a DigitalOcean reserved
// IP answered nothing, because the game replied from the instance's own address. A server with a
// reserved address gets the provider's egress script as a boot service and a NetworkManager hook;
// one without, or on a provider that needs none, gets neither.
func TestReservedEgressInTheDocument(t *testing.T) {
	r := newRig(t)
	s := r.src.specs[0]
	const script = "#!/bin/sh\n# provider egress\nip route replace default via 10.0.0.1\n"
	keys := []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGravelTestKeyGravelTestKeyGravelTestKey000 gravel-hub"}
	doc, err := cloudvm.Document(s, s.GameName, "/dev/disk/by-id/scsi-0DO_Volume_cs2-1-data", keys, script)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"path: /usr/local/libexec/gravel-reserved-egress", "# provider egress",
		"path: /etc/systemd/system/gravel-reserved-egress.service", "ExecStart=/usr/local/libexec/gravel-reserved-egress",
		"path: /etc/NetworkManager/dispatcher.d/90-gravel-reserved-egress",
		"systemctl enable --now --no-block gravel-reserved-egress.service",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("the document lacks %q", want)
		}
	}
	path := filepath.Join("testdata", "cs2-reserved-egress.golden.yaml")
	if *update {
		if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if want, err := os.ReadFile(path); err != nil || doc != string(want) {
		t.Errorf("the document differs from %s (go test -update after a deliberate change): %v", path, err)
	}
	for name, spec := range map[string]func(){
		"no reserved address": func() { s.AddressReserved = false },
		"no provider script":  func() { script2 = "" },
	} {
		s, script2 = r.src.specs[0], script
		spec()
		doc, err := cloudvm.Document(s, s.GameName, "/dev/x", keys, script2)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(doc, "gravel-reserved-egress") {
			t.Errorf("%s: the document installs the egress script", name)
		}
	}
}

var script2 string

// TestSourceAddressesIncludeTheReservedAddress: once the egress script runs, the log push comes
// from the reserved address, so the log route's pin admits it beside the instance's own.
func TestSourceAddressesIncludeTheReservedAddress(t *testing.T) {
	r := newRig(t)
	r.remote.BootAll()
	for i := 0; i < 6; i++ {
		_ = r.tick()
	}
	h := r.host()
	if !h.AddressReserved || h.Address == "" || h.InstanceAddress == "" || h.Address == h.InstanceAddress {
		t.Fatalf("the rig's host has no separate reserved address: %+v", h)
	}
	got, ok := r.m.SourceAddresses(serverID)
	if !ok || len(got) != 2 || got[0].String() != h.InstanceAddress || got[1].String() != h.Address {
		t.Errorf("SourceAddresses = %v %v, want [%s %s]", got, ok, h.InstanceAddress, h.Address)
	}
	if _, ok := r.m.SourceAddresses("nope"); ok {
		t.Error("a server with no host has source addresses")
	}
}

// #169: the hub reads the environment file on the host, not only the hash it recorded.

const handEdit = "CS2_IP=10.48.0.6\n" // HTG's first live run: this line bound RCON to the anchor address

func TestEnvDriftOnTheVolumeIsPushedAgainAtUnpark(t *testing.T) {
	r := newRig(t)
	h := r.up()
	want := r.env(h.InstanceAddress)
	r.remote.SetEnv(h.InstanceAddress, want+handEdit) // on the volume, so it outlives the instance
	ctx := context.Background()
	if _, err := r.m.SetDesired(ctx, serverID, cloudvm.DesiredParked, 0); err != nil {
		t.Fatal(err)
	}
	if err := r.tick(); err != nil {
		t.Fatal(err)
	}
	if r.driftEvents() != 0 {
		t.Fatal("drift recorded before the hub looked")
	}
	pushes := envPushes(r.remote)
	if _, err := r.m.SetDesired(ctx, serverID, cloudvm.DesiredRunning, 0); err != nil {
		t.Fatal(err)
	}
	r.advance(time.Minute)
	got := r.up()
	if r.env(got.InstanceAddress) != want {
		t.Errorf("the hand edit survived the unpark:\n%s", r.env(got.InstanceAddress))
	}
	if n := envPushes(r.remote) - pushes; n != 1 {
		t.Errorf("%d pushes at the unpark, want 1", n)
	}
	if r.driftEvents() != 1 || r.counter("gravel_cloudvm_env_drift_total", map[string]string{"server": serverID}) != 1 {
		t.Errorf("drift events %d, counter %v: want one of each", r.driftEvents(), r.counter("gravel_cloudvm_env_drift_total", nil))
	}
	for _, e := range r.st.Events() {
		if e.Detail == cloudvm.DetailEnvDrift && (e.From != cloudvm.StateStarting || strings.Contains(e.Detail+e.Step, "CS2_IP")) {
			t.Errorf("drift event %+v: recorded at starting, without the file's content", e)
		}
	}
}

func TestEnvDriftIsPushedAgainOnARecreatedInstance(t *testing.T) {
	r := newRig(t)
	h := r.up()
	want := r.env(h.InstanceAddress)
	r.remote.SetEnv(h.InstanceAddress, want+handEdit)
	if _, err := r.p.DeleteInstance(context.Background(), h.InstanceID); err != nil {
		t.Fatal(err)
	}
	r.advance(cloudvm.DriftEvery)
	if err := r.tick(); err != nil {
		t.Fatal(err)
	}
	got := r.host()
	if got.State != cloudvm.StateRunning || got.InstanceID == h.InstanceID {
		t.Fatalf("after a lost instance %+v", got)
	}
	if r.env(got.InstanceAddress) != want || r.driftEvents() != 1 {
		t.Errorf("recreate: drift events %d, env:\n%s", r.driftEvents(), r.env(got.InstanceAddress))
	}
}

func TestEnvDriftWhileRunningEmptyIsPushedAtTheDriftPass(t *testing.T) {
	r := newRig(t)
	h := r.up()
	want := r.env(h.InstanceAddress)
	r.remote.SetEnv(h.InstanceAddress, want+handEdit)
	pushes := envPushes(r.remote)
	r.advance(cloudvm.DriftEvery - time.Second)
	if err := r.tick(); err != nil {
		t.Fatal(err)
	}
	if envPushes(r.remote) != pushes {
		t.Fatal("pushed before the drift pass")
	}
	r.advance(time.Second)
	if err := r.tick(); err != nil {
		t.Fatal(err)
	}
	if got := r.host(); got.State != cloudvm.StateRunning || r.env(h.InstanceAddress) != want || envPushes(r.remote)-pushes != 1 {
		t.Errorf("after the drift pass: %s, %d pushes, env:\n%s", got.State, envPushes(r.remote)-pushes, r.env(h.InstanceAddress))
	}
	if !slices.Contains(r.st.Transitions(), "running>starting") || r.driftEvents() != 1 {
		t.Errorf("transitions %v, drift events %d: the game restarts on the pushed file", r.st.Transitions(), r.driftEvents())
	}
	r.advance(cloudvm.DriftEvery)
	if err := r.tick(); err != nil {
		t.Fatal(err)
	}
	if r.driftEvents() != 1 || envPushes(r.remote)-pushes != 1 {
		t.Errorf("a matching file was pushed again or recorded as drift (%d events)", r.driftEvents())
	}
}

func TestEnvDriftWithPlayersOnWaitsForEmpty(t *testing.T) {
	r := newRig(t)
	h := r.up()
	ctx := context.Background()
	want := r.env(h.InstanceAddress)
	r.game.SetPlayers(cloudvm.GamePlayer{UserID: 2})
	_, _ = r.m.Status(ctx, serverID)
	r.remote.SetEnv(h.InstanceAddress, want+handEdit)
	r.advance(cloudvm.DriftEvery)
	if err := r.tick(); err != nil {
		t.Fatal(err)
	}
	if got := r.host(); got.Step != "env_pending" || r.env(h.InstanceAddress) == want {
		t.Fatalf("with a player on: step %q; the file must wait for an empty server", got.Step)
	}
	r.advance(cloudvm.DriftEvery)
	if err := r.tick(); err != nil {
		t.Fatal(err)
	}
	if r.driftEvents() != 1 {
		t.Errorf("drift recorded %d times while it waited", r.driftEvents())
	}
	r.game.SetPlayers()
	_, _ = r.m.Status(ctx, serverID)
	if err := r.tick(); err != nil {
		t.Fatal(err)
	}
	if got := r.host(); got.State != cloudvm.StateRunning || r.env(h.InstanceAddress) != want {
		t.Errorf("after the room emptied: %s, env:\n%s", got.State, r.env(h.InstanceAddress))
	}
}

func TestEnvHashUnreadableAtStartFailsTheStep(t *testing.T) {
	r := newRig(t)
	r.remote.BootAll()
	r.game.Up(true)
	r.remote.FailHash("*", errors.New("ssh: handshake failed"))
	if err := r.tick(); err == nil {
		t.Fatal("no error with an unreadable hash")
	}
	if got := r.host(); got.State != cloudvm.StateStarting || got.ErrorCode != cloudvm.CodeSSHFailed || envPushes(r.remote) != 0 {
		t.Fatalf("host %s/%s code %q, %d pushes: never trust the record when the host can't be read", got.State, got.Step, got.ErrorCode, envPushes(r.remote))
	}
	r.remote.FailHash("*", nil)
	r.advance(10 * time.Minute)
	if got := r.up(); envPushes(r.remote) != 1 || got.ErrorCode != "" {
		t.Errorf("after the host answered: %d pushes, code %q", envPushes(r.remote), got.ErrorCode)
	}
}

func TestEnvHashUnreadableWhileRunningKeepsTheGame(t *testing.T) {
	r := newRig(t)
	r.up()
	pushes := envPushes(r.remote)
	r.remote.FailHash("*", errors.New("ssh: handshake failed"))
	r.advance(cloudvm.DriftEvery)
	if err := r.tick(); err != nil {
		t.Fatal(err)
	}
	if got := r.host(); got.State != cloudvm.StateRunning || got.ErrorCode != "" || envPushes(r.remote) != pushes || r.driftEvents() != 0 {
		t.Errorf("host %s code %q, %d new pushes, %d drift events: a running game is left alone", got.State, got.ErrorCode, envPushes(r.remote)-pushes, r.driftEvents())
	}
}

func TestEnvHashScriptPrintsOnlyTheHash(t *testing.T) {
	s := cloudvm.EnvHashScript("cs2", profile(t).Hooks)
	if !strings.Contains(s, "sha256sum") || !strings.Contains(s, "for f in ~/.config/cs2/env ~/cs2/pre.sh; do") || !strings.Contains(s, "echo missing") || strings.Contains(s, "cat ") {
		t.Errorf("script %q: the hashes or \"missing\", never the files", s)
	}
}

// #170: the game's hooks travel with the environment file, in the script itself (they hold no
// secret), each written in one move into the game's directory on the volume.
func TestEnvScriptWritesTheHooks(t *testing.T) {
	hooks := []cloudvm.Hook{{Path: "pre.sh", Content: "echo one\n"}, {Path: "cfg/x.cfg", Content: strings.Repeat("y", 200)}}
	script := cloudvm.EnvScript("cs2", hooks, true)
	for _, want := range []string{"mkdir -p ~/cs2\nbase64 -d > ~/cs2/pre.sh.tmp <<'GRAVEL_HOOK'\n", "mv -f ~/cs2/pre.sh.tmp ~/cs2/pre.sh\n",
		"mkdir -p ~/cs2/cfg\n", "mv -f ~/cs2/cfg/x.cfg.tmp ~/cs2/cfg/x.cfg\n"} {
		if !strings.Contains(script, want) {
			t.Errorf("the script lacks %q:\n%s", want, script)
		}
	}
	if i, j := strings.Index(script, "/env\n"), strings.Index(script, "pre.sh.tmp"); i < 0 || j < i || strings.Index(script, "systemctl") < j {
		t.Errorf("the hooks are written after the environment and before the restart:\n%s", script)
	}
	for _, line := range strings.Split(script, "\n") {
		if len(line) > 76 {
			t.Errorf("a line over 76 characters: %q", line)
		}
	}
	// The script decodes on a real shell to the hooks' exact bytes.
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	home := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "bash", "-c", strings.ReplaceAll(script, "systemctl", "true")) // no user manager here
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
	cmd.Stdin = strings.NewReader("A=1\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, h := range hooks {
		b, err := os.ReadFile(filepath.Join(home, "cs2", h.Path))
		if err != nil || string(b) != h.Content {
			t.Errorf("%s: %q %v", h.Path, b, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".config", "cs2", "env")); string(b) != "A=1\n" {
		t.Errorf("env %q", b)
	}
}

// #170 with #169: a hook deleted or edited on the volume is a difference like the environment's,
// pushed again at a start, and while running at the drift pass.
func TestHookDriftIsPushedAgain(t *testing.T) {
	r := newRig(t)
	h := r.up()
	want, ok := r.remote.FileOn(h.InstanceAddress, "~/cs2/pre.sh")
	if !ok || !strings.Contains(want, "[redacted]") {
		t.Fatalf("the first start didn't write pre.sh: %q", want)
	}
	edited := want + "echo hand edit\n"
	r.remote.SetFile(h.InstanceAddress, "~/cs2/pre.sh", &edited)
	pushes := envPushes(r.remote)
	r.advance(cloudvm.DriftEvery)
	if err := r.tick(); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.remote.FileOn(h.InstanceAddress, "~/cs2/pre.sh"); got != want || envPushes(r.remote)-pushes != 1 || r.driftEvents() != 1 {
		t.Errorf("edited hook: %d pushes, %d drift events, file:\n%s", envPushes(r.remote)-pushes, r.driftEvents(), got)
	}

	// Deleted on the volume while parked: the unpark puts it back.
	ctx := context.Background()
	if _, err := r.m.SetDesired(ctx, serverID, cloudvm.DesiredParked, 0); err != nil {
		t.Fatal(err)
	}
	if err := r.tick(); err != nil {
		t.Fatal(err)
	}
	r.remote.SetFile(h.InstanceAddress, "~/cs2/pre.sh", nil)
	if _, err := r.m.SetDesired(ctx, serverID, cloudvm.DesiredRunning, 0); err != nil {
		t.Fatal(err)
	}
	r.advance(time.Minute)
	got := r.up()
	if f, ok := r.remote.FileOn(got.InstanceAddress, "~/cs2/pre.sh"); !ok || f != want {
		t.Errorf("a deleted hook wasn't put back at the unpark: %v", ok)
	}
}
