package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/store"
)

// fakeFleet is a fleet snapshot and its alarms as a test sets them.
type fakeFleet struct {
	snap   []servers.FleetServer
	alarms []servers.Alarm
}

func (f *fakeFleet) Snapshot(context.Context) ([]servers.FleetServer, error) { return f.snap, nil }

func (f *fakeFleet) Alarms(context.Context, []servers.FleetServer) ([]servers.Alarm, error) {
	return f.alarms, nil
}

// The fleet summary: totals over the caller's servers (idle ones' slots are no capacity), the rows
// in the fleet's order, and the alarms the caller may see (gravel#190).
func TestGetFleetSummary(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	obs := func(players, slots int) servers.Observation {
		o := servers.Observation{State: servers.StateOK, Reachable: true, ObservedAt: at, LastPollDuration: 120 * time.Millisecond}
		o.Status.Players, o.Status.MaxPlayers, o.Status.Map = players, slots, "de_dust2"
		return o
	}
	down := servers.Observation{State: servers.StateUnreachable, Misses: 3, FailingSince: at.Add(-5 * time.Minute)}
	r.fleet.snap = []servers.FleetServer{
		{Server: servers.Server{ID: "cs2-2", Name: "CS2 #2", Game: "cs2", Driver: "cloudvm"}, GameName: "Counter-Strike 2", Observation: down,
			Verdict: servers.Verdict{Health: servers.HealthDown, Reason: "not answering", Since: at.Add(-5 * time.Minute)}},
		{Server: servers.Server{ID: "wd-1", Name: "War Dogs #1", Game: "wardogs", Driver: "wardogs"}, GameName: "War Dogs", Observation: obs(24, 80),
			Verdict: servers.Verdict{Health: servers.HealthUp, Reason: "answering"}},
		{Server: servers.Server{ID: "cs2-1", Name: "CS2 #1", Game: "cs2", Driver: "cloudvm"}, GameName: "Counter-Strike 2", Observation: obs(0, 10),
			Host: &servers.HostHealth{Desired: "parked", State: "parked"}, LeaseState: "",
			Verdict: servers.Verdict{Health: servers.HealthIdle, Reason: "parked, as asked"}},
	}
	r.fleet.alarms = []servers.Alarm{
		{Kind: "hub.unowned", Severity: servers.AlarmWarning, Since: at.Add(-time.Hour)},
		{Kind: servers.AlarmServerDown, Severity: servers.AlarmCritical, ServerID: "cs2-2", Since: at.Add(-5 * time.Minute)},
	}

	anon := hubv1connect.NewServerServiceClient(http.DefaultClient, r.srv.URL)
	if _, err := anon.GetFleetSummary(ctx, connect.NewRequest(&hubv1.GetFleetSummaryRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("anonymous: %v", err)
	}

	// A member reads the fleet but not the hub's own alarms.
	jo := r.user(t, "Jo", "1")
	member := hubv1connect.NewServerServiceClient(cookieClient(http.DefaultClient, r.login(t, jo.ID)), r.srv.URL)
	got, err := member.GetFleetSummary(ctx, connect.NewRequest(&hubv1.GetFleetSummaryRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	tot := got.Msg.GetTotals()
	if tot.GetServers() != 3 || tot.GetUp() != 1 || tot.GetDown() != 1 || tot.GetIdle() != 1 || tot.GetPlayers() != 24 || tot.GetCapacity() != 80 {
		t.Errorf("totals: %v", tot)
	}
	var ids []string
	for _, s := range got.Msg.GetServers() {
		ids = append(ids, s.GetId())
	}
	if len(ids) != 3 || ids[0] != "cs2-2" || ids[1] != "wd-1" || ids[2] != "cs2-1" {
		t.Errorf("order: %v", ids)
	}
	first, idle := got.Msg.GetServers()[0], got.Msg.GetServers()[2]
	if first.GetHealth() != "down" || first.GetReason() != "not answering" || first.GetMisses() != 3 || first.GetObservedAt() != nil || first.GetHealthSince() == nil {
		t.Errorf("a down server: %v", first)
	}
	if idle.GetHostState() != "parked" || idle.GetGameName() != "Counter-Strike 2" || got.Msg.GetServers()[1].GetPollMs() != 120 {
		t.Errorf("rows: %v", got.Msg.GetServers())
	}
	if a := got.Msg.GetAlarms(); len(a) != 1 || a[0].GetKind() != servers.AlarmServerDown || a[0].GetServerId() != "cs2-2" {
		t.Errorf("a member's alarms: %v", a)
	}

	// The owner reads every alarm, critical first.
	if _, err := r.org.Claim(ctx, r.token, store.Actor{UserID: &jo.ID}); err != nil {
		t.Fatal(err)
	}
	got, err = member.GetFleetSummary(ctx, connect.NewRequest(&hubv1.GetFleetSummaryRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if a := got.Msg.GetAlarms(); len(a) != 2 || a[0].GetSeverity() != "critical" || a[1].GetKind() != "hub.unowned" {
		t.Errorf("the owner's alarms: %v", a)
	}
}
