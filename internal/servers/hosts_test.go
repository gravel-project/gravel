package servers

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/crypto/ssh"

	"github.com/gravel-project/gravel/cloud"
	"github.com/gravel-project/gravel/cloud/fake"
	"github.com/gravel-project/gravel/drivers/cloudvm"
	"github.com/gravel-project/gravel/drivers/cloudvm/cloudvmtest"
	"github.com/gravel-project/gravel/internal/jobs"
	"github.com/gravel-project/gravel/internal/servers/serverstest"
	"github.com/gravel-project/gravel/internal/store"
)

// hostRows is HostRows in memory over the fake servers store.
type hostRows struct {
	*serverstest.FakeStore
	mu     sync.Mutex
	hosts  map[string]store.ServerHost
	events []store.ServerHostEvent
}

func (f *hostRows) ListServerHosts(context.Context, uuid.UUID) ([]store.ServerHost, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.ServerHost
	for _, h := range f.hosts {
		out = append(out, h)
	}
	return out, nil
}

func (f *hostRows) SaveServerHost(_ context.Context, h store.ServerHost, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hosts[h.ServerID] = h
	return nil
}

func (f *hostRows) InsertServerHostEvent(_ context.Context, e store.ServerHostEvent) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e.ID = int64(len(f.events) + 1)
	f.events = append(f.events, e)
	return e.ID, nil
}

func (f *hostRows) ListServerHostEvents(_ context.Context, _ uuid.UUID, serverID string, _ int64, limit int) ([]store.ServerHostEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.ServerHostEvent
	for i := len(f.events) - 1; i >= 0 && len(out) < limit; i-- {
		if f.events[i].ServerID == serverID {
			out = append(out, f.events[i])
		}
	}
	return out, nil
}

func (f *hostRows) RunningSecondsSince(context.Context, uuid.UUID, string, time.Time, time.Time) (int64, error) {
	return 3600, nil
}

func (f *hostRows) RunningCostSince(context.Context, uuid.UUID, string, time.Time, time.Time) (float64, error) {
	return 0.25, nil
}

func writeKey(t *testing.T, path string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
}

type hostsRig struct {
	svc    *Service
	rows   *hostRows
	hosts  *Hosts
	audit  *serverstest.AuditStore
	m      Manifest
	dir    string
	mgr    *cloudvm.Manager
	game   *cloudvmtest.Game
	remote *cloudvmtest.Remote
	owner  uuid.UUID
}

func newHostsRig(t *testing.T) *hostsRig { return newHostsRigWith(t, nil, nil) }

// newHostsRigWith is the rig with the manifest edited before it is applied, and a clock the
// Manager reads (nil: the wall clock).
func newHostsRigWith(t *testing.T, edit func(*Manifest), now func() time.Time) *hostsRig {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := &hostsRig{rows: &hostRows{FakeStore: serverstest.NewFakeStore(), hosts: map[string]store.ServerHost{}}, audit: serverstest.NewAuditStore(),
		dir: t.TempDir(), game: cloudvmtest.NewGame(), owner: uuid.New()}
	r.svc = New(r.rows, uuid.New(), []string{"cloudvm", "wardogs"}, logger).WithProviders("fake")
	r.m = parseCloud(t)
	r.m.Locations[0].Provider = "fake"
	r.m.Locations[0].Region = "fake-1"
	file := func(name, v string) string {
		p := filepath.Join(r.dir, name)
		if err := os.WriteFile(p, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	r.m.Locations[0].TokenFile = file("token", "do-token\n")
	r.m.Locations[0].SSHKeyFile = filepath.Join(r.dir, "key")
	writeKey(t, r.m.Locations[0].SSHKeyFile)
	s := &r.m.Servers[0]
	s.CredentialFile, s.Feed.TokenFile, s.VM.PasswordFile = file("rcon", "rcon-pw\n"), file("feed", "feed-now\nfeed-before\n"), file("pw", "join-pw\n")
	s.VM.Size = "dedicated-2"
	if edit != nil {
		edit(&r.m)
	}
	if _, err := r.svc.Apply(context.Background(), r.m, ApplyOptions{CheckCredentials: true}); err != nil {
		t.Fatal(err)
	}
	r.hosts = NewHosts(r.svc, r.rows, r.audit, r.svc.orgID, logger)
	remote := cloudvmtest.NewRemote()
	remote.BootAll()
	r.remote = remote
	r.game.Up(true)
	reg := prometheus.NewRegistry()
	p := fake.New()
	r.mgr = cloudvm.NewManager(r.hosts, r.hosts, map[string]cloudvm.GameFactory{"cs2": r.game.Factory()}, cloudvm.Options{
		Providers: func(context.Context, cloudvm.Location) (cloud.Provider, error) { return p, nil },
		Remote:    remote, Egress: func(context.Context) ([]string, error) { return []string{"203.0.113.4"}, nil },
		DefaultImages: map[string]string{"fake": "fedora"}, Runner: jobs.New(logger, reg), Logger: logger, Registerer: reg,
		Sleep: func(context.Context, time.Duration) {}, Wake: func(string) {}, Now: now,
	})
	r.hosts.Attach(r.mgr)
	return r
}

func TestHostsSource(t *testing.T) {
	r := newHostsRig(t)
	ctx := context.Background()
	specs, err := r.hosts.CloudVMServers(ctx)
	if err != nil || len(specs) != 1 {
		t.Fatalf("specs %v %v", specs, err)
	}
	s := specs[0]
	if s.RCONPassword != "rcon-pw" || s.JoinPassword != "join-pw" || s.PublisherToken != "" || s.FeedURL != "https://ingest.example.com/api/ingest/log/feed-now" ||
		s.Location.Token != "do-token" || s.Location.SSHKey == nil || s.ParkAfter != 30*time.Minute || !s.AddressReserved || s.Size != "dedicated-2" ||
		s.Profile.RCONPort != 27015 || s.GameName != "Counter-Strike 2" || s.Removed {
		t.Errorf("spec %+v", s)
	}
	// A live server whose secret cannot be read waits until it can.
	if err := os.Remove(r.m.Servers[0].VM.PasswordFile); err != nil {
		t.Fatal(err)
	}
	if specs, _ := r.hosts.CloudVMServers(ctx); len(specs) != 0 {
		t.Errorf("a server with a missing password file was handed over: %+v", specs)
	}
	// A removed one needs only its location's secrets: it is parked and destroyed, never run.
	m := r.m
	m.Servers = nil
	m.Locations = r.m.Locations
	if _, err := r.svc.Apply(ctx, m, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	specs, _ = r.hosts.CloudVMServers(ctx)
	if len(specs) != 1 || !specs[0].Removed {
		t.Errorf("removed server %+v", specs)
	}
}

func TestHostsEndToEnd(t *testing.T) {
	r := newHostsRig(t)
	ctx := context.Background()
	if err := r.mgr.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.mgr.Tick(ctx, "htg-cs2-1"); err != nil {
		t.Fatal(err)
	}
	v, err := r.hosts.Host(ctx, "htg-cs2-1")
	if err != nil || v.State != cloudvm.StateRunning || v.PriceHourly <= 0 || v.RunningSecondsMonth != 3600 || v.Region != "fake-1" ||
		v.VolumeGiB != 100 || v.ParkAfter != 30*time.Minute || len(v.Sizes) != 1 || v.MonthCostUSD() <= 0 {
		t.Fatalf("view %+v %v", v, err)
	}
	// The rows round-trip: a new manager on them sees the same host.
	rows, _ := r.hosts.Hosts(ctx)
	if len(rows) != 1 || rows[0].InstanceID == "" || rows[0].HostKey == "" || len(rows[0].EnvSHA256) == 0 || rows[0].Desired != cloudvm.DesiredRunning {
		t.Errorf("stored host %+v", rows)
	}

	owner := Actor{UserID: &r.owner, RequestID: "req-1"}
	r.game.SetPlayers(cloudvm.GamePlayer{UserID: 2})
	if _, err := r.mgr.Status(ctx, "htg-cs2-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.hosts.Resize(ctx, owner, "htg-cs2-1", "dedicated-4", false, false, ""); !errors.Is(err, cloudvm.ErrPlayersOn) {
		t.Errorf("resize with a player on: %v", err)
	}
	id, err := r.hosts.SetDesired(ctx, owner, "htg-cs2-1", cloudvm.DesiredParked, "nobody tonight")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.mgr.Tick(ctx, "htg-cs2-1"); err != nil {
		t.Fatal(err)
	}
	entries := r.audit.Entries()
	if len(entries) != 2 || entries[0].Action != ActionHostResize || entries[0].Outcome != OutcomePlayersOn ||
		entries[1].Action != ActionHostPark || entries[1].Outcome != OutcomeAccepted || entries[1].ID != id || entries[1].Reason != "nobody tonight" {
		t.Errorf("audit %+v", entries)
	}
	ev, more, err := r.hosts.Events(ctx, "htg-cs2-1", 0, 2)
	if err != nil || !more || len(ev) != 2 || ev[0].ToState != cloudvm.StateParked || ev[1].AuditID == nil || *ev[1].AuditID != id {
		t.Errorf("events %+v %v %v", ev, more, err)
	}
	if _, err := r.hosts.SetDesired(ctx, Actor{}, "htg-cs2-1", cloudvm.DesiredRunning, ""); err == nil {
		t.Error("a request with no actor")
	}
	if _, err := r.hosts.SetDesired(ctx, owner, "nope", cloudvm.DesiredRunning, ""); !errors.Is(err, ErrNotCloudVM) {
		t.Errorf("an unknown server: %v", err)
	}
	if _, err := r.hosts.Host(ctx, "nope"); !errors.Is(err, ErrNotCloudVM) {
		t.Errorf("host of an unknown server: %v", err)
	}
	if n := len(r.audit.Entries()); n != 2 {
		t.Errorf("refused-before-recording requests were recorded: %d entries", n)
	}
}

// A host's row keeps every field the Manager writes, the scaling policy's included (#37).
func TestHostRowRoundTrip(t *testing.T) {
	at := time.Date(2026, 10, 12, 18, 0, 0, 0, time.UTC)
	h := cloudvm.Host{ServerID: "htg-cs2-1", LocationID: "do-sfo3",
		Intent: cloudvm.Intent{Desired: cloudvm.DesiredRunning, TargetSize: "c-4", PinnedSize: "c-4", RequestAuditID: 7},
		State:  cloudvm.StateRunning, Size: "c-2", DiskGiB: 25, InstanceID: "1", InstanceAddress: "203.0.113.1", VolumeID: "v",
		Address: "198.51.100.1", AddressReserved: true, FirewallID: "f", Egress: []string{"203.0.113.4/32"}, HostKey: "k",
		EnvSHA256: []byte{1}, StateSince: at, InstanceCreatedAt: at, EmptySince: at, BootSeconds: 40, ReadySeconds: 120,
		PendingSize: "c-4", PendingReason: "up: 9 of 10 players for 5m0s", PendingSince: at.Add(time.Minute), LastResizeAt: at.Add(-time.Hour)}
	row, err := hostToRow(uuid.New(), h)
	if err != nil {
		t.Fatal(err)
	}
	got, err := hostFromRow(row)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, h) {
		t.Errorf("round trip:\n got %+v\nwant %+v", got, h)
	}
}
