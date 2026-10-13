package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gravel-project/gravel/drivers/cloudvm"
	"github.com/gravel-project/gravel/internal/events"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

// Every kind a writer can produce is in the registry, with a severity (ADR-0028).
func TestEveryWrittenKindIsRegistered(t *testing.T) {
	var kinds []string
	for _, a := range []string{servers.ActionKick, servers.ActionBan, servers.ActionUnban, servers.ActionMessage, servers.ActionBroadcast, servers.ActionMovePlayer,
		servers.ActionApplyConfig, servers.ActionHostPark, servers.ActionHostUnpark, servers.ActionHostResize, servers.ActionHostUnpin, servers.ActionHostSnapshot,
		servers.ActionHostExpect, servers.ActionHostDestroy, servers.ActionLeaseRequest, servers.ActionLeaseRelease} {
		kinds = append(kinds, store.AuditEventKind(a))
	}
	states := []string{cloudvm.StateAbsent, cloudvm.StateCreating, cloudvm.StateBooting, cloudvm.StateStarting, cloudvm.StateRunning,
		cloudvm.StateParking, cloudvm.StateParked, cloudvm.StateResizing, cloudvm.StateDestroying}
	for _, to := range states {
		kinds = append(kinds, store.HostEventKind("x", to, ""))
	}
	for _, step := range []string{cloudvm.DetailExpect, cloudvm.DetailSnapshot, cloudvm.DetailScale, cloudvm.DetailScaleCancelled, cloudvm.DetailEnvDrift, "anything"} {
		kinds = append(kinds, store.HostEventKind("running", "running", step))
	}
	kinds = append(kinds, store.HostEventKind("resizing", "running", ""), "host.failing", "host.recovered")
	for _, s := range []string{store.LeaseStarting, store.LeaseReady, store.LeaseEnding, store.LeaseReleased, store.LeaseFailed} {
		kinds = append(kinds, "lease."+s)
	}
	for _, e := range []string{store.IdentityEventRegistered, store.IdentityEventLinked, store.IdentityEventUnlinked, store.IdentityEventProven,
		store.IdentityEventPasswordSet, store.IdentityEventPasswordReset, store.IdentityEventPasswordLinkIssued} {
		kinds = append(kinds, "identity."+e)
	}
	kinds = append(kinds, "role.granted", "role.revoked", "match.ended", "backup.failed", "backup.stale", "backup.recovered")
	for _, k := range kinds {
		if _, err := events.SeverityOf(k); err != nil {
			t.Error(err)
		}
	}
	for _, k := range events.Kinds() {
		if events.Rank(k.Severity) < 0 || !strings.Contains(k.Name, ".") || k.About == "" {
			t.Errorf("registry entry %+v", k)
		}
	}
}

func eventKinds(t *testing.T, st *store.Store, orgID uuid.UUID, q store.EventQuery) []string {
	t.Helper()
	evs, _, err := st.ListEvents(context.Background(), orgID, q)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

func TestEventsLog(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	var counted []string
	st.OnEvent(func(kind string) { counted = append(counted, kind) })
	t0 := time.Date(2026, 10, 13, 1, 0, 0, 0, time.UTC)
	if err := st.ApplyServers(ctx, o.ID, []store.Game{{ID: "cs2"}}, nil, []store.ManagedServer{cloudServer()}, t0); err != nil {
		t.Fatal(err)
	}

	// Identity: a registration is an event, a login is not.
	jo, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: "Jo"}, ident("discord", "1", t0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordLogin(ctx, "discord", "1", "Jo", "", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// A role, scoped to CS2.
	owner, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: "Owner"}, ident("discord", "2", t0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetRoleGrant(ctx, jo.ID, store.RoleOperator, "cs2", true, store.RoleChange{By: &owner.ID, At: t0.Add(2 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	// The host: a transition, a failing step and its recovery, a resize.
	if _, err := st.InsertServerHostEvent(ctx, store.ServerHostEvent{OrganizationID: o.ID, ServerID: "cs2-1", At: t0.Add(3 * time.Minute), FromState: "starting", ToState: "running", Size: "c-2"}); err != nil {
		t.Fatal(err)
	}
	h := store.ServerHost{OrganizationID: o.ID, ServerID: "cs2-1", LocationID: "do-sfo3", Desired: "running", State: "running", Step: "push", Size: "c-2", StateSince: t0}
	for _, code := range []string{"", "ssh_failed", "ssh_failed", ""} {
		h.ErrorCode = code
		if err := st.SaveServerHost(ctx, h, t0.Add(4*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.InsertServerHostEvent(ctx, store.ServerHostEvent{OrganizationID: o.ID, ServerID: "cs2-1", At: t0.Add(5 * time.Minute), FromState: "resizing", ToState: "running", Size: "c-4"}); err != nil {
		t.Fatal(err)
	}
	// A lease taken and made ready; a state written again is no new event.
	l := store.ServerLease{ID: uuid.New(), OrganizationID: o.ID, ServerID: "cs2-1", Pool: "pug", HolderKind: store.LeaseHolderApp, Holder: "app",
		HolderRef: "r1", State: store.LeaseStarting, CreatedAt: t0.Add(6 * time.Minute), MaxUntil: t0.Add(5 * time.Hour)}
	if err := st.CreateServerLease(ctx, l); err != nil {
		t.Fatal(err)
	}
	ready := t0.Add(7 * time.Minute)
	l.State, l.ReadyAt = store.LeaseReady, &ready
	for range 2 {
		if err := st.UpdateServerLease(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
	// A moderation call's outcome.
	id, err := st.InsertAuditEntry(ctx, store.AuditEntry{OrganizationID: o.ID, At: t0.Add(8 * time.Minute), ActorUserID: &owner.ID, ServerID: "cs2-1", Action: "kick", RequestID: "req-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishAuditEntry(ctx, o.ID, id, "ok", t0.Add(8*time.Minute)); err != nil {
		t.Fatal(err)
	}

	want := "identity.registered identity.registered role.granted host.running host.failing host.recovered host.resized lease.starting lease.ready moderation.kick"
	all := eventKinds(t, st, o.ID, store.EventQuery{AfterID: 0, Limit: 500})
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	if got := strings.Join(all, " "); got != want {
		t.Fatalf("the log:\n got %s\nwant %s", got, want)
	}
	if strings.Join(counted, " ") != want {
		t.Errorf("OnEvent saw %v", counted)
	}
	evs, head, err := st.ListEvents(ctx, o.ID, store.EventQuery{Kinds: []string{"role"}})
	if err != nil || len(evs) != 1 || evs[0].GameID != "cs2" || evs[0].SubjectUserID == nil || *evs[0].SubjectUserID != jo.ID ||
		*evs[0].ActorUserID != owner.ID || evs[0].Summary != "Jo was granted operator for cs2" || evs[0].Severity != "notice" || head == 0 {
		t.Errorf("the grant event: %+v %d %v", evs, head, err)
	}
	kick, _, _ := st.ListEvents(ctx, o.ID, store.EventQuery{Kinds: []string{"moderation.kick"}})
	if len(kick) != 1 || kick[0].GameID != "cs2" || kick[0].RequestID != "req-1" || kick[0].Summary != "kick on cs2-1: ok" {
		t.Errorf("the kick event: %+v", kick)
	}

	// Cursors: after an id, oldest first; before an id, newest first; filters compose.
	first := eventKinds(t, st, o.ID, store.EventQuery{AfterID: head - 3, Limit: 2})
	if strings.Join(first, " ") != "lease.starting lease.ready" {
		t.Errorf("after: %v", first)
	}
	if got := eventKinds(t, st, o.ID, store.EventQuery{BeforeID: head, Limit: 1}); strings.Join(got, " ") != "lease.ready" {
		t.Errorf("before: %v", got)
	}
	if got := eventKinds(t, st, o.ID, store.EventQuery{MinSeverity: events.Warning}); strings.Join(got, " ") != "host.failing" {
		t.Errorf("min severity: %v", got)
	}
	if got := eventKinds(t, st, o.ID, store.EventQuery{ServerID: "cs2-1", Kinds: []string{"host"}, Limit: 500}); len(got) != 4 {
		t.Errorf("one server's host events: %v", got)
	}
	if got := eventKinds(t, st, o.ID, store.EventQuery{Games: []string{"wardogs"}}); len(got) != 0 {
		t.Errorf("another game's reader sees: %v", got)
	}
	if got := eventKinds(t, st, o.ID, store.EventQuery{Games: []string{}}); len(got) != 0 {
		t.Errorf("a reader with no games sees: %v", got)
	}
	if got, h2, _ := st.ListEvents(ctx, o.ID, store.EventQuery{AfterID: head}); len(got) != 0 || h2 != head {
		t.Errorf("at the head: %v %d", got, h2)
	}

	// An unknown kind fails and writes nothing; a source row already in the log is skipped.
	if err := st.AppendEvent(ctx, store.Event{OrganizationID: o.ID, At: t0, Kind: "made.up", Summary: "x", Source: "test"}); err == nil {
		t.Error("an unknown kind was written")
	}
	b := store.Event{OrganizationID: o.ID, At: t0, Kind: "backup.failed", Summary: "x", Source: "backup", SourceID: "failed:1"}
	if err := st.AppendEvent(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, b); err != nil {
		t.Fatal(err)
	}
	if got := eventKinds(t, st, o.ID, store.EventQuery{Kinds: []string{"backup"}}); len(got) != 1 {
		t.Errorf("a replayed source: %v", got)
	}

	// Append-only: no update, no delete, no truncate; only the prune deletes, and only the old.
	conn, err := pgx.Connect(ctx, storetest.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	for _, q := range []string{`UPDATE events SET summary = 'x'`, `DELETE FROM events`, `TRUNCATE events`} {
		if _, err := conn.Exec(ctx, q); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: %v", q, err)
		}
	}
	n, err := st.PruneEvents(ctx, o.ID, t0.Add(150*time.Second))
	if err != nil || n != 4 { // the two registrations, the grant, the failed backup at t0
		t.Errorf("prune: %d %v", n, err)
	}
	if got := eventKinds(t, st, o.ID, store.EventQuery{MinSeverity: events.Critical}); len(got) != 0 {
		t.Errorf("after the prune: %v", got)
	}
	if got := eventKinds(t, st, o.ID, store.EventQuery{MinSeverity: events.Warning}); strings.Join(got, " ") != "host.failing" {
		t.Errorf("an event inside the window was pruned: %v", got)
	}
}

// The migration's backfill turns the history the hub already kept into events, oldest first, and
// a second run adds nothing.
func TestEventsBackfill(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	t0 := time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC)
	if err := st.ApplyServers(ctx, o.ID, []store.Game{{ID: "cs2"}}, nil, []store.ManagedServer{cloudServer()}, t0); err != nil {
		t.Fatal(err)
	}
	jo, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: "Jo"}, ident("discord", "1", t0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetRoleGrant(ctx, jo.ID, store.RoleModerator, "", true, store.RoleChange{At: t0.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertServerHostEvent(ctx, store.ServerHostEvent{OrganizationID: o.ID, ServerID: "cs2-1", At: t0.Add(30 * time.Minute), FromState: "absent", ToState: "creating"}); err != nil {
		t.Fatal(err)
	}
	before := eventKinds(t, st, o.ID, store.EventQuery{Limit: 500})
	if err := st.MigrateDownTo(ctx, 25); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	evs, _, err := st.ListEvents(ctx, o.ID, store.EventQuery{AfterID: 0, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for i := len(evs) - 1; i >= 0; i-- {
		kinds = append(kinds, evs[i].Kind)
		if i < len(evs)-1 && evs[i].At.Before(evs[i+1].At) {
			t.Errorf("not oldest first: %v before %v", evs[i+1], evs[i])
		}
	}
	// Oldest first across every source: the registration, the host's start, the grant.
	if got := strings.Join(kinds, " "); got != "identity.registered host.creating role.granted" || len(before) != 3 {
		t.Errorf("backfilled: %s (before the migration: %v)", got, before)
	}
	reg := evs[len(evs)-1]
	if reg.Summary != "Jo: registered (discord)" || reg.Source != "identity_events" {
		t.Errorf("a backfilled registration: %+v", reg)
	}
}
