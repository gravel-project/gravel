package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/gravel-project/gravel/internal/store"
)

// gapStore is the store as the gap detectors see it: the last batch before a time, the minutes
// anyone was on, and the events.
type gapStore struct {
	memStore
	mu      sync.Mutex
	last    map[string]time.Time
	players map[string][]time.Time // minutes anyone was on
	fail    bool
}

func (g *gapStore) LastIngestBatch(_ context.Context, _ uuid.UUID, server string, before time.Time) (time.Time, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.fail {
		return time.Time{}, false, errors.New("database away")
	}
	at, ok := g.last[server]
	if !ok || !at.Before(before) {
		return time.Time{}, false, nil
	}
	return at, true, nil
}

func (g *gapStore) HadPlayers(_ context.Context, _ uuid.UUID, server string, at time.Time) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	m := at.Truncate(time.Minute)
	for _, x := range g.players[server] {
		if x.Equal(m) || x.Equal(m.Add(-time.Minute)) {
			return true, nil
		}
	}
	return false, nil
}

type gapRig struct {
	st      *gapStore
	g       *Gaps
	now     time.Time
	stopped time.Time // when the previous run was last known to run
	players map[string]int
	down    map[string]bool
}

// newGapRig is a hub that starts at 01:00, eight minutes after its previous run stopped.
func newGapRig(t *testing.T) *gapRig { return newGapRigAway(t, true) }

func newGapRigAway(t *testing.T, known bool) *gapRig {
	t.Helper()
	start := time.Date(2026, 10, 21, 1, 0, 0, 0, time.UTC)
	r := &gapRig{st: &gapStore{last: map[string]time.Time{}, players: map[string][]time.Time{}}, now: start,
		stopped: start.Add(-8*time.Minute + 20*time.Second), players: map[string]int{}, down: map[string]bool{}}
	servers := func(context.Context) ([]FeedServer, error) {
		return []FeedServer{{ID: "wd-1", Game: "wardogs", Trust: "official"}, {ID: "wd-2", Game: "wardogs", Trust: "community"}}, nil
	}
	occupancy := func(server string) (int, bool) { return r.players[server], !r.down[server] }
	away := Away{From: r.stopped, To: start, Known: known}
	if !known {
		away.From = time.Time{}
	}
	r.g = NewGaps(r.st, uuid.New(), servers, occupancy, away, prometheus.NewRegistry(), quiet())
	r.g.now = func() time.Time { return r.now }
	return r
}

func (r *gapRig) gaps() []store.StatsEvent {
	r.st.memStore.mu.Lock()
	defer r.st.memStore.mu.Unlock()
	return append([]store.StatsEvent(nil), r.st.events...)
}

func gapOf(t *testing.T, e store.StatsEvent) (reason string, to time.Time) {
	t.Helper()
	var ext struct {
		Reason string    `json:"reason"`
		To     time.Time `json:"to"`
	}
	if err := json.Unmarshal(e.Extensions, &ext); err != nil {
		t.Fatal(err)
	}
	return ext.Reason, ext.To
}

// TestHubDown: at start, a server with players on as the hub stopped gets a gap from the stop to
// the start; one that was empty then gets none, even when its feed last posted with players on
// hours before (a feed that posts only when it has events: Hidden Token Gaming's 1029-minute gap
// of 2026-10-19, with nobody on); one whose feed never posted gets none.
func TestHubDown(t *testing.T) {
	r := newGapRig(t)
	r.st.last["wd-1"] = r.now.Add(-40 * time.Minute)
	r.st.players["wd-1"] = []time.Time{r.stopped.Truncate(time.Minute)}
	lastBatch := r.now.Add(-17 * time.Hour)
	r.st.last["wd-2"] = lastBatch // players on then, nobody on as the hub stopped
	r.st.players["wd-2"] = []time.Time{lastBatch.Truncate(time.Minute)}
	ctx := context.Background()
	if err := r.g.Run(ctx); err != nil {
		t.Fatal(err)
	}
	got := r.gaps()
	if len(got) != 1 {
		t.Fatalf("gaps = %+v", got)
	}
	e := got[0]
	reason, to := gapOf(t, e)
	if e.ServerID != "wd-1" || e.Kind != KindGap || e.Source != SourceServerAdapter || e.Trust != "official" || e.GameID != "wardogs" ||
		!e.ObservedAt.Equal(r.stopped) || reason != GapHubDown || !to.Equal(r.now) || e.BatchID != 0 || e.Player.Subject != "" {
		t.Errorf("gap = %+v (%s to %s)", e, reason, to)
	}
	if n := testutil.ToFloat64(r.g.gaps.WithLabelValues("wd-1", GapHubDown)); n != 1 {
		t.Errorf("counted %v", n)
	}
	// It is decided once.
	if err := r.g.Run(ctx); err != nil || len(r.gaps()) != 1 {
		t.Errorf("a second run: %d gaps, %v", len(r.gaps()), err)
	}
	// The same gap found again (a hub that restarts before its run was recorded alive again) is
	// stored once.
	again := newGapRig(t)
	again.st = r.st
	again.g.st = r.st
	if err := again.g.Run(ctx); err != nil || len(r.gaps()) != 1 {
		t.Errorf("found again: %d gaps, %v", len(r.gaps()), err)
	}
	// A server whose feed never posted lost nothing.
	never := newGapRig(t)
	never.st.players["wd-1"] = []time.Time{never.stopped.Truncate(time.Minute)}
	if err := never.g.Run(ctx); err != nil || len(never.gaps()) != 0 {
		t.Errorf("a feed that never posted: %d gaps, %v", len(never.gaps()), err)
	}
}

// TestHubDownUnknown: a hub with no record of its previous run cannot say when it was away, so it
// records no hub_down gap, whoever was on.
func TestHubDownUnknown(t *testing.T) {
	r := newGapRigAway(t, false)
	r.st.last["wd-1"] = r.now.Add(-time.Minute)
	r.st.players["wd-1"] = []time.Time{r.now.Add(-time.Minute).Truncate(time.Minute), r.stopped.Truncate(time.Minute)}
	if err := r.g.Run(context.Background()); err != nil || len(r.gaps()) != 0 {
		t.Errorf("unknown: %d gaps, %v", len(r.gaps()), err)
	}
}

func TestHubDownRetriesTheStore(t *testing.T) {
	r := newGapRig(t)
	r.st.last["wd-1"] = r.now.Add(-time.Hour)
	r.st.players["wd-1"] = []time.Time{r.stopped.Truncate(time.Minute)}
	r.st.fail = true
	if err := r.g.Run(context.Background()); err == nil {
		t.Fatal("no error")
	}
	r.st.fail = false
	if err := r.g.Run(context.Background()); err != nil || len(r.gaps()) != 1 {
		t.Errorf("after the store came back: %d gaps, %v", len(r.gaps()), err)
	}
}

// TestSilence: ten minutes with no batch while ten or more are on opens a silence, the next batch
// ends it and writes the gap; a server with fewer on (idling below the match's minimum posts
// nothing), an unreachable one, or one whose feed never posted, is quiet for no reason worth a gap.
func TestSilence(t *testing.T) {
	r := newGapRig(t)
	ctx := context.Background()
	if err := r.g.Run(ctx); err != nil { // nothing posted before the start
		t.Fatal(err)
	}
	r.g.Stored("wd-1", r.now.Add(time.Minute))
	r.g.Stored("wd-2", r.now.Add(time.Minute))
	r.g.Stored("wd-1", r.now) // an older one changes nothing
	r.players["wd-1"], r.players["wd-2"] = SilencePlayers, SilencePlayers-1
	r.now = r.now.Add(time.Minute + SilenceAfter - time.Second)
	if err := r.g.Run(ctx); err != nil || len(r.g.silent) != 0 {
		t.Fatalf("before ten minutes: %v %v", r.g.silent, err)
	}
	r.now = r.now.Add(time.Second)
	if err := r.g.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if from := r.g.silent["wd-1"]; !from.Equal(r.now.Add(-SilenceAfter)) || len(r.g.silent) != 1 {
		t.Fatalf("silent = %v", r.g.silent)
	}
	if len(r.gaps()) != 0 {
		t.Fatal("a gap before the silence ended")
	}
	// Still silent: nothing more.
	r.now = r.now.Add(20 * time.Minute)
	r.players["wd-1"] = 0
	if err := r.g.Run(ctx); err != nil || len(r.gaps()) != 0 {
		t.Fatalf("still silent: %d gaps, %v", len(r.gaps()), err)
	}
	from := r.g.silent["wd-1"]
	r.g.Stored("wd-1", r.now)
	if err := r.g.Run(ctx); err != nil {
		t.Fatal(err)
	}
	got := r.gaps()
	if len(got) != 1 {
		t.Fatalf("gaps = %+v", got)
	}
	if reason, to := gapOf(t, got[0]); reason != GapSilence || !got[0].ObservedAt.Equal(from) || !to.Equal(r.now) || got[0].ServerID != "wd-1" {
		t.Errorf("gap = %+v (%s to %s)", got[0], reason, to)
	}
	if len(r.g.silent) != 0 {
		t.Errorf("silent after the gap = %v", r.g.silent)
	}
	// An unreachable server's silence is the server's, not the feed's.
	r.now = r.now.Add(time.Hour)
	r.players["wd-1"], r.down["wd-1"] = 20, true
	if err := r.g.Run(ctx); err != nil || len(r.g.silent) != 0 {
		t.Errorf("unreachable: %v %v", r.g.silent, err)
	}
	if n := testutil.ToFloat64(r.g.gaps.WithLabelValues("wd-1", GapSilence)); n != 1 {
		t.Errorf("counted %v", n)
	}
}
