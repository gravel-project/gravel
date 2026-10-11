package cloudvm_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gravel-project/gravel/drivers"
	"github.com/gravel-project/gravel/drivers/cloudvm"
)

func (r *rig) driver() drivers.Driver {
	r.t.Helper()
	d, err := r.m.Factory()(drivers.Target{ServerID: serverID, Game: "cs2"})
	if err != nil {
		r.t.Fatal(err)
	}
	return d
}

func TestDriverWhileTheHostIsDown(t *testing.T) {
	r := newRig(t)
	d := r.driver()
	ctx := context.Background()
	var hs *drivers.HostStateError
	if _, err := d.Build(ctx); !errors.As(err, &hs) || hs.Phase != drivers.HostParked {
		t.Errorf("absent host: %v", err)
	}
	_ = r.tick() // creating → booting, waits there
	if _, err := d.Status(ctx); !errors.As(err, &hs) || hs.Phase != drivers.HostProvisioning {
		t.Errorf("booting host: %v", err)
	}
	if _, err := r.m.Factory()(drivers.Target{}); err == nil {
		t.Error("a driver without a server id")
	}
}

func TestDriverWhileRunning(t *testing.T) {
	r := newRig(t)
	h := r.up()
	d := r.driver()
	ctx := context.Background()
	steam := drivers.Identity{Provider: "steam", Subject: "76561197960287930"}
	r.game.SetPlayers(cloudvm.GamePlayer{UserID: 2, Name: "One", Identity: steam, Team: "CT", Kills: 3, PingMs: 40},
		cloudvm.GamePlayer{UserID: 3, Name: "Not named by the log yet"})

	caps, err := d.Capabilities(ctx)
	if err != nil || !slices.Equal(caps, cloudvm.Capabilities) || slices.Contains(caps, drivers.CapConfigWrite) {
		t.Errorf("capabilities %v %v", caps, err)
	}
	r.advance(4 * time.Second) // past the driver's status reuse
	st, err := d.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Players != 2 || st.MaxPlayers != 10 || st.Map != "de_test" || st.Join.Method != drivers.JoinByAddress ||
		st.Join.Value != h.Address+":27015" || !strings.Contains(st.Join.Hint, "connect "+h.Address+":27015") {
		t.Errorf("status %+v", st)
	}
	players, err := d.Players(ctx)
	if err != nil || len(players) != 2 || players[0].Identity != steam || players[0].Team != "CT" || players[1].Identity.Subject != "" {
		t.Errorf("players %+v %v", players, err)
	}
	if err := d.Kick(ctx, steam, "bye"); err != nil {
		t.Fatal(err)
	}
	if err := d.Kick(ctx, drivers.Identity{Provider: "steam", Subject: "1"}, "x"); !errors.Is(err, drivers.ErrPlayerNotFound) {
		t.Errorf("kicking someone not on: %v", err)
	}
	if err := d.Broadcast(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	if got := r.game.Commands(); !slices.Contains(got, "kick 2 bye") || !slices.Contains(got, "say hello") {
		t.Errorf("commands %v", got)
	}
	for name, err := range map[string]error{
		"kill":    d.Kill(ctx, steam),
		"message": d.Message(ctx, steam, "x"),
		"move":    d.MovePlayer(ctx, steam, "CT"),
	} {
		if !errors.Is(err, drivers.ErrNotSupported) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := d.Unban(ctx, steam); !errors.Is(err, drivers.ErrBanNotFound) {
		t.Errorf("unban: %v", err)
	}
	bl := d.(drivers.BanList)
	if err := bl.SetBans(ctx, []drivers.Identity{steam}); err != nil {
		t.Fatal(err)
	}
	if bans, _ := d.Bans(ctx); len(bans) != 1 || bans[0].Identity != steam || bans[0].By != "hub" {
		t.Errorf("bans %+v", bans)
	}
}
func TestBansAreKickedOnConnect(t *testing.T) {
	r := newRig(t)
	r.up()
	steam := drivers.Identity{Provider: "steam", Subject: "76561197960287930"}
	d, err := r.m.Factory()(drivers.Target{ServerID: serverID, Game: "cs2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.(drivers.BanList).SetBans(context.Background(), []drivers.Identity{steam}); err != nil {
		t.Fatal(err)
	}
	r.game.OnLog("joined", cloudvm.GamePlayer{UserID: 5, Identity: steam}, cloudvm.GamePlayer{UserID: 6, Identity: drivers.Identity{Provider: "steam", Subject: "1"}})
	r.m.Lines(serverID, []byte("joined"))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !slices.ContainsFunc(r.game.Commands(), func(c string) bool { return strings.HasPrefix(c, "kick 5 ") }) {
		time.Sleep(10 * time.Millisecond)
	}
	got := r.game.Commands()
	if !slices.ContainsFunc(got, func(c string) bool { return strings.HasPrefix(c, "kick 5 ") }) || slices.ContainsFunc(got, func(c string) bool { return strings.HasPrefix(c, "kick 6 ") }) {
		t.Errorf("commands %v: kick the banned player only", got)
	}
}
