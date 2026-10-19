package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

// TestHubRuns: each start is a run, the next start sees the one before with the last time it was
// known to run, and that time never moves back.
func TestHubRuns(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	t0 := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

	first, _, ok, err := st.StartHubRun(ctx, o.ID, t0)
	if err != nil || ok || first.ID == 0 || !first.StartedAt.Equal(t0) || !first.AliveAt.Equal(t0) {
		t.Fatalf("first run = %+v, a previous one %v, %v", first, ok, err)
	}
	if err := st.HubRunAlive(ctx, first.ID, t0.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.HubRunAlive(ctx, first.ID, t0.Add(time.Minute)); err != nil { // late: changes nothing
		t.Fatal(err)
	}
	second, prev, ok, err := st.StartHubRun(ctx, o.ID, t0.Add(9*time.Minute))
	if err != nil || !ok || second.ID == first.ID {
		t.Fatalf("second run = %+v, %v, %v", second, ok, err)
	}
	if prev.ID != first.ID || !prev.StartedAt.Equal(t0) || !prev.AliveAt.Equal(t0.Add(5*time.Minute)) {
		t.Errorf("previous = %+v", prev)
	}
	if err := st.HubRunAlive(ctx, second.ID+100, t0); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an unknown run: %v", err)
	}
}

// TestHubRunsMigrationDropsOldHubDownGaps: migration 37 removes the hub_down gaps the previous rule
// wrote (from a server's last batch, which can be hours before the hub stopped) and keeps every
// other event.
func TestHubRunsMigrationDropsOldHubDownGaps(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	t0 := time.Date(2026, 10, 19, 12, 28, 5, 0, time.UTC)
	srv := store.ManagedServer{ID: "wd-1", GameID: "wardogs", Name: "War Dogs #1", Driver: "wardogs", Location: "slc",
		Endpoint: "http://203.0.113.10:7789", CredentialFile: "/run/secrets/wd", PollInterval: 15 * time.Second, Trust: "official"}
	if err := st.ApplyServers(ctx, o.ID, []store.Game{{ID: "wardogs"}}, nil, []store.ManagedServer{srv}, t0); err != nil {
		t.Fatal(err)
	}
	if err := st.MigrateDownTo(ctx, 36); err != nil {
		t.Fatal(err)
	}
	event := func(key, kind, ext string) store.StatsEvent {
		return store.StatsEvent{OrganizationID: o.ID, ServerID: "wd-1", GameID: "wardogs", Source: "server_adapter", SourceKey: key,
			Kind: kind, ObservedAt: t0.Add(-time.Hour), ReceivedAt: t0, Trust: "official", SchemaVersion: 1, Extensions: json.RawMessage(ext)}
	}
	if _, err := st.RecordStatsEvents(ctx, []store.StatsEvent{
		event("gap:hub_down:1", "gap", `{"reason":"hub_down","to":"2026-10-19T12:28:05Z"}`),
		event("gap:silence:1", "gap", `{"reason":"silence","to":"2026-10-19T12:00:00Z"}`),
		event("s1", "match_state", `{"map":"Kavkazi","state":"MatchState.PreMatch"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := st.StatsEvents(ctx, o.ID, "wd-1", "", t0.Add(-2*time.Hour), t0, 10)
	if err != nil || len(got) != 2 {
		t.Fatalf("after the migration: %+v, %v", got, err)
	}
	for _, e := range got {
		if e.SourceKey == "gap:hub_down:1" {
			t.Errorf("the old hub_down gap is still there: %+v", e)
		}
	}
}
