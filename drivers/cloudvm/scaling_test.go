package cloudvm_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gravel-project/gravel/cloud/fake"
	"github.com/gravel-project/gravel/drivers/cloudvm"
)

// The scaling policy (#37) on the fake cloud: the rig's server is a CS2 server whose ladder is
// dedicated-2 (10 players) and dedicated-4 (20), the fake's cpu family, with the defaults: up at
// 80 % for 5 minutes, down at 25 % for 30, a 30-minute cooldown.

var ladder = []cloudvm.Tier{{Size: "dedicated-2", Players: 10}, {Size: "dedicated-4", Players: 20}}

// scaled is a running rig whose server has the policy; edit changes it first (the defaults
// are filled after).
func scaled(t *testing.T, edit func(*cloudvm.Scaling, *cloudvm.ServerSpec)) *rig {
	t.Helper()
	r := newRig(t)
	r.src.edit(func(s *cloudvm.ServerSpec) {
		sc := cloudvm.Scaling{Ladder: ladder}
		if edit != nil {
			edit(&sc, s)
		}
		sc = sc.WithDefaults()
		s.Scaling = &sc
	})
	if err := r.m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.up()
	return r
}

// players puts n humans on the server and lets the hub read them.
func (r *rig) players(n int) {
	r.t.Helper()
	rows := make([]cloudvm.GamePlayer, n)
	for i := range rows {
		rows[i] = cloudvm.GamePlayer{UserID: i + 2}
	}
	r.game.SetPlayers(rows...)
	if _, err := r.m.Status(context.Background(), serverID); err != nil {
		r.t.Fatal(err)
	}
}

// step moves the clock, reads the players again and ticks.
func (r *rig) step(d time.Duration) {
	r.t.Helper()
	r.advance(d)
	_, _ = r.m.Status(context.Background(), serverID) // a host that is not running has no status; the tick still runs
	if err := r.tick(); err != nil {
		r.t.Fatal(err)
	}
}

// boundary has the game announce one in its log.
func (r *rig) boundary(kind string) {
	body := "boundary " + kind + " " + r.clock().String()
	r.game.OnBoundary(body, kind)
	r.m.Lines(serverID, []byte(body))
}

func (r *rig) warnings() int {
	n := 0
	for _, c := range r.game.Commands() {
		if strings.HasPrefix(c, "say Heads up") {
			n++
		}
	}
	return n
}

func (r *rig) eventsWith(step string) []cloudvm.Event {
	var out []cloudvm.Event
	for _, e := range r.st.Events() {
		if e.Step == step {
			out = append(out, e)
		}
	}
	return out
}

func TestScaleUpWaitsForTheMatchToEnd(t *testing.T) {
	r := scaled(t, nil)
	r.players(8)
	r.step(4*time.Minute + 59*time.Second)
	if h := r.host(); h.PendingSize != "" {
		t.Fatalf("decided after %s: %+v", 4*time.Minute+59*time.Second, h)
	}
	r.step(time.Second)
	h := r.host()
	if h.PendingSize != "dedicated-4" || h.State != cloudvm.StateRunning || h.Size != "dedicated-2" {
		t.Fatalf("after 5 minutes at 8 of 10: %+v", h)
	}
	if ev := r.eventsWith(cloudvm.DetailScale); len(ev) != 1 || !strings.Contains(ev[0].Detail, "8 of 10 players for 5m0s") {
		t.Errorf("decision events %+v: one, with its inputs", ev)
	}
	if r.warnings() != 1 || r.counter("gravel_cloudvm_scale_pending", map[string]string{"direction": "up"}) != 1 {
		t.Errorf("%d warnings, pending gauge %v: warned once, the gauge up", r.warnings(), r.counter("gravel_cloudvm_scale_pending", map[string]string{"direction": "up"}))
	}
	r.step(10 * time.Minute)
	if h := r.host(); h.State != cloudvm.StateRunning || h.Size != "dedicated-2" || r.warnings() != 1 {
		t.Fatalf("mid-match: %s at %s, %d warnings: no move and no second warning before the boundary", h.State, h.Size, r.warnings())
	}
	r.boundary(cloudvm.BoundaryMatchEnd)
	r.step(time.Second)
	h = r.host()
	if h.State != cloudvm.StateRunning || h.Size != "dedicated-4" || h.PendingSize != "" || h.LastResizeAt.IsZero() {
		t.Fatalf("after the match ended: %+v", h)
	}
	if !strings.Contains(r.env(h.InstanceAddress), "CS2_MAXPLAYERS=20\n") {
		t.Errorf("the new rung's players did not reach the game:\n%s", r.env(h.InstanceAddress))
	}
	if r.counter("gravel_cloudvm_resizes_total", map[string]string{"direction": "up", "result": "ok"}) != 1 ||
		r.counter("gravel_cloudvm_scale_decisions_total", map[string]string{"direction": "up", "outcome": "executed"}) != 1 ||
		r.counter("gravel_cloudvm_tier_players", nil) != 20 {
		t.Error("metrics: one resize up, one decision executed, the tier at 20")
	}
}

func TestScaleUpAtAMapChange(t *testing.T) {
	r := scaled(t, nil)
	r.players(10)
	r.step(5 * time.Minute)
	if r.host().PendingSize == "" {
		t.Fatal("a full rung did not decide")
	}
	r.step(time.Minute)
	r.boundary(cloudvm.BoundaryMapChange)
	r.step(time.Second)
	if h := r.host(); h.Size != "dedicated-4" {
		t.Errorf("after the map change: %s", h.Size)
	}
}

func TestABoundaryBeforeTheDecisionDoesNotCount(t *testing.T) {
	r := scaled(t, nil)
	r.players(9)
	r.step(4 * time.Minute)
	r.boundary(cloudvm.BoundaryMatchEnd)
	r.step(time.Minute) // decided now, a minute after the boundary
	if h := r.host(); h.PendingSize != "dedicated-4" || h.Size != "dedicated-2" {
		t.Errorf("%+v: the players were warned after that boundary, so the move waits for the next", h)
	}
}

func TestHysteresisDown(t *testing.T) {
	r := scaled(t, func(_ *cloudvm.Scaling, s *cloudvm.ServerSpec) { s.ParkAfter = 0 })
	ctx := context.Background()
	if _, err := r.m.Resize(ctx, serverID, "dedicated-4", false, false, 0); err != nil {
		t.Fatal(err)
	}
	r.step(time.Second)
	r.players(3) // 15 % of 20
	r.step(10 * time.Minute)
	if h := r.host(); h.PendingSize != "" || h.Size != "dedicated-4" {
		t.Fatalf("10 minutes at 3 of 20: %+v; down waits 30", h)
	}
	r.step(20 * time.Minute)
	if h := r.host(); h.PendingSize != "dedicated-2" || r.warnings() != 1 {
		t.Fatalf("30 minutes at 3 of 20: pending %q, %d warnings", h.PendingSize, r.warnings())
	}
	r.boundary(cloudvm.BoundaryMatchEnd)
	r.step(time.Second)
	h := r.host()
	if h.Size != "dedicated-2" || !strings.Contains(r.env(h.InstanceAddress), "CS2_MAXPLAYERS=10\n") {
		t.Errorf("after the boundary: %s", h.Size)
	}
	if r.counter("gravel_cloudvm_resizes_total", map[string]string{"direction": "down", "result": "ok"}) != 1 {
		t.Error("the resize down was not counted")
	}
}

func TestAFullishRoomBetweenTheThresholdsStays(t *testing.T) {
	r := scaled(t, nil)
	r.players(5) // 50 %: neither up nor down
	for range 12 {
		r.step(5 * time.Minute)
	}
	if h := r.host(); h.PendingSize != "" || h.Size != "dedicated-2" || len(r.eventsWith(cloudvm.DetailScale)) != 0 {
		t.Errorf("an hour at 5 of 10: %+v", h)
	}
}

func TestAnEmptyServerIsABoundary(t *testing.T) {
	r := scaled(t, func(_ *cloudvm.Scaling, s *cloudvm.ServerSpec) { s.ParkAfter = 0 })
	if _, err := r.m.Resize(context.Background(), serverID, "dedicated-4", false, false, 0); err != nil {
		t.Fatal(err)
	}
	r.step(time.Second)
	r.players(0)
	r.step(30 * time.Minute)
	if h := r.host(); h.Size != "dedicated-2" || h.PendingSize != "" || r.warnings() != 0 {
		t.Errorf("30 minutes empty on the big rung: %s, pending %q, %d warnings (nobody to warn)", h.Size, h.PendingSize, r.warnings())
	}
}

func TestParkingWinsOverScalingDown(t *testing.T) {
	r := scaled(t, nil) // park_after 30m, down_for 30m
	if _, err := r.m.Resize(context.Background(), serverID, "dedicated-4", false, false, 0); err != nil {
		t.Fatal(err)
	}
	r.step(time.Second)
	r.players(0)
	r.step(30 * time.Minute)
	if h := r.host(); h.State != cloudvm.StateParked {
		t.Errorf("30 minutes empty with park_after 30m: %s; parking beats a resize nobody needs", h.State)
	}
}

func TestABoundaryThatNeverComes(t *testing.T) {
	r := scaled(t, nil)
	r.players(9)
	r.step(5 * time.Minute)
	for range 60 {
		r.step(time.Minute)
	}
	if h := r.host(); h.PendingSize != "dedicated-4" || h.Size != "dedicated-2" || r.warnings() != 1 {
		t.Errorf("an hour-long match: %+v, %d warnings; still waiting, warned once", h, r.warnings())
	}
}

func TestAPendingMoveIsDroppedWhenTheRoomEmpties(t *testing.T) {
	r := scaled(t, nil)
	r.players(9)
	r.step(5 * time.Minute)
	r.players(2)
	r.step(time.Second)
	if h := r.host(); h.PendingSize != "" {
		t.Fatalf("still pending at 2 of 10: %+v", h)
	}
	if ev := r.eventsWith(cloudvm.DetailScaleCancelled); len(ev) != 1 || !strings.Contains(ev[0].Detail, "2 of 10") {
		t.Errorf("cancel events %+v", ev)
	}
	if r.counter("gravel_cloudvm_scale_decisions_total", map[string]string{"outcome": "cancelled"}) != 1 {
		t.Error("the cancel was not counted")
	}
}

func TestAPinnedSizeStaysAndUnpinningLetsThePolicyDecide(t *testing.T) {
	r := scaled(t, nil)
	ctx := context.Background()
	if _, err := r.m.Resize(ctx, serverID, "dedicated-2", false, true, 0); err != nil {
		t.Fatalf("pin the size it is at: %v", err)
	}
	r.players(10)
	r.step(20 * time.Minute)
	if h := r.host(); h.PinnedSize != "dedicated-2" || h.PendingSize != "" || h.Size != "dedicated-2" {
		t.Fatalf("pinned and full: %+v", h)
	}
	if _, err := r.m.Unpin(ctx, serverID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Unpin(ctx, serverID, 0); !errors.Is(err, cloudvm.ErrInvalid) {
		t.Errorf("unpin with nothing pinned: %v", err)
	}
	r.step(5 * time.Minute)
	if h := r.host(); h.PendingSize != "dedicated-4" {
		t.Errorf("unpinned and full for 5 minutes: %+v", h)
	}
}

func TestAPinnedSizeComesBackAfterAPark(t *testing.T) {
	r := scaled(t, nil)
	ctx := context.Background()
	if _, err := r.m.Resize(ctx, serverID, "dedicated-4", false, true, 0); err != nil {
		t.Fatal(err)
	}
	r.step(time.Second)
	if _, err := r.m.SetDesired(ctx, serverID, cloudvm.DesiredParked, 0); err != nil {
		t.Fatal(err)
	}
	r.step(time.Second)
	if _, err := r.m.SetDesired(ctx, serverID, cloudvm.DesiredRunning, 0); err != nil {
		t.Fatal(err)
	}
	r.advance(time.Minute)
	r.up()
	r.players(0)
	r.step(time.Second)
	h := r.host()
	if h.Size != "dedicated-4" || h.DiskGiB != 25 {
		t.Errorf("unparked: %s with a %d GiB disk; made at the server's size (its 25 GiB disk keeps the way down), then resized to the pin", h.Size, h.DiskGiB)
	}
}

func TestTheCooldownHoldsTheNextMove(t *testing.T) {
	r := scaled(t, func(sc *cloudvm.Scaling, s *cloudvm.ServerSpec) { sc.Cooldown = time.Hour; s.ParkAfter = 0 })
	r.players(10)
	r.step(5 * time.Minute)
	r.boundary(cloudvm.BoundaryMatchEnd)
	r.step(time.Second)
	if r.host().Size != "dedicated-4" {
		t.Fatal("no move up")
	}
	r.players(1)
	r.step(31 * time.Minute)
	if h := r.host(); h.PendingSize != "" {
		t.Fatalf("31 minutes after a resize with a 1-hour cooldown: %+v", h)
	}
	r.step(30 * time.Minute)
	if h := r.host(); h.PendingSize != "dedicated-2" {
		t.Errorf("past the cooldown: %+v", h)
	}
}

func TestAFailedResizeStaysOnItsRung(t *testing.T) {
	r := scaled(t, nil)
	r.players(9)
	r.step(5 * time.Minute)
	r.p.FailOperationNext("ResizeInstance")
	r.boundary(cloudvm.BoundaryMatchEnd)
	r.step(time.Second)
	h := r.host()
	if h.State != cloudvm.StateRunning || h.Size != "dedicated-2" || h.LastResizeAt.IsZero() {
		t.Fatalf("after a failed resize: %+v", h)
	}
	if r.counter("gravel_cloudvm_resizes_total", map[string]string{"direction": "up", "result": "failed"}) != 1 {
		t.Error("the failed resize was not counted")
	}
	r.step(10 * time.Minute)
	if h := r.host(); h.PendingSize != "" {
		t.Errorf("decided again inside the cooldown: %+v", h)
	}
}

func TestASizeOffTheLadderWaits(t *testing.T) {
	r := scaled(t, func(sc *cloudvm.Scaling, _ *cloudvm.ServerSpec) {
		sc.Ladder = []cloudvm.Tier{{Size: "dedicated-4", Players: 20}, {Size: "dedicated-8", Players: 40}}
	})
	r.players(20)
	r.step(10 * time.Minute)
	if h := r.host(); h.PendingSize != "" || h.Size != "dedicated-2" {
		t.Errorf("off the ladder: %+v", h)
	}
}

func TestRenderEnvWritesTheRungsPlayers(t *testing.T) {
	s := newRig(t).src.specs[0]
	sc := cloudvm.Scaling{Ladder: ladder}.WithDefaults()
	s.Scaling = &sc
	for size, want := range map[string]string{"dedicated-2": "CS2_MAXPLAYERS=10\n", "dedicated-4": "CS2_MAXPLAYERS=20\n", "medium": "CS2_MAXPLAYERS=10\n"} {
		env, err := cloudvm.RenderEnv(s, size)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(env), want) {
			t.Errorf("%s: want %q (off the ladder: the game's default)", size, want)
		}
	}
}

func TestCheckLadder(t *testing.T) {
	p := fake.New()
	sizes, err := p.Sizes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		start  string
		region string
		ladder []cloudvm.Tier
		want   string
	}{
		{"walkable both ways", "dedicated-2", "fake-1", ladder, ""},
		{"made at the top rung: its disk can't come down", "dedicated-4", "fake-1", ladder, "back to dedicated-2"},
		{"another family", "dedicated-2", "fake-1", []cloudvm.Tier{{Size: "dedicated-2", Players: 10}, {Size: "large", Players: 20}}, "family"},
		{"not a size", "dedicated-2", "fake-1", []cloudvm.Tier{{Size: "dedicated-2", Players: 10}, {Size: "huge-9", Players: 20}}, "not a size this cloud offers"},
		{"not in the region", "dedicated-2", "nowhere-1", ladder, "not offered in nowhere-1"},
	} {
		err := cloudvm.CheckLadder(sizes, p.CanResize, c.region, c.start, c.ladder)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: %v, want it to say %q", c.name, err, c.want)
		}
	}
}
