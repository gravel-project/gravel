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
	players map[string]int
	down    map[string]bool
}

func newGapRig(t *testing.T) *gapRig {
	t.Helper()
	start := time.Date(2026, 10, 21, 1, 0, 0, 0, time.UTC)
	r := &gapRig{st: &gapStore{last: map[string]time.Time{}, players: map[string][]time.Time{}}, now: start,
		players: map[string]int{}, down: map[string]bool{}}
	servers := func(context.Context) ([]FeedServer, error) {
		return []FeedServer{{ID: "wd-1", Game: "wardogs", Trust: "official"}, {ID: "wd-2", Game: "wardogs", Trust: "community"}}, nil
	}
	occupancy := func(server string) (int, bool) { return r.players[server], !r.down[server] }
	r.g = NewGaps(r.st, uuid.New(), servers, occupancy, start, prometheus.NewRegistry(), quiet())
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

// TestHubDown: at start, a server whose last batch came while players were on gets a gap from
// that batch to the start; one that was empty then, or never posted, gets none.
func TestHubDown(t *testing.T) {
	r := newGapRig(t)
	lastBatch := r.now.Add(-7*time.Minute - 30*time.Second)
	r.st.last["wd-1"] = lastBatch
	r.st.players["wd-1"] = []time.Time{lastBatch.Truncate(time.Minute)}
	r.st.last["wd-2"] = r.now.Add(-3 * time.Minute) // nobody on
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
		!e.ObservedAt.Equal(lastBatch) || reason != GapHubDown || !to.Equal(r.now) || e.BatchID != 0 || e.Player.Subject != "" {
		t.Errorf("gap = %+v (%s to %s)", e, reason, to)
	}
	if n := testutil.ToFloat64(r.g.gaps.WithLabelValues("wd-1", GapHubDown)); n != 1 {
		t.Errorf("counted %v", n)
	}
	// It is decided once.
	if err := r.g.Run(ctx); err != nil || len(r.gaps()) != 1 {
		t.Errorf("a second run: %d gaps, %v", len(r.gaps()), err)
	}
	// The same gap found again (a hub that restarts with the same last batch) is stored once.
	again := newGapRig(t)
	again.st = r.st
	again.g.st = r.st
	if err := again.g.Run(ctx); err != nil || len(r.gaps()) != 1 {
		t.Errorf("found again: %d gaps, %v", len(r.gaps()), err)
	}
}

func TestHubDownRetriesTheStore(t *testing.T) {
	r := newGapRig(t)
	r.st.last["wd-1"] = r.now.Add(-time.Minute)
	r.st.players["wd-1"] = []time.Time{r.now.Add(-time.Minute).Truncate(time.Minute)}
	r.st.fail = true
	if err := r.g.Run(context.Background()); err == nil {
		t.Fatal("no error")
	}
	r.st.fail = false
	if err := r.g.Run(context.Background()); err != nil || len(r.gaps()) != 1 {
		t.Errorf("after the store came back: %d gaps, %v", len(r.gaps()), err)
	}
}

// TestSilence: ten minutes with no batch while someone is on opens a silence, the next batch ends
// it and writes the gap; an empty or unreachable server, or one whose feed never posted, is quiet
// for no reason worth a gap.
func TestSilence(t *testing.T) {
	r := newGapRig(t)
	ctx := context.Background()
	if err := r.g.Run(ctx); err != nil { // nothing posted before the start
		t.Fatal(err)
	}
	r.g.Stored("wd-1", r.now.Add(time.Minute))
	r.g.Stored("wd-2", r.now.Add(time.Minute))
	r.g.Stored("wd-1", r.now) // an older one changes nothing
	r.players["wd-1"], r.players["wd-2"] = 3, 0
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
	r.players["wd-1"], r.down["wd-1"] = 5, true
	if err := r.g.Run(ctx); err != nil || len(r.g.silent) != 0 {
		t.Errorf("unreachable: %v %v", r.g.silent, err)
	}
	if n := testutil.ToFloat64(r.g.gaps.WithLabelValues("wd-1", GapSilence)); n != 1 {
		t.Errorf("counted %v", n)
	}
}
