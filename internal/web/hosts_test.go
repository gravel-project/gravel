package web_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
)

// fakeHosts is ServerHostService with one cloud-VM server's host; it records the requests.
type fakeHosts struct {
	hubv1connect.UnimplementedServerHostServiceHandler
	mu    sync.Mutex
	host  *hubv1.ServerHost
	calls []string
	err   connect.Code
}

func newFakeHosts() *fakeHosts {
	return &fakeHosts{host: &hubv1.ServerHost{ServerId: "htg-cs2-1", Desired: "running", State: "running", Size: "c-2", Address: "203.0.113.50",
		AddressReserved: true, HourlyPriceUsd: 0.0625, RunningSecondsMonth: 7200, MonthCostUsd: 0.125, BootSeconds: 70, ReadySeconds: 1500, ParkAfterSeconds: 1800,
		Sizes: []*hubv1.HostSize{{Slug: "c-4", Vcpus: 4, MemoryMib: 8192, PriceHourlyUsd: 0.125}}}}
}

func (f *fakeHosts) GetServerHost(_ context.Context, req *connect.Request[hubv1.GetServerHostRequest]) (*connect.Response[hubv1.GetServerHostResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.Msg.GetServerId() != f.host.GetServerId() {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("not a cloud-VM server"))
	}
	return connect.NewResponse(&hubv1.GetServerHostResponse{Host: f.host}), nil
}

func (f *fakeHosts) record(c string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	if f.err != 0 {
		return connect.NewError(f.err, errors.New("players are on the server; pass force to resize anyway"))
	}
	return nil
}

func (f *fakeHosts) SetServerHostDesired(_ context.Context, req *connect.Request[hubv1.SetServerHostDesiredRequest]) (*connect.Response[hubv1.SetServerHostDesiredResponse], error) {
	if err := f.record("desired " + req.Msg.GetDesired()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.SetServerHostDesiredResponse{AuditId: 1}), nil
}

func (f *fakeHosts) ResizeServerHost(_ context.Context, req *connect.Request[hubv1.ResizeServerHostRequest]) (*connect.Response[hubv1.ResizeServerHostResponse], error) {
	force := ""
	if req.Msg.GetForce() {
		force = " force"
	}
	if err := f.record("resize " + req.Msg.GetSize() + force); err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.ResizeServerHostResponse{AuditId: 2}), nil
}

func (f *fakeHosts) DestroyServerHost(_ context.Context, req *connect.Request[hubv1.DestroyServerHostRequest]) (*connect.Response[hubv1.DestroyServerHostResponse], error) {
	if err := f.record("destroy " + map[bool]string{true: "all", false: "keep"}[req.Msg.GetDeleteVolume()]); err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.DestroyServerHostResponse{AuditId: 3}), nil
}

func (f *fakeHosts) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func withCS2(r *rig) {
	r.servers.mu.Lock()
	defer r.servers.mu.Unlock()
	r.servers.games = append(r.servers.games, &hubv1.Game{Id: "cs2", Name: "Counter-Strike 2"})
	r.servers.servers = append(r.servers.servers, &hubv1.Server{Id: "htg-cs2-1", Name: "HTG CS2", GameId: "cs2", Location: "do-sfo3", Trust: "official", Driver: "cloudvm",
		Status: &hubv1.ServerStatus{State: "ok", Reachable: true, Capabilities: []string{"status", "players", "kick", "broadcast", "bans", "ban", "unban", "park", "resize"},
			Join: &hubv1.ServerJoin{Method: "address", Value: "203.0.113.50:27015", Hint: "In CS2's console: connect 203.0.113.50:27015"}}})
}

func TestServerPageHostPanel(t *testing.T) {
	r := newRig(t)
	withCS2(r)
	member := browser(t)
	r.loginAs(t, member, "sam")
	if _, body := get(t, member, r.srv.URL+"/servers/htg-cs2-1"); strings.Contains(body, "host-heading") {
		t.Error("a member sees the machine")
	}
	if _, body := get(t, browser(t), r.srv.URL+"/servers/htg-cs2-1"); strings.Contains(body, "host-heading") {
		t.Error("anyone sees the machine")
	}
	c := moderator(t, r) // the owner
	_, body := get(t, c, r.srv.URL+"/servers/htg-cs2-1")
	for _, want := range []string{"Server machine", "Running", "c-2", "203.0.113.50", "reserved", "$0.06 an hour", "$0.12 so far this month", "2.0 hours",
		"Park the server", `<option value="c-4">c-4 · 4 vCPU · 8 GB · $0.125/h</option>`, "after 30m0s with nobody on", "playable in 25m0s"} {
		if !strings.Contains(body, want) {
			t.Errorf("the owner's page lacks %q", want)
		}
	}
	if strings.Contains(body, "Start the server") || strings.Contains(body, "Destroy the machine") {
		t.Error("a running host offers start or destroy")
	}
	// A War Dogs server has no machine panel.
	if _, body := get(t, c, r.srv.URL+"/servers/htg-wardogs-1"); strings.Contains(body, "host-heading") {
		t.Error("a War Dogs server shows a machine")
	}
	// Parked: start and destroy, no park or resize.
	r.hosts.mu.Lock()
	r.hosts.host.State, r.hosts.host.Desired, r.hosts.host.Sizes = "parked", "parked", nil
	r.hosts.mu.Unlock()
	_, body = get(t, c, r.srv.URL+"/servers/htg-cs2-1")
	if !strings.Contains(body, "Start the server") || !strings.Contains(body, "Destroy the machine") || strings.Contains(body, "Park the server") || strings.Contains(body, "host-size") {
		t.Errorf("parked panel: %s", body)
	}
}

func TestHostActionsConfirmThenSend(t *testing.T) {
	r := newRig(t)
	withCS2(r)
	c := moderator(t, r)
	_, page := get(t, c, r.srv.URL+"/servers/htg-cs2-1")
	csrf := csrfOf(t, page)
	post1 := func(v url.Values) (reply, string) {
		v.Set("_csrf", csrf)
		return post(t, c, r.srv.URL+"/servers/htg-cs2-1/host", v, nil)
	}
	resp, body := post1(url.Values{"action": {"resize"}, "size": {"c-4"}})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Resize HTG CS2 to c-4?") || !strings.Contains(body, `name="force"`) || len(r.hosts.recorded()) != 0 {
		t.Fatalf("resize confirmation: %d %v %s", resp.StatusCode, r.hosts.recorded(), body)
	}
	resp, _ = post1(url.Values{"action": {"resize"}, "size": {"c-4"}, "force": {"1"}, "confirm": {"1"}})
	if resp.StatusCode != http.StatusSeeOther || !slices.Equal(r.hosts.recorded(), []string{"resize c-4 force"}) {
		t.Fatalf("resize: %d %v", resp.StatusCode, r.hosts.recorded())
	}
	post1(url.Values{"action": {"park"}, "confirm": {"1"}})
	post1(url.Values{"action": {"unpark"}, "confirm": {"1"}})
	_, body = post1(url.Values{"action": {"destroy"}})
	if !strings.Contains(body, `name="delete_volume"`) {
		t.Error("the destroy confirmation offers no choice about the disk")
	}
	post1(url.Values{"action": {"destroy"}, "confirm": {"1"}})
	if got := r.hosts.recorded(); !slices.Equal(got, []string{"resize c-4 force", "desired parked", "desired running", "destroy keep"}) {
		t.Errorf("calls %v", got)
	}
	if resp, _ := post1(url.Values{"action": {"explode"}}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("an unknown action: %d", resp.StatusCode)
	}
	r.hosts.mu.Lock()
	r.hosts.err = connect.CodeFailedPrecondition
	r.hosts.mu.Unlock()
	post1(url.Values{"action": {"resize"}, "size": {"c-4"}, "confirm": {"1"}})
	if _, body := get(t, c, r.srv.URL+"/servers/htg-cs2-1"); !strings.Contains(body, "Nothing done: players are on the server") {
		t.Errorf("refusal not shown: %s", body)
	}
	r.hosts.mu.Lock()
	r.hosts.err = connect.CodePermissionDenied
	r.hosts.mu.Unlock()
	if resp, _ := post1(url.Values{"action": {"park"}, "confirm": {"1"}}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("refused by the API: %d", resp.StatusCode)
	}
}
