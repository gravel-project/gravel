package servers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/gravel-project/gravel/drivers"
	"github.com/gravel-project/gravel/internal/jobs"
	"github.com/gravel-project/gravel/internal/servers/serverstest"
)

// listDriver is a driver with no configuration document that enforces the hub's ban list itself
// (drivers.BanList), as the cloud-VM driver does.
type listDriver struct {
	mu      sync.Mutex
	caps    []string
	lists   [][]drivers.Identity // every list it was handed
	banned  []drivers.Identity   // online bans (kicks) it was asked for
	setErr  error
	players []drivers.Player
}

var (
	_ drivers.Driver  = (*listDriver)(nil)
	_ drivers.BanList = (*listDriver)(nil)
)

func (d *listDriver) Build(context.Context) (string, error)          { return "b1", nil }
func (d *listDriver) Capabilities(context.Context) ([]string, error) { return d.caps, nil }
func (d *listDriver) Status(context.Context) (drivers.Status, error) {
	return drivers.Status{Players: len(d.players), MaxPlayers: 10}, nil
}
func (d *listDriver) Players(context.Context) ([]drivers.Player, error) { return d.players, nil }
func (d *listDriver) SetCredential(string)                              {}
func (d *listDriver) Kick(context.Context, drivers.Identity, string) error {
	return nil
}
func (d *listDriver) Kill(context.Context, drivers.Identity) error { return drivers.ErrNotSupported }
func (d *listDriver) Message(context.Context, drivers.Identity, string) error {
	return drivers.ErrNotSupported
}
func (d *listDriver) Broadcast(context.Context, string) error { return nil }
func (d *listDriver) MovePlayer(context.Context, drivers.Identity, string) error {
	return drivers.ErrNotSupported
}
func (d *listDriver) Ban(_ context.Context, p drivers.Identity, _ string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.banned = append(d.banned, p)
	return drivers.ErrPlayerNotFound // not on: the list bans them at their next join
}
func (d *listDriver) Unban(context.Context, drivers.Identity) error { return drivers.ErrBanNotFound }
func (d *listDriver) Bans(context.Context) ([]drivers.Ban, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []drivers.Ban
	if n := len(d.lists); n > 0 {
		for _, id := range d.lists[n-1] {
			out = append(out, drivers.Ban{Identity: id, By: "hub"})
		}
	}
	return out, nil
}
func (d *listDriver) SetBans(_ context.Context, bans []drivers.Identity) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.setErr != nil {
		return d.setErr
	}
	d.lists = append(d.lists, slices.Clone(bans))
	return nil
}

func (d *listDriver) last() []drivers.Identity {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.lists) == 0 {
		return nil
	}
	return d.lists[len(d.lists)-1]
}

type listRig struct {
	svc   *Service
	mon   *Monitor
	mod   *Moderation
	drv   *listDriver
	bans  *serverstest.BanStore
	audit *serverstest.AuditStore
	owner uuid.UUID
	m     Manifest
}

// newListRig runs one War Dogs server through listDriver: the game is War Dogs only because its
// spec names a driver; nothing here talks to a War Dogs server.
func newListRig(t *testing.T) *listRig {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := prometheus.NewRegistry()
	drv := &listDriver{caps: []string{drivers.CapStatus, drivers.CapPlayers, drivers.CapKick, drivers.CapBans, drivers.CapBan, drivers.CapUnban}}
	registry := drivers.Registry{"wardogs": func(drivers.Target) (drivers.Driver, error) { return drv, nil }}
	cred := filepath.Join(t.TempDir(), "cred")
	r := &listRig{drv: drv, bans: serverstest.NewBanStore(), audit: serverstest.NewAuditStore(), owner: uuid.New()}
	(&monitorRig{cred: cred}).setCredential(t, "secret")
	r.svc = New(serverstest.NewFakeStore(), uuid.New(), registry.Names(), logger)
	r.mon = NewMonitor(r.svc, registry, jobs.New(logger, reg), reg, "test", logger)
	r.mod = NewModeration(r.svc, r.mon, r.audit, r.bans, r.svc.orgID, reg, logger)
	r.mon.SetPrimer(r.mod.Prime)
	r.m = Manifest{
		Games: []GameRef{{ID: "wardogs"}},
		Servers: []Server{{ID: "wd-1", Name: "One", Game: "wardogs", Driver: "wardogs", Location: "x",
			Endpoint: "http://127.0.0.1:1", CredentialFile: cred, Trust: TrustOfficial}},
	}.Normalized()
	ctx := context.Background()
	if _, err := r.svc.Apply(ctx, r.m, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.mon.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.mon.poll(ctx, "wd-1"); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestMonitorPrimesANewDriver(t *testing.T) {
	r := newListRig(t)
	if got := r.drv.lists; len(got) != 1 || len(got[0]) != 0 {
		t.Fatalf("a new driver was handed %v, want one empty list", got)
	}
	ctx := context.Background()
	steam := drivers.Identity{Provider: "steam", Subject: steamA}
	if _, err := r.mod.Do(ctx, Actor{UserID: &r.owner}, Action{Server: "wd-1", Kind: ActionBan, Player: steam, Reason: "cheating"}); err != nil {
		t.Fatal(err)
	}
	// A changed definition gets a fresh driver, which starts with the list.
	r.m.Servers[0].Name = "Renamed"
	if _, err := r.svc.Apply(ctx, r.m, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.mon.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := r.drv.last(); !slices.Equal(got, []drivers.Identity{steam}) {
		t.Errorf("the new driver was primed with %v", got)
	}
}

func TestModerationBansThroughABanList(t *testing.T) {
	r := newListRig(t)
	ctx := context.Background()
	steam := drivers.Identity{Provider: "steam", Subject: steamA}

	if _, err := r.mod.Do(ctx, Actor{UserID: &r.owner}, Action{Server: "wd-1", Kind: ActionBan, Player: steam, Reason: "cheating"}); err != nil {
		t.Fatalf("ban: %v", err)
	}
	if got := r.drv.last(); !slices.Equal(got, []drivers.Identity{steam}) {
		t.Errorf("the driver holds %v after the ban", got)
	}
	// The online ban runs too, best effort: it kicks a player who is on (this one is not).
	if !slices.Equal(r.drv.banned, []drivers.Identity{steam}) {
		t.Errorf("online bans %v, want the one", r.drv.banned)
	}
	if active, _ := r.bans.ActiveBans(ctx, r.svc.orgID, "wd-1"); len(active) != 1 {
		t.Errorf("the hub holds %d bans", len(active))
	}
	entries, err := r.mod.Bans(ctx, "wd-1")
	if err != nil || len(entries) != 1 || entries[0].Hub == nil {
		t.Errorf("Bans = %+v, %v; want the one ban with its hub record", entries, err)
	}

	if _, err := r.mod.Do(ctx, Actor{UserID: &r.owner}, Action{Server: "wd-1", Kind: ActionUnban, Player: steam}); err != nil {
		t.Fatalf("unban: %v", err)
	}
	if got := r.drv.last(); len(got) != 0 {
		t.Errorf("the driver holds %v after the unban", got)
	}
	if _, err := r.mod.Do(ctx, Actor{UserID: &r.owner}, Action{Server: "wd-1", Kind: ActionUnban, Player: steam}); !errors.Is(err, drivers.ErrBanNotFound) {
		t.Errorf("unbanning again: %v, want ErrBanNotFound", err)
	}
}

func TestModerationRollsBackABanTheDriverRefuses(t *testing.T) {
	r := newListRig(t)
	ctx := context.Background()
	r.drv.setErr = errors.New("down")
	steam := drivers.Identity{Provider: "steam", Subject: steamA}
	if _, err := r.mod.Do(ctx, Actor{UserID: &r.owner}, Action{Server: "wd-1", Kind: ActionBan, Player: steam, Reason: "x"}); err == nil {
		t.Fatal("a ban the driver refused succeeded")
	}
	if active, _ := r.bans.ActiveBans(ctx, r.svc.orgID, "wd-1"); len(active) != 0 {
		t.Errorf("the hub kept %d bans the driver never took", len(active))
	}
}

func TestConfigRefusedForANonConfigurer(t *testing.T) {
	r := newListRig(t)
	r.drv.caps = append(r.drv.caps, drivers.CapConfigRead) // claims it, but has no document
	if _, err := r.mod.Config(context.Background(), "wd-1"); !errors.Is(err, drivers.ErrNotSupported) {
		t.Errorf("Config on a driver without a configuration document: %v, want ErrNotSupported", err)
	}
}
