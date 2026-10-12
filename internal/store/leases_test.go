package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

// Server leases (#20) on Postgres: one live lease per server and per holder's reference, the
// update a lease's job makes, and the lookups.
func TestServerLeases(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	t0 := time.Date(2026, 10, 14, 1, 0, 0, 0, time.UTC)
	if err := st.ApplyServers(ctx, o.ID, []store.Game{{ID: "cs2"}}, nil, []store.ManagedServer{cloudServer()}, t0); err != nil {
		t.Fatal(err)
	}
	lease := func(ref string, at time.Time) store.ServerLease {
		return store.ServerLease{ID: uuid.New(), OrganizationID: o.ID, ServerID: "cs2-1", Pool: "pug", HolderKind: store.LeaseHolderApp, Holder: "app-1",
			HolderRef: ref, State: store.LeaseStarting, Settings: map[string]string{"start_map": "de_nuke"}, Plugins: []string{"matchzy"},
			MatchConfig: []byte(`{"matchid":1}`), ExpectedPlayers: 10, CreatedAt: at, MaxUntil: at.Add(4 * time.Hour)}
	}
	a := lease("s1", t0)
	if err := st.CreateServerLease(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetServerLease(ctx, o.ID, a.ID)
	if err != nil || got.Settings["start_map"] != "de_nuke" || len(got.Plugins) != 1 || string(got.MatchConfig) != `{"matchid":1}` || !got.Live() ||
		!got.MaxUntil.Equal(a.MaxUntil) || got.ReadyAt != nil {
		t.Fatalf("read back %+v %v", got, err)
	}
	if err := st.CreateServerLease(ctx, lease("s2", t0)); !errors.Is(err, store.ErrLeaseServerTaken) {
		t.Errorf("a second live lease of the server: %v", err)
	}
	if ref, err := st.LiveLeaseForRef(ctx, o.ID, store.LeaseHolderApp, "app-1", "s1"); err != nil || ref.ID != a.ID {
		t.Errorf("live for the reference: %+v %v", ref, err)
	}
	if _, err := st.LiveLeaseForRef(ctx, o.ID, store.LeaseHolderApp, "app-2", "s1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("another holder's reference: %v", err)
	}
	ready := t0.Add(2 * time.Minute)
	a.State, a.ReadyAt, a.PluginState, a.MatchLoaded = store.LeaseReady, &ready, "loaded", true
	if err := st.UpdateServerLease(ctx, a); err != nil {
		t.Fatal(err)
	}
	released := t0.Add(time.Hour)
	a.State, a.Reason, a.ReleasedAt = store.LeaseReleased, "the match is over", &released
	if err := st.UpdateServerLease(ctx, a); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetServerLease(ctx, o.ID, a.ID); got.State != store.LeaseReleased || got.PluginState != "loaded" || !got.MatchLoaded ||
		got.ReadyAt == nil || !got.ReleasedAt.Equal(released) || got.Live() {
		t.Errorf("updated %+v", got)
	}
	// Over, it frees the server and the reference.
	b := lease("s1", t0.Add(2*time.Hour))
	if err := st.CreateServerLease(ctx, b); err != nil {
		t.Fatal(err)
	}
	c := lease("s1", t0.Add(3*time.Hour))
	c.ServerID = "cs2-1"
	if err := st.CreateServerLease(ctx, c); !errors.Is(err, store.ErrLeaseServerTaken) && !errors.Is(err, store.ErrLeaseRefTaken) {
		t.Errorf("the same reference again while live: %v", err)
	}
	all, err := st.ListServerLeases(ctx, o.ID, "", false, 10)
	if err != nil || len(all) != 2 || all[0].ID != b.ID {
		t.Errorf("list, newest first: %v %v", all, err)
	}
	live, err := st.ListServerLeases(ctx, o.ID, "cs2-1", true, 10)
	if err != nil || len(live) != 1 || live[0].ID != b.ID {
		t.Errorf("live only: %v %v", live, err)
	}
	if none, _ := st.ListServerLeases(ctx, o.ID, "other", false, 10); len(none) != 0 {
		t.Errorf("another server's: %v", none)
	}
	if one, _ := st.ListServerLeases(ctx, o.ID, "", false, 1); len(one) != 1 {
		t.Errorf("limit: %v", one)
	}
	if _, err := st.GetServerLease(ctx, o.ID, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	if err := st.UpdateServerLease(ctx, store.ServerLease{OrganizationID: o.ID, ID: uuid.New(), State: store.LeaseFailed}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("update unknown: %v", err)
	}
	bad := lease("s9", t0)
	bad.ServerID = "nope"
	if err := st.CreateServerLease(ctx, bad); err == nil {
		t.Error("a lease of a server that doesn't exist")
	}
}
