package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

func cloudServer() store.ManagedServer {
	return store.ManagedServer{ID: "cs2-1", GameID: "cs2", Name: "CS2", Driver: "cloudvm", Location: "do-sfo3",
		CredentialFile: "/run/secrets/rcon", PollInterval: 15 * time.Second, Trust: "official",
		Feed:     json.RawMessage(`{"url": "https://ingest.example.com", "token_file": "/run/secrets/feed"}`),
		VM:       json.RawMessage(`{"size": "c-2", "volume_gib": 100, "address": "reserved"}`),
		Settings: json.RawMessage(`{"max_players": "10", "tv": "false"}`)}
}

func TestApplyServersWithLocations(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	t0 := time.Date(2026, 10, 12, 1, 0, 0, 0, time.UTC)
	loc := store.Location{ID: "do-sfo3", Provider: "digitalocean", Region: "sfo3", Project: "p", TokenFile: "/run/secrets/do",
		SSHKeyFile: "/run/secrets/key", Image: "fedora-44-x64", ExtraSSHKeys: []string{"ssh-ed25519 AAAA john"}}
	games := []store.Game{{ID: "cs2"}}

	if err := st.ApplyServers(ctx, o.ID, games, []store.Location{loc}, []store.ManagedServer{cloudServer()}, t0); err != nil {
		t.Fatal(err)
	}
	locs, err := st.ListLocations(ctx, o.ID)
	if err != nil || len(locs) != 1 || locs[0].Region != "sfo3" || len(locs[0].ExtraSSHKeys) != 1 || locs[0].SSHFrom != nil || !locs[0].UpdatedAt.Equal(t0) {
		t.Fatalf("locations %+v %v", locs, err)
	}
	srvs, _ := st.ListManagedServers(ctx, o.ID, false)
	if len(srvs) != 1 || !strings.Contains(string(srvs[0].VM), `"c-2"`) || !strings.Contains(string(srvs[0].Settings), `"10"`) || srvs[0].Endpoint != "" {
		t.Fatalf("server %+v", srvs)
	}

	// The same again keeps updated_at; a changed location updates it.
	t1 := t0.Add(time.Hour)
	if err := st.ApplyServers(ctx, o.ID, games, []store.Location{loc}, []store.ManagedServer{cloudServer()}, t1); err != nil {
		t.Fatal(err)
	}
	if locs, _ = st.ListLocations(ctx, o.ID); !locs[0].UpdatedAt.Equal(t0) {
		t.Errorf("an unchanged location was updated at %v", locs[0].UpdatedAt)
	}
	loc.SSHFrom = []string{"203.0.113.4/32"}
	if err := st.ApplyServers(ctx, o.ID, games, []store.Location{loc}, []store.ManagedServer{cloudServer()}, t1); err != nil {
		t.Fatal(err)
	}
	if locs, _ = st.ListLocations(ctx, o.ID); !locs[0].UpdatedAt.Equal(t1) || len(locs[0].SSHFrom) != 1 {
		t.Errorf("a changed location: %+v", locs[0])
	}

	// Removed, then revived.
	if err := st.ApplyServers(ctx, o.ID, games, nil, []store.ManagedServer{cloudServer()}, t1.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if locs, _ = st.ListLocations(ctx, o.ID); len(locs) != 0 {
		t.Errorf("a removed location is listed: %+v", locs)
	}
	if err := st.ApplyServers(ctx, o.ID, games, []store.Location{loc}, []store.ManagedServer{cloudServer()}, t1.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if locs, _ = st.ListLocations(ctx, o.ID); len(locs) != 1 || locs[0].RemovedAt != nil {
		t.Errorf("not revived: %+v", locs)
	}
}

func TestServerHosts(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	t0 := time.Date(2026, 10, 12, 1, 0, 0, 0, time.UTC)
	if err := st.ApplyServers(ctx, o.ID, []store.Game{{ID: "cs2"}}, nil, []store.ManagedServer{cloudServer()}, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetServerHost(ctx, o.ID, "cs2-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("no row yet: %v", err)
	}
	next := t0.Add(time.Minute)
	boot := 42
	h := store.ServerHost{OrganizationID: o.ID, ServerID: "cs2-1", LocationID: "do-sfo3", Desired: "running", State: "creating",
		Step: "volume", Size: "c-2", Egress: []string{"203.0.113.4/32"}, Operation: json.RawMessage(`{"id":"1","kind":"create"}`),
		EnvSHA256: []byte{1, 2, 3}, Attempts: 2, NextAttemptAt: &next, ErrorCode: "cloud_failed", LastError: "boom",
		StateSince: t0, BootSeconds: &boot}
	if err := st.SaveServerHost(ctx, h, t0); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetServerHost(ctx, o.ID, "cs2-1")
	if err != nil || got.Step != "volume" || len(got.Egress) != 1 || got.Attempts != 2 || got.NextAttemptAt == nil || !got.NextAttemptAt.Equal(next) ||
		*got.BootSeconds != 42 || string(got.EnvSHA256) != "\x01\x02\x03" || !strings.Contains(string(got.Operation), "create") {
		t.Fatalf("read back %+v %v", got, err)
	}
	// The scaling policy's columns (migration 00018).
	pendingSince, resized := t0.Add(2*time.Minute), t0.Add(-time.Hour)
	h.PinnedSize, h.PendingSize, h.PendingReason, h.PendingSince, h.LastResizeAt = "c-4", "c-4", "up: 9 of 10 players for 5m0s", &pendingSince, &resized
	if err := st.SaveServerHost(ctx, h, t0); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetServerHost(ctx, o.ID, "cs2-1")
	if err != nil || got.PinnedSize != "c-4" || got.PendingSize != "c-4" || got.PendingReason != h.PendingReason ||
		got.PendingSince == nil || !got.PendingSince.Equal(pendingSince) || got.LastResizeAt == nil || !got.LastResizeAt.Equal(resized) {
		t.Fatalf("scaling columns read back %+v %v", got, err)
	}
	// The expected population (migration 00019).
	expectedUntil := t0.Add(3 * time.Hour)
	h.ExpectedPlayers, h.ExpectedUntil, h.ExpectedReason = 10, &expectedUntil, "PUG 3 filled"
	if err := st.SaveServerHost(ctx, h, t0); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetServerHost(ctx, o.ID, "cs2-1")
	if err != nil || got.ExpectedPlayers != 10 || got.ExpectedUntil == nil || !got.ExpectedUntil.Equal(expectedUntil) || got.ExpectedReason != "PUG 3 filled" {
		t.Fatalf("expectation columns read back %+v %v", got, err)
	}
	h.ExpectedPlayers, h.ExpectedUntil, h.ExpectedReason = 0, nil, ""
	h.State, h.Step, h.Operation, h.Egress = "running", "", nil, nil
	if err := st.SaveServerHost(ctx, h, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	all, err := st.ListServerHosts(ctx, o.ID)
	if err != nil || len(all) != 1 || all[0].State != "running" || all[0].Operation != nil || all[0].Egress != nil || !all[0].CreatedAt.Equal(t0) {
		t.Fatalf("list %+v %v", all, err)
	}
	h.State = "resting"
	if err := st.SaveServerHost(ctx, h, t0); err == nil {
		t.Error("a state the table does not know was saved")
	}
}

func TestServerHostEvents(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	t0 := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	if err := st.ApplyServers(ctx, o.ID, []store.Game{{ID: "cs2"}}, nil, []store.ManagedServer{cloudServer()}, t0); err != nil {
		t.Fatal(err)
	}
	price := 0.0625
	ev := func(at time.Duration, from, to string) {
		t.Helper()
		if _, err := st.InsertServerHostEvent(ctx, store.ServerHostEvent{OrganizationID: o.ID, ServerID: "cs2-1", At: t0.Add(at),
			FromState: from, ToState: to, Size: "c-2", PriceHourlyUSD: &price}); err != nil {
			t.Fatal(err)
		}
	}
	// Running 01:00–03:00 and from 05:00 on.
	ev(0, "absent", "creating")
	ev(time.Hour, "starting", "running")
	ev(3*time.Hour, "running", "parking")
	ev(3*time.Hour+time.Minute, "parking", "parked")
	ev(5*time.Hour, "starting", "running")

	for _, c := range []struct {
		from, now time.Duration
		want      int64
	}{
		{0, 6 * time.Hour, 3 * 3600},             // 2 h + 1 h
		{2 * time.Hour, 6 * time.Hour, 2 * 3600}, // 1 h + 1 h
		{4 * time.Hour, 4*time.Hour + 1, 0},      // parked
		{0, 2 * time.Hour, 3600},                 // the open run is cut at now
		{5*time.Hour + 30*time.Minute, 6 * time.Hour, 1800},
	} {
		got, err := st.RunningSecondsSince(ctx, o.ID, "cs2-1", t0.Add(c.from), t0.Add(c.now))
		if err != nil || got != c.want {
			t.Errorf("from %s to %s: %d %v, want %d", c.from, c.now, got, err, c.want)
		}
	}

	page, err := st.ListServerHostEvents(ctx, o.ID, "cs2-1", 0, 2)
	if err != nil || len(page) != 2 || page[0].ToState != "running" || page[1].ToState != "parked" || page[0].PriceHourlyUSD == nil || *page[0].PriceHourlyUSD != price {
		t.Fatalf("page %+v %v", page, err)
	}
	older, _ := st.ListServerHostEvents(ctx, o.ID, "cs2-1", page[1].ID, 10)
	if len(older) != 3 {
		t.Errorf("older page %d", len(older))
	}
	conn, err := pgx.Connect(ctx, storetest.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })
	for _, q := range []string{`UPDATE server_host_events SET detail = 'x'`, `DELETE FROM server_host_events`, `TRUNCATE server_host_events`} {
		if _, err := conn.Exec(ctx, q); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: %v, want the append-only refusal", q, err)
		}
	}
}

// RunningCostSince prices each run at what its transition into running recorded (#37): a host
// that ran at two rungs of a ladder costs each at its own price, and an event that changes no state
// (a scaling decision, a drift) neither ends a run nor zeroes its price.
func TestServerHostRunningCost(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	t0 := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	if err := st.ApplyServers(ctx, o.ID, []store.Game{{ID: "cs2"}}, nil, []store.ManagedServer{cloudServer()}, t0); err != nil {
		t.Fatal(err)
	}
	ev := func(at time.Duration, from, to, size string, price float64) {
		t.Helper()
		e := store.ServerHostEvent{OrganizationID: o.ID, ServerID: "cs2-1", At: t0.Add(at), FromState: from, ToState: to, Size: size, Detail: "x"}
		if price > 0 {
			e.PriceHourlyUSD = &price
		}
		if _, err := st.InsertServerHostEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	// c-2 at $0.0625 from 01:00 (a scaling decision at 01:30, which prices nothing), resizing at
	// 02:00, c-4 at $0.125 from 02:05 until now.
	ev(time.Hour, "starting", "running", "c-2", 0.0625)
	ev(90*time.Minute, "running", "running", "c-2", 0)
	ev(2*time.Hour, "running", "resizing", "c-2", 0)
	ev(2*time.Hour+5*time.Minute, "starting", "running", "c-4", 0.125)
	got, err := st.RunningCostSince(ctx, o.ID, "cs2-1", t0, t0.Add(4*time.Hour+5*time.Minute))
	if want := 1*0.0625 + 2*0.125; err != nil || math.Abs(got-want) > 1e-9 {
		t.Errorf("cost %v %v, want %v", got, err, want)
	}
	if got, _ := st.RunningCostSince(ctx, o.ID, "cs2-1", t0.Add(3*time.Hour+5*time.Minute), t0.Add(4*time.Hour+5*time.Minute)); math.Abs(got-0.125) > 1e-9 {
		t.Errorf("the last hour: %v", got)
	}
	if secs, _ := st.RunningSecondsSince(ctx, o.ID, "cs2-1", t0, t0.Add(4*time.Hour+5*time.Minute)); secs != 3*3600 {
		t.Errorf("seconds %d: the same-state event neither ends nor doubles a run", secs)
	}
}
