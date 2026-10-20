package stats

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/ingest"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/store"
)

// gapStore answers StatsEvents as the Postgres query does: one server's rows of a kind observed
// in [from, to), oldest first, at most limit.
type gapStore struct {
	*boardStore
	events []store.StatsEvent
	asked  struct {
		kind     string
		from, to time.Time
		limit    int
	}
}

func (g *gapStore) StatsEvents(_ context.Context, _ uuid.UUID, server, kind string, from, to time.Time, limit int) ([]store.StatsEvent, error) {
	g.asked.kind, g.asked.from, g.asked.to, g.asked.limit = kind, from, to, limit
	var out []store.StatsEvent
	for _, e := range g.events {
		if e.ServerID == server && (kind == "" || e.Kind == kind) && !e.ObservedAt.Before(from) && e.ObservedAt.Before(to) && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func newGapBoards(t *testing.T) (*Boards, *gapStore) {
	t.Helper()
	_, bs := newBoards(org.Stats{})
	bs.games = map[string]string{"wd-1": "wardogs"}
	gs := &gapStore{boardStore: bs}
	b := NewBoards(gs, uuid.New(), NewNamer(gs, uuid.New(), key, quiet()), func(context.Context) (org.Stats, error) { return org.Stats{}, nil }, &bs.changes)
	b.now = func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) }
	return b, gs
}

func gapRow(t *testing.T, server string, at time.Time, fields map[string]any) store.StatsEvent {
	t.Helper()
	ext, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return store.StatsEvent{ServerID: server, Kind: ingest.KindGap, Source: ingest.SourceServerAdapter, ObservedAt: at, Extensions: ext}
}

func TestGapsReadsEachReason(t *testing.T) {
	b, gs := newGapBoards(t)
	day := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	gs.events = []store.StatsEvent{
		gapRow(t, "wd-1", day.Add(time.Hour), map[string]any{"reason": ingest.GapHubDown, "to": day.Add(2 * time.Hour)}),
		gapRow(t, "wd-1", day.Add(3*time.Hour), map[string]any{"reason": ingest.GapSilence, "to": day.Add(3*time.Hour + 12*time.Minute)}),
		gapRow(t, "wd-1", day.Add(4*time.Hour), map[string]any{"reason": ingest.GapDropped, "to": day.Add(4*time.Hour + time.Minute), "events": 7}),
		gapRow(t, "wd-1", day.Add(5*time.Hour), map[string]any{"reason": ingest.GapReconcile, "to": day.Add(5*time.Hour + 30*time.Minute),
			"match": 42, "kills": 3, "deaths": 2, "players": 2}),
		gapRow(t, "other", day.Add(time.Hour), map[string]any{"reason": ingest.GapSilence, "to": day.Add(2 * time.Hour)}),
	}
	gs.games["other"] = "wardogs"
	got, err := b.Gaps(context.Background(), GapQuery{ServerID: "wd-1"})
	if err != nil {
		t.Fatal(err)
	}
	if gs.asked.kind != ingest.KindGap || gs.asked.limit != MaxGaps+1 {
		t.Errorf("asked for kind %q limit %d, want %q and %d", gs.asked.kind, gs.asked.limit, ingest.KindGap, MaxGaps+1)
	}
	now := b.now()
	if !got.To.Equal(now) || !got.From.Equal(now.Add(-DefaultGapWindow)) || !gs.asked.from.Equal(got.From) || !gs.asked.to.Equal(got.To) {
		t.Errorf("window %v–%v (asked %v–%v), want the 30 days to %v", got.From, got.To, gs.asked.from, gs.asked.to, now)
	}
	if len(got.Gaps) != 4 || got.More {
		t.Fatalf("got %d gaps (more %v), want this server's 4: %+v", len(got.Gaps), got.More, got.Gaps)
	}
	want := []Gap{
		{Reason: ingest.GapHubDown, From: day.Add(time.Hour), To: day.Add(2 * time.Hour)},
		{Reason: ingest.GapSilence, From: day.Add(3 * time.Hour), To: day.Add(3*time.Hour + 12*time.Minute)},
		{Reason: ingest.GapDropped, From: day.Add(4 * time.Hour), To: day.Add(4*time.Hour + time.Minute), Events: 7},
		{Reason: ingest.GapReconcile, From: day.Add(5 * time.Hour), To: day.Add(5*time.Hour + 30*time.Minute), MatchID: "42", Kills: 3, Deaths: 2, Players: 2},
	}
	for i := range want {
		g, w := got.Gaps[i], want[i]
		if g.Reason != w.Reason || !g.From.Equal(w.From) || !g.To.Equal(w.To) || g.Events != w.Events || g.MatchID != w.MatchID ||
			g.Kills != w.Kills || g.Deaths != w.Deaths || g.Players != w.Players {
			t.Errorf("gap %d = %+v, want %+v", i, g, w)
		}
	}
}

func TestGapsWindowAndLimits(t *testing.T) {
	b, gs := newGapBoards(t)
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i := range MaxGaps + 1 {
		gs.events = append(gs.events, gapRow(t, "wd-1", start.Add(time.Duration(i)*time.Minute), map[string]any{"reason": ingest.GapSilence, "to": start.Add(time.Duration(i)*time.Minute + 30*time.Second)}))
	}
	// A window given in full is read as given (in UTC), and a full page says there is more.
	from, to := start.In(time.FixedZone("CDT", -5*3600)), start.Add(48*time.Hour)
	got, err := b.Gaps(context.Background(), GapQuery{ServerID: "wd-1", From: from, To: to})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Gaps) != MaxGaps || !got.More || got.From.Location() != time.UTC || !got.From.Equal(from) {
		t.Errorf("got %d gaps, more %v, from %v; want %d, true, %v in UTC", len(got.Gaps), got.More, got.From, MaxGaps, from)
	}
	// Exactly a page is not "more".
	gs.events = gs.events[:MaxGaps]
	if got, err = b.Gaps(context.Background(), GapQuery{ServerID: "wd-1", From: from, To: to}); err != nil || got.More || len(got.Gaps) != MaxGaps {
		t.Errorf("a page exactly: %d gaps, more %v, err %v", len(got.Gaps), got.More, err)
	}
	// Only a from: the window runs to now.
	if got, err = b.Gaps(context.Background(), GapQuery{ServerID: "wd-1", From: b.now().Add(-time.Hour)}); err != nil || !got.To.Equal(b.now()) {
		t.Errorf("from only: to %v err %v, want now", got.To, err)
	}
	// A window of exactly the most is allowed.
	if _, err = b.Gaps(context.Background(), GapQuery{ServerID: "wd-1", From: to.Add(-MaxGapWindow), To: to}); err != nil {
		t.Errorf("a 90-day window: %v", err)
	}
	// Nothing recorded is an empty list, not an error.
	gs.events = nil
	if got, err = b.Gaps(context.Background(), GapQuery{ServerID: "wd-1"}); err != nil || len(got.Gaps) != 0 || got.More {
		t.Errorf("no gaps: %+v, %v", got, err)
	}
}

func TestGapsRefusals(t *testing.T) {
	b, _ := newGapBoards(t)
	now := b.now()
	for name, q := range map[string]GapQuery{
		"no server":            {},
		"not a server id":      {ServerID: "Not An ID!"},
		"from after to":        {ServerID: "wd-1", From: now, To: now.Add(-time.Hour)},
		"from equal to":        {ServerID: "wd-1", From: now, To: now},
		"longer than the most": {ServerID: "wd-1", From: now.Add(-MaxGapWindow - time.Second), To: now},
		"only a from, too far": {ServerID: "wd-1", From: now.Add(-MaxGapWindow - time.Hour)},
	} {
		if _, err := b.Gaps(context.Background(), q); !errors.Is(err, ErrInvalidQuery) {
			t.Errorf("%s: err %v, want ErrInvalidQuery", name, err)
		}
	}
	if _, err := b.Gaps(context.Background(), GapQuery{ServerID: "nope"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an unknown server: err %v, want store.ErrNotFound", err)
	}
}

func TestGapsOddRows(t *testing.T) {
	b, gs := newGapBoards(t)
	at := b.now().Add(-time.Hour)
	// A row whose end is missing or before its start reads as a point; an unknown field is ignored.
	gs.events = []store.StatsEvent{gapRow(t, "wd-1", at, map[string]any{"reason": ingest.GapSilence, "later": true})}
	got, err := b.Gaps(context.Background(), GapQuery{ServerID: "wd-1"})
	if err != nil || len(got.Gaps) != 1 || !got.Gaps[0].To.Equal(at) {
		t.Fatalf("a gap without an end: %+v, %v; want one ending where it starts", got.Gaps, err)
	}
	// Extensions that aren't a gap's fields are an error, not a made-up gap.
	gs.events = []store.StatsEvent{{ServerID: "wd-1", Kind: ingest.KindGap, ObservedAt: at, Extensions: json.RawMessage(`{"to": "yesterday"}`)}}
	if _, err := b.Gaps(context.Background(), GapQuery{ServerID: "wd-1"}); err == nil {
		t.Error("a gap with a bad end: no error")
	}
}
