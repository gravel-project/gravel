package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

// TestFeedDelivery: a server's delivery reading is saved in place of the last; none before the first.
func TestFeedDelivery(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	t0 := time.Date(2026, 10, 20, 12, 30, 0, 0, time.UTC)
	if _, ok, err := st.LastFeedDelivery(ctx, o.ID, "wd-1"); err != nil || ok {
		t.Fatalf("before any: %v, %v", ok, err)
	}
	for _, d := range []store.FeedDelivery{{Sent: 4, Dropped: 0, ObservedAt: t0}, {Sent: 6, Dropped: 2, ObservedAt: t0.Add(time.Minute)}} {
		if err := st.SaveFeedDelivery(ctx, o.ID, "wd-1", d); err != nil {
			t.Fatal(err)
		}
	}
	if d, ok, err := st.LastFeedDelivery(ctx, o.ID, "wd-1"); err != nil || !ok || d.Sent != 6 || d.Dropped != 2 || !d.ObservedAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("last = %+v, %v, %v", d, ok, err)
	}
	if _, ok, _ := st.LastFeedDelivery(ctx, o.ID, "wd-2"); ok {
		t.Error("another server's reading")
	}
	if err := st.SaveFeedDelivery(ctx, o.ID, "wd-1", store.FeedDelivery{Sent: -1, ObservedAt: t0}); err == nil {
		t.Error("a negative count saved")
	}
}

// TestKillCounts: the matches that ended in a window, and a server's kills counted per player in
// one: a kill for its player unless that is its target (a suicide is a death only), a death for
// its target (the world's kill too); another kind, another server, or outside the window, nothing.
func TestKillCounts(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	t0 := time.Date(2026, 10, 21, 1, 0, 0, 0, time.UTC)
	servers := []store.ManagedServer{}
	for _, id := range []string{"wd-1", "wd-2"} {
		servers = append(servers, store.ManagedServer{ID: id, GameID: "wardogs", Name: id, Driver: "wardogs", Location: "slc",
			Endpoint: "http://203.0.113.10:7789", CredentialFile: "/run/secrets/wd", PollInterval: 15 * time.Second, Trust: "official",
			Feed: json.RawMessage(`{"url": "https://ingest.example.com", "token_file": "/run/secrets/feed"}`)})
	}
	if err := st.ApplyServers(ctx, o.ID, []store.Game{{ID: "wardogs"}}, nil, servers, t0); err != nil {
		t.Fatal(err)
	}
	m1, err := st.StartMatch(ctx, store.Match{OrganizationID: o.ID, ServerID: "wd-1", GameID: "wardogs", StartedAt: t0, Map: "Kavkazi"})
	if err != nil {
		t.Fatal(err)
	}
	m2, err := st.StartMatch(ctx, store.Match{OrganizationID: o.ID, ServerID: "wd-1", GameID: "wardogs", StartedAt: t0.Add(30 * time.Minute), Map: "Bakurani"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EndMatch(ctx, m2.ID, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	ended, err := st.EndedMatches(ctx, o.ID, "wd-1", t0, t0.Add(time.Hour))
	if err != nil || len(ended) != 1 || ended[0].ID != m1.ID || ended[0].EndedAt == nil || !ended[0].EndedAt.Equal(t0.Add(30*time.Minute)) {
		t.Fatalf("ended in [t0, t0+1h) = %+v, %v (m2 ends at t0+1h, outside)", ended, err)
	}
	if ended, _ := st.EndedMatches(ctx, o.ID, "wd-2", t0, t0.Add(2*time.Hour)); len(ended) != 0 {
		t.Errorf("another server's matches: %+v", ended)
	}

	p1 := store.PlayerKey{Provider: "steam", Subject: "76561190000000001"}
	p2 := store.PlayerKey{Provider: "steam", Subject: "76561190000000002"}
	n := 0
	ev := func(server, kind string, at time.Time, player, target store.PlayerKey) store.StatsEvent {
		n++
		return store.StatsEvent{OrganizationID: o.ID, ServerID: server, GameID: "wardogs", Source: "server_adapter", SourceKey: "e" + string(rune('a'+n)),
			Kind: kind, ObservedAt: at, ReceivedAt: at, Trust: "official", SchemaVersion: 1, Player: player, Target: target, Extensions: json.RawMessage(`{}`)}
	}
	in := t0.Add(5 * time.Minute)
	if _, err := st.RecordStatsEvents(ctx, []store.StatsEvent{
		ev("wd-1", "kill", in, p1, p2),
		ev("wd-1", "kill", in.Add(time.Second), p1, p2),
		ev("wd-1", "kill", in.Add(2*time.Second), p2, p2),                // a suicide
		ev("wd-1", "kill", in.Add(3*time.Second), store.PlayerKey{}, p1), // the world
		ev("wd-1", "match_state", in, store.PlayerKey{}, store.PlayerKey{}),
		ev("wd-1", "player_joined", in, p1, store.PlayerKey{}),
		ev("wd-2", "kill", in, p2, p1),
		ev("wd-1", "kill", t0.Add(30*time.Minute), p2, p1), // at the window's end: outside
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.KillCounts(ctx, o.ID, "wd-1", "kill", t0, t0.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	want := map[store.PlayerKey]store.KillCount{p1: {Kills: 2, Deaths: 1}, p2: {Kills: 0, Deaths: 3}}
	if len(got) != len(want) || got[p1] != want[p1] || got[p2] != want[p2] {
		t.Errorf("counts = %+v, want %+v", got, want)
	}
}
