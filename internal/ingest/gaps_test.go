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
	mu        sync.Mutex
	last      map[string]time.Time
	players   map[string][]time.Time // minutes anyone was on
	fail      bool
	delivered map[string]store.FeedDelivery
	saves     int
	matches   map[string][]store.Match
	stats     map[int64][]store.MatchPlayer
	kills     []store.StatsEvent // what KillCounts counts
}

func (g *gapStore) LastFeedDelivery(_ context.Context, _ uuid.UUID, server string) (store.FeedDelivery, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.fail {
		return store.FeedDelivery{}, false, errors.New("database away")
	}
	d, ok := g.delivered[server]
	return d, ok, nil
}

func (g *gapStore) SaveFeedDelivery(_ context.Context, _ uuid.UUID, server string, d store.FeedDelivery) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.delivered[server] = d
	g.saves++
	return nil
}

func (g *gapStore) EndedMatches(_ context.Context, _ uuid.UUID, server string, from, to time.Time) ([]store.Match, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []store.Match
	for _, m := range g.matches[server] {
		if m.EndedAt != nil && !m.EndedAt.Before(from) && m.EndedAt.Before(to) {
			out = append(out, m)
		}
	}
	return out, nil
}

func (g *gapStore) MatchPlayers(_ context.Context, id int64) ([]store.MatchPlayer, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stats[id], nil
}

// KillCounts counts as the store does: a kill for a player who is not the target, a death for the target.
func (g *gapStore) KillCounts(_ context.Context, _ uuid.UUID, server, kind string, from, to time.Time) (map[store.PlayerKey]store.KillCount, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := map[store.PlayerKey]store.KillCount{}
	for _, e := range g.kills {
		if e.ServerID != server || e.Kind != kind || e.ObservedAt.Before(from) || !e.ObservedAt.Before(to) {
			continue
		}
		if e.Player != (store.PlayerKey{}) && e.Player != e.Target {
			c := out[e.Player]
			c.Kills++
			out[e.Player] = c
		}
		if e.Target != (store.PlayerKey{}) {
			c := out[e.Target]
			c.Deaths++
			out[e.Target] = c
		}
	}
	return out, nil
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
	st       *gapStore
	g        *Gaps
	reg      *prometheus.Registry
	now      time.Time
	stopped  time.Time // when the previous run was last known to run
	players  map[string]int
	down     map[string]bool
	mu       sync.Mutex
	feed     FeedServer // what wd-1's and wd-2's game says of its feed
	delivery map[string]store.FeedDelivery
}

func (r *gapRig) setDelivery(server string, sent, dropped int64, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.delivery[server] = store.FeedDelivery{Sent: sent, Dropped: dropped, ObservedAt: at}
}

// newGapRig is a hub that starts at 01:00, eight minutes after its previous run stopped.
func newGapRig(t *testing.T) *gapRig { return newGapRigAway(t, true) }

func newGapRigAway(t *testing.T, known bool) *gapRig {
	t.Helper()
	start := time.Date(2026, 10, 21, 1, 0, 0, 0, time.UTC)
	r := &gapRig{st: &gapStore{last: map[string]time.Time{}, players: map[string][]time.Time{}, delivered: map[string]store.FeedDelivery{},
		matches: map[string][]store.Match{}, stats: map[int64][]store.MatchPlayer{}}, now: start,
		stopped: start.Add(-8*time.Minute + 20*time.Second), players: map[string]int{}, down: map[string]bool{},
		delivery: map[string]store.FeedDelivery{}, reg: prometheus.NewRegistry()}
	servers := func(context.Context) ([]FeedServer, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		return []FeedServer{{ID: "wd-1", Game: "wardogs", Trust: "official", Retries: r.feed.Retries, Kills: r.feed.Kills},
			{ID: "wd-2", Game: "wardogs", Trust: "community", Retries: r.feed.Retries, Kills: r.feed.Kills}}, nil
	}
	occupancy := func(server string) (int, bool) { return r.players[server], !r.down[server] }
	delivery := func(server string) (store.FeedDelivery, bool) {
		r.mu.Lock()
		defer r.mu.Unlock()
		d, ok := r.delivery[server]
		return d, ok
	}
	away := Away{From: r.stopped, To: start, Known: known}
	if !known {
		away.From = time.Time{}
	}
	r.g = NewGaps(r.st, uuid.New(), servers, occupancy, delivery, away, r.reg, quiet())
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

// TestHubDownRetries: a feed that retries (War Dogs, deploy#49) lost nothing the server still
// holds while the hub was away, so no hub_down gap with players on as the hub stopped.
func TestHubDownRetries(t *testing.T) {
	r := newGapRig(t)
	r.feed.Retries = true
	r.st.last["wd-1"] = r.now.Add(-40 * time.Minute)
	r.st.players["wd-1"] = []time.Time{r.stopped.Truncate(time.Minute)}
	if err := r.g.Run(context.Background()); err != nil || len(r.gaps()) != 0 {
		t.Errorf("a feed that retries: %+v, %v", r.gaps(), err)
	}
}

// gapExt is a gap's own fields.
func gapExt(t *testing.T, e store.StatsEvent) map[string]any {
	t.Helper()
	var ext map[string]any
	if err := json.Unmarshal(e.Extensions, &ext); err != nil {
		t.Fatal(err)
	}
	return ext
}

// TestDropped: the server's count of what it gave up on, against the last reading. The first
// reading is only saved; a rise is a gap from the last reading with how many; the same reading
// again changes nothing; a count below the last is a restart, and all it counts since is lost.
func TestDropped(t *testing.T) {
	r := newGapRig(t)
	ctx := context.Background()
	t0 := r.now
	r.setDelivery("wd-1", 4, 2, t0)
	r.setDelivery("wd-2", 100, 50, t0) // another server's counts are its own
	if err := r.g.Run(ctx); err != nil || len(r.gaps()) != 0 || r.st.delivered["wd-1"].Dropped != 2 {
		t.Fatalf("first reading: %+v, %+v, %v", r.gaps(), r.st.delivered, err)
	}
	t1 := t0.Add(time.Minute)
	r.setDelivery("wd-1", 10, 5, t1)
	r.setDelivery("wd-2", 120, 50, t1)
	if err := r.g.Run(ctx); err != nil {
		t.Fatal(err)
	}
	got := r.gaps()
	if len(got) != 1 {
		t.Fatalf("gaps = %+v", got)
	}
	if reason, to := gapOf(t, got[0]); reason != GapDropped || !to.Equal(t1) || !got[0].ObservedAt.Equal(t0) || got[0].ServerID != "wd-1" || gapExt(t, got[0])["events"] != 3.0 {
		t.Errorf("gap = %+v (%s to %s)", got[0], reason, to)
	}
	if n := testutil.ToFloat64(r.g.dropped.WithLabelValues("wd-1")); n != 3 {
		t.Errorf("dropped events counted %v", n)
	}
	saves := r.st.saves
	if err := r.g.Run(ctx); err != nil || len(r.gaps()) != 1 || r.st.saves != saves {
		t.Errorf("the same reading again: %d gaps, %d saves, %v", len(r.gaps()), r.st.saves-saves, err)
	}
	// Sent and dropped below the last: the server restarted and dropped 1 since.
	t2 := t1.Add(time.Minute)
	r.setDelivery("wd-1", 2, 1, t2)
	if err := r.g.Run(ctx); err != nil || len(r.gaps()) != 2 {
		t.Fatalf("after a restart: %+v, %v", r.gaps(), err)
	}
	if e := r.gaps()[1]; gapExt(t, e)["events"] != 1.0 || !e.ObservedAt.Equal(t1) {
		t.Errorf("restart gap = %+v", e)
	}
	// Nothing new dropped: no gap, the reading saved.
	r.setDelivery("wd-1", 9, 1, t2.Add(time.Minute))
	if err := r.g.Run(ctx); err != nil || len(r.gaps()) != 2 || r.st.delivered["wd-1"].Sent != 9 {
		t.Errorf("no new drop: %d gaps, %+v, %v", len(r.gaps()), r.st.delivered["wd-1"], err)
	}
}

// TestDroppedWhileAway: the reading saved before the hub stopped is what the first one after its
// start is compared with, so what the server dropped while the hub was away is a gap.
func TestDroppedWhileAway(t *testing.T) {
	r := newGapRig(t)
	r.st.delivered["wd-1"] = store.FeedDelivery{Sent: 40, Dropped: 0, ObservedAt: r.stopped}
	r.setDelivery("wd-1", 41, 7, r.now)
	if err := r.g.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := r.gaps()
	if len(got) != 1 || !got[0].ObservedAt.Equal(r.stopped) || gapExt(t, got[0])["events"] != 7.0 {
		t.Errorf("gaps = %+v", got)
	}
	// A store that cannot say is an error, and nothing is decided.
	broken := newGapRig(t)
	broken.st.fail = true
	broken.setDelivery("wd-1", 1, 0, broken.now)
	if err := broken.g.Run(context.Background()); err == nil {
		t.Error("a store that is away: no error")
	}
}

// TestReconcile: ReconcileAfter past a match's end, each player's kills and deaths from the feed
// against the poll's. The feed short of the poll is one gap for the match with what is missing; a
// feed that has them all (a suicide a death only, the world nobody's kill) is none; each match is
// decided once; a game whose feed does not post kills is not reconciled.
func TestReconcile(t *testing.T) {
	r := newGapRig(t)
	r.feed.Kills = true
	ctx := context.Background()
	p1, p2, p3 := store.PlayerKey{Provider: "steam", Subject: "76561190000000001"}, store.PlayerKey{Provider: "steam", Subject: "76561190000000002"}, store.PlayerKey{Provider: "steam", Subject: "76561190000000003"}
	start := r.now.Add(-time.Hour)
	end := r.now.Add(-ReconcileAfter - time.Second)
	short, whole := start.Add(-2*time.Hour), start.Add(-90*time.Minute)
	shortEnd, wholeEnd := short.Add(20*time.Minute), whole.Add(20*time.Minute)
	r.st.matches["wd-1"] = []store.Match{{ID: 1, ServerID: "wd-1", StartedAt: start, EndedAt: &end}}
	r.st.matches["wd-2"] = []store.Match{{ID: 2, ServerID: "wd-2", StartedAt: short, EndedAt: &shortEnd}, {ID: 3, ServerID: "wd-2", StartedAt: whole, EndedAt: &wholeEnd}}
	row := func(m int64, p store.PlayerKey, k, d int) store.MatchPlayer {
		return store.MatchPlayer{MatchID: m, Provider: p.Provider, Subject: p.Subject, Kills: k, Deaths: d}
	}
	kill := func(server string, at time.Time, killer, victim store.PlayerKey) store.StatsEvent {
		return store.StatsEvent{ServerID: server, Kind: KindKill, ObservedAt: at, Player: killer, Target: victim}
	}
	// Match 1: the poll has p1 2 kills, p2 2 deaths, p3 a suicide; the feed has all of it, one
	// kill late (posted after the end, within ReconcileAfter), and a fall that is p3's death too.
	r.st.stats[1] = []store.MatchPlayer{row(1, p1, 2, 0), row(1, p2, 0, 2), row(1, p3, 0, 2)}
	r.st.kills = append(r.st.kills, kill("wd-1", start.Add(time.Minute), p1, p2), kill("wd-1", end.Add(time.Minute), p1, p2),
		kill("wd-1", start.Add(2*time.Minute), p3, p3), kill("wd-1", start.Add(3*time.Minute), store.PlayerKey{}, p3))
	// Match 2: the poll has p1 3 kills, p2 3 deaths; the feed only one of them. Match 3: whole.
	r.st.stats[2] = []store.MatchPlayer{row(2, p1, 3, 0), row(2, p2, 0, 3)}
	r.st.kills = append(r.st.kills, kill("wd-2", short.Add(time.Minute), p1, p2))
	r.st.stats[3] = []store.MatchPlayer{row(3, p1, 1, 0), row(3, p2, 0, 1)}
	r.st.kills = append(r.st.kills, kill("wd-2", whole.Add(time.Minute), p1, p2))

	if err := r.g.Run(ctx); err != nil {
		t.Fatal(err)
	}
	got := r.gaps()
	if len(got) != 1 {
		t.Fatalf("gaps = %+v", got)
	}
	e := got[0]
	reason, to := gapOf(t, e)
	ext := gapExt(t, e)
	if e.ServerID != "wd-2" || reason != GapReconcile || !e.ObservedAt.Equal(short) || !to.Equal(shortEnd) ||
		ext["match"] != 2.0 || ext["kills"] != 2.0 || ext["deaths"] != 2.0 || ext["players"] != 2.0 || e.Trust != "community" {
		t.Errorf("gap = %+v %v", e, ext)
	}
	if err := r.g.Run(ctx); err != nil || len(r.gaps()) != 1 {
		t.Errorf("a second run: %d gaps, %v", len(r.gaps()), err)
	}
	// A match that ended less than ReconcileAfter ago waits.
	soon := r.now.Add(-time.Minute)
	r.st.matches["wd-1"] = append(r.st.matches["wd-1"], store.Match{ID: 4, ServerID: "wd-1", StartedAt: r.now.Add(-30 * time.Minute), EndedAt: &soon})
	r.st.stats[4] = []store.MatchPlayer{row(4, p1, 5, 0)}
	if err := r.g.Run(ctx); err != nil || len(r.gaps()) != 1 {
		t.Errorf("too soon: %d gaps, %v", len(r.gaps()), err)
	}
	r.now = r.now.Add(ReconcileAfter)
	if err := r.g.Run(ctx); err != nil || len(r.gaps()) != 2 {
		t.Errorf("then: %d gaps, %v", len(r.gaps()), err)
	}
	if n := testutil.ToFloat64(r.g.gaps.WithLabelValues("wd-2", GapReconcile)); n != 1 {
		t.Errorf("counted %v", n)
	}
	// A game whose feed does not post kills: nothing to reconcile.
	off := newGapRig(t)
	off.st.matches = r.st.matches
	off.st.stats = r.st.stats
	if err := off.g.Run(ctx); err != nil || len(off.gaps()) != 0 {
		t.Errorf("feed.kills off: %d gaps, %v", len(off.gaps()), err)
	}
}
