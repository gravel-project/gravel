package cloudvm_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gravel-project/gravel/drivers/cloudvm"
)

// Expected population (#172) on the scaling rig: dedicated-2 (10 players) and dedicated-4 (20),
// up at 80 % for 5 minutes, park_after 30m.

// expect has the host expect n players for d from now.
func (r *rig) expect(n int, d time.Duration) cloudvm.Host {
	r.t.Helper()
	h, err := r.m.SetExpectation(context.Background(), serverID, n, r.clock().Add(d), "PUG 3 filled", 0)
	if err != nil {
		r.t.Fatal(err)
	}
	return h
}

func TestAnExpectedCrowdMovesAnEmptyServerUpAtOnce(t *testing.T) {
	r := scaled(t, nil)
	r.players(0)
	if h := r.expect(10, 2*time.Hour); h.ExpectedPlayers != 10 || h.ExpectedReason != "PUG 3 filled" {
		t.Fatalf("the expectation: %+v", h)
	}
	r.step(time.Second)
	h := r.host()
	if h.Size != "dedicated-4" || h.State != cloudvm.StateRunning {
		t.Fatalf("an empty server expecting 10 on a 10-player rung: %s at %s; it moves up at once", h.State, h.Size)
	}
	ev := r.eventsWith(cloudvm.DetailScale)
	if len(ev) != 1 || !strings.Contains(ev[0].Detail, "expected 10 until") || !strings.Contains(ev[0].Detail, "PUG 3 filled") {
		t.Errorf("decision events %+v: one, saying where it came from", ev)
	}
	if r.warnings() != 0 {
		t.Error("nobody on was warned")
	}
	if e := r.eventsWith(cloudvm.DetailExpect); len(e) != 1 || !strings.Contains(e[0].Detail, "expected 10 until") {
		t.Errorf("expect events %+v", e)
	}
	if r.counter("gravel_cloudvm_expected_players", map[string]string{"server": serverID}) != 10 {
		t.Error("the expected players gauge")
	}
}

func TestAnExpectedCrowdWaitsForTheBoundaryWithPlayersOn(t *testing.T) {
	r := scaled(t, nil)
	r.players(3)
	r.expect(10, time.Hour)
	r.step(time.Second)
	if h := r.host(); h.PendingSize != "dedicated-4" || h.Size != "dedicated-2" || r.warnings() != 1 {
		t.Fatalf("3 on, 10 expected: %+v, %d warnings; decided at once (no 5-minute wait), warned, not moved", h, r.warnings())
	}
	r.step(10 * time.Minute)
	if h := r.host(); h.Size != "dedicated-2" {
		t.Fatalf("moved mid-match: %s", h.Size)
	}
	r.boundary(cloudvm.BoundaryMapChange)
	r.step(time.Second)
	if h := r.host(); h.Size != "dedicated-4" || h.PendingSize != "" {
		t.Errorf("after the map change: %+v", h)
	}
}

func TestAnExpectationBelowTheLiveRoomChangesNothing(t *testing.T) {
	r := scaled(t, nil)
	r.players(8)
	r.expect(4, time.Hour)
	r.step(time.Second)
	if h := r.host(); h.PendingSize != "" {
		t.Fatalf("4 expected with 8 on decided at once: %+v", h)
	}
	r.step(5 * time.Minute)
	if h := r.host(); h.PendingSize != "dedicated-4" || !strings.Contains(r.eventsWith(cloudvm.DetailScale)[0].Detail, "8 of 10 players for 5m1s") {
		t.Errorf("the live room decides as before: %+v", h)
	}
}

func TestAnExpectedCrowdHoldsTheHostUpAndOffTheIdlePark(t *testing.T) {
	r := scaled(t, nil)
	r.players(0)
	r.expect(10, 45*time.Minute)
	r.step(time.Second) // up to dedicated-4 at once
	for range 40 {
		r.step(time.Minute)
	}
	h := r.host()
	if h.State != cloudvm.StateRunning || h.Size != "dedicated-4" || h.PendingSize != "" {
		t.Fatalf("40 minutes empty while 10 are expected: %s at %s, pending %q; no park and no move down", h.State, h.Size, h.PendingSize)
	}
	r.step(5 * time.Minute) // past the expectation: cleared, and the host has been empty long enough
	h = r.host()
	if h.ExpectedPlayers != 0 || !h.ExpectedUntil.IsZero() {
		t.Errorf("a passed expectation stays: %+v", h)
	}
	if e := r.eventsWith(cloudvm.DetailExpect); len(e) != 2 || !strings.HasPrefix(e[1].Detail, "passed: expected 10") {
		t.Errorf("expect events %+v: set, then passed", e)
	}
	r.step(time.Second)
	if h := r.host(); h.State != cloudvm.StateParked {
		t.Errorf("empty past park_after once nobody is expected: %s", h.State)
	}
}

func TestAnExpectationHoldsTheIdleParkWithoutAPolicy(t *testing.T) {
	r := newRig(t) // no scaling policy
	r.up()
	r.players(0)
	r.expect(10, time.Hour)
	r.step(31 * time.Minute)
	if h := r.host(); h.State != cloudvm.StateRunning || h.Size != "dedicated-2" {
		t.Fatalf("31 minutes empty with 10 expected and no policy: %s at %s", h.State, h.Size)
	}
	r.step(30 * time.Minute)
	r.step(time.Second)
	if h := r.host(); h.State != cloudvm.StateParked {
		t.Errorf("after the expectation passed: %s", h.State)
	}
}

func TestAPinBeatsAnExpectation(t *testing.T) {
	r := scaled(t, nil)
	if _, err := r.m.Resize(context.Background(), serverID, "dedicated-2", false, true, 0); err != nil {
		t.Fatal(err)
	}
	r.players(0)
	r.expect(10, time.Hour)
	r.step(time.Second)
	if h := r.host(); h.Size != "dedicated-2" || h.PendingSize != "" {
		t.Errorf("a pinned host moved for an expectation: %+v", h)
	}
}

func TestAnExpectedCrowdIsNotHeldByTheCooldown(t *testing.T) {
	r := scaled(t, nil) // cooldown 30m
	ctx := context.Background()
	r.players(0)
	if _, err := r.m.Resize(ctx, serverID, "dedicated-4", false, false, 0); err != nil {
		t.Fatal(err)
	}
	r.step(time.Second)
	if _, err := r.m.Resize(ctx, serverID, "dedicated-2", false, false, 0); err != nil {
		t.Fatal(err)
	}
	r.step(time.Second)
	if h := r.host(); h.Size != "dedicated-2" || h.LastResizeAt.IsZero() {
		t.Fatalf("setup: %+v", h)
	}
	r.expect(10, time.Hour)
	r.step(time.Second)
	if h := r.host(); h.Size != "dedicated-4" {
		t.Errorf("an announced crowd a minute after a resize: %s; the cooldown guards against noise, not announcements", h.Size)
	}
}

func TestSetExpectationChecks(t *testing.T) {
	r := scaled(t, nil)
	ctx := context.Background()
	now := r.clock()
	for name, c := range map[string]struct {
		players int
		until   time.Time
		reason  string
	}{
		"negative":        {-1, now.Add(time.Hour), ""},
		"passed":          {10, now.Add(-time.Second), ""},
		"now":             {10, now, ""},
		"too far":         {10, now.Add(cloudvm.MaxExpectation + time.Minute), ""},
		"two lines":       {10, now.Add(time.Hour), "a\nb"},
		"a long reason":   {10, now.Add(time.Hour), strings.Repeat("x", cloudvm.MaxExpectationReason+1)},
		"clear when none": {0, time.Time{}, ""},
	} {
		if _, err := r.m.SetExpectation(ctx, serverID, c.players, c.until, c.reason, 0); !errors.Is(err, cloudvm.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if _, err := r.m.SetExpectation(ctx, "nope", 10, now.Add(time.Hour), "", 0); !errors.Is(err, cloudvm.ErrNoSuchHost) {
		t.Errorf("unknown host: %v", err)
	}
	if h := r.expect(50, cloudvm.MaxExpectation); h.ExpectedPlayers != 20 {
		t.Errorf("50 on a ladder topping at 20: %d; clamped", h.ExpectedPlayers)
	}
	h, err := r.m.SetExpectation(ctx, serverID, 0, time.Time{}, "", 0)
	if err != nil || h.ExpectedPlayers != 0 || !h.ExpectedUntil.IsZero() || h.ExpectedReason != "" {
		t.Errorf("clear: %+v %v", h, err)
	}
	if e := r.eventsWith(cloudvm.DetailExpect); len(e) != 2 || e[1].Detail != "cleared" {
		t.Errorf("expect events %+v", e)
	}
}
