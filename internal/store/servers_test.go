package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

func TestApplyServers(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	t0 := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)

	wd := store.Game{ID: "wardogs", Bands: json.RawMessage(`[{"key": "ScorePeriod", "max": 24, "section": "MatchState.Playing.KOTH"}]`)}
	srv := store.ManagedServer{ID: "wd-1", GameID: "wardogs", Name: "War Dogs #1", Driver: "wardogs", Location: "slc",
		Endpoint: "http://203.0.113.10:7789", CredentialFile: "/run/secrets/wd", PollInterval: 15 * time.Second, Trust: "official",
		Seeding: json.RawMessage(`{"threshold": 20, "hours": "17:00-23:00", "timezone": "America/Chicago", "cooldown": "1h0m0s"}`)}
	two := srv
	two.ID, two.Seeding = "wd-2", nil

	if err := st.ApplyServers(ctx, o.ID, []store.Game{wd}, []store.ManagedServer{srv, two}, t0); err != nil {
		t.Fatal(err)
	}
	games, err := st.ListGames(ctx, o.ID, false)
	if err != nil || len(games) != 1 || games[0].ID != "wardogs" || !games[0].CreatedAt.Equal(t0) {
		t.Fatalf("games = %+v, %v", games, err)
	}
	var bands []map[string]any
	if err := json.Unmarshal(games[0].Bands, &bands); err != nil || len(bands) != 1 || bands[0]["max"] != float64(24) {
		t.Errorf("bands = %s, %v", games[0].Bands, err)
	}
	got, err := st.ListManagedServers(ctx, o.ID, false)
	if err != nil || len(got) != 2 {
		t.Fatalf("servers = %+v, %v", got, err)
	}
	if g := got[0]; g.ID != "wd-1" || g.PollInterval != 15*time.Second || g.Trust != "official" || len(g.Seeding) == 0 || g.RemovedAt != nil {
		t.Errorf("wd-1 = %+v", g)
	}
	if got[1].Seeding != nil {
		t.Errorf("wd-2 seeding = %s, want NULL", got[1].Seeding)
	}

	// The same again: nothing changes, updated_at included (jsonb compares by value, so key
	// order and spacing don't count).
	t1 := t0.Add(time.Hour)
	wdReordered := wd
	wdReordered.Bands = json.RawMessage(`[{"section":"MatchState.Playing.KOTH","key":"ScorePeriod","max":24}]`)
	if err := st.ApplyServers(ctx, o.ID, []store.Game{wdReordered}, []store.ManagedServer{srv, two}, t1); err != nil {
		t.Fatal(err)
	}
	got, _ = st.ListManagedServers(ctx, o.ID, false)
	games, _ = st.ListGames(ctx, o.ID, false)
	if !got[0].UpdatedAt.Equal(t0) || !got[1].UpdatedAt.Equal(t0) || !games[0].UpdatedAt.Equal(t0) {
		t.Errorf("an unchanged apply touched updated_at: %v %v %v", got[0].UpdatedAt, got[1].UpdatedAt, games[0].UpdatedAt)
	}

	// A change to one server, the other taken out.
	t2 := t1.Add(time.Hour)
	changed := srv
	changed.Name = "Renamed"
	if err := st.ApplyServers(ctx, o.ID, []store.Game{wd}, []store.ManagedServer{changed}, t2); err != nil {
		t.Fatal(err)
	}
	got, _ = st.ListManagedServers(ctx, o.ID, false)
	if len(got) != 1 || got[0].Name != "Renamed" || !got[0].UpdatedAt.Equal(t2) || !got[0].CreatedAt.Equal(t0) {
		t.Errorf("after the change = %+v", got)
	}
	all, _ := st.ListManagedServers(ctx, o.ID, true)
	if len(all) != 2 || all[1].RemovedAt == nil || !all[1].RemovedAt.Equal(t2) {
		t.Errorf("the removed server = %+v", all)
	}

	// Back in the manifest: revived, the row is the same one.
	t3 := t2.Add(time.Hour)
	if err := st.ApplyServers(ctx, o.ID, []store.Game{wd}, []store.ManagedServer{changed, two}, t3); err != nil {
		t.Fatal(err)
	}
	got, _ = st.ListManagedServers(ctx, o.ID, false)
	if len(got) != 2 || got[1].RemovedAt != nil || !got[1].CreatedAt.Equal(t0) || !got[1].UpdatedAt.Equal(t3) {
		t.Errorf("revived = %+v", got)
	}

	// Everything out: servers and the game marked removed, nothing deleted.
	if err := st.ApplyServers(ctx, o.ID, nil, nil, t3.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if live, _ := st.ListManagedServers(ctx, o.ID, false); len(live) != 0 {
		t.Errorf("live servers = %+v", live)
	}
	if g, _ := st.ListGames(ctx, o.ID, false); len(g) != 0 {
		t.Errorf("live games = %+v", g)
	}
	if g, _ := st.ListGames(ctx, o.ID, true); len(g) != 1 {
		t.Errorf("all games = %+v", g)
	}

	// A server whose game is not there is refused, and the transaction leaves nothing behind.
	orphan := srv
	orphan.ID, orphan.GameID = "cs-1", "cs2"
	if err := st.ApplyServers(ctx, o.ID, []store.Game{wd}, []store.ManagedServer{orphan}, t3.Add(2*time.Hour)); err == nil {
		t.Error("a server of an unknown game was stored")
	}
	if g, _ := st.ListGames(ctx, o.ID, false); len(g) != 0 {
		t.Errorf("a failed apply revived a game: %+v", g)
	}
}
