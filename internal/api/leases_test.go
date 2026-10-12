package api_test

import (
	"context"
	"net/http"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/apps"
	"github.com/gravel-project/gravel/internal/store"
)

// Who may lease (#20). The rig has no leasable server, so a caller the service admits gets
// not_found, and one it refuses gets unauthenticated or permission_denied first.
func TestLeaseServiceCallers(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	owner := r.user(t, "Owner", "1")
	if _, err := r.org.Claim(ctx, r.token, owner.ID); err != nil {
		t.Fatal(err)
	}
	mod := r.user(t, "Mod", "3")
	if _, err := r.ids.SetRole(ctx, mod.ID, store.RoleModerator, true, owner.ID); err != nil {
		t.Fatal(err)
	}
	member := r.user(t, "Member", "2")
	client := func(c *http.Client) hubv1connect.ServerLeaseServiceClient {
		return hubv1connect.NewServerLeaseServiceClient(c, r.srv.URL)
	}
	callers := map[string]hubv1connect.ServerLeaseServiceClient{
		"anonymous":    client(http.DefaultClient),
		"member":       client(cookieClient(http.DefaultClient, r.login(t, member.ID))),
		"moderator":    client(cookieClient(http.DefaultClient, r.login(t, mod.ID))),
		"owner":        client(cookieClient(http.DefaultClient, r.login(t, owner.ID))),
		"moderate-app": client(bearerClient(http.DefaultClient, r.bearer(t, "bot", apps.ScopeServersModerate))),
		"lease-app":    client(bearerClient(http.DefaultClient, r.bearer(t, "muster", apps.ScopeServersLease))),
		"member-token": client(bearerClient(http.DefaultClient, r.memberToken(t, member.ID, []string{apps.ScopeServersRead}, apps.ScopeServersRead))),
	}
	id := uuid.NewString()
	calls := map[string]func(hubv1connect.ServerLeaseServiceClient) error{
		"request": func(c hubv1connect.ServerLeaseServiceClient) error {
			_, err := c.RequestServerLease(ctx, connect.NewRequest(&hubv1.RequestServerLeaseRequest{ServerId: "cs2-1", HolderRef: "s"}))
			return err
		},
		"get": func(c hubv1connect.ServerLeaseServiceClient) error {
			_, err := c.GetServerLease(ctx, connect.NewRequest(&hubv1.GetServerLeaseRequest{LeaseId: id}))
			return err
		},
		"release": func(c hubv1connect.ServerLeaseServiceClient) error {
			_, err := c.ReleaseServerLease(ctx, connect.NewRequest(&hubv1.ReleaseServerLeaseRequest{LeaseId: id}))
			return err
		},
		"watch": func(c hubv1connect.ServerLeaseServiceClient) error {
			s, err := c.WatchServerLease(ctx, connect.NewRequest(&hubv1.WatchServerLeaseRequest{LeaseId: id}))
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()
			for s.Receive() {
			}
			return s.Err()
		},
	}
	const (
		admitted = connect.CodeNotFound
		denied   = connect.CodePermissionDenied
		anon     = connect.CodeUnauthenticated
	)
	want := map[string]connect.Code{"anonymous": anon, "member": denied, "moderator": admitted, "owner": admitted,
		"moderate-app": denied, "lease-app": admitted, "member-token": denied}
	for who, c := range callers {
		for name, call := range calls {
			if got := connect.CodeOf(call(c)); got != want[who] {
				t.Errorf("%s %s: %v, want %v", who, name, got, want[who])
			}
		}
		// Listing admits the same callers and finds nothing.
		res, err := c.ListServerLeases(ctx, connect.NewRequest(&hubv1.ListServerLeasesRequest{}))
		if want[who] == admitted && (err != nil || len(res.Msg.GetLeases()) != 0) {
			t.Errorf("%s list: %v %v", who, res, err)
		} else if want[who] != admitted && connect.CodeOf(err) != want[who] {
			t.Errorf("%s list: %v", who, err)
		}
	}
	if _, err := callers["lease-app"].GetServerLease(ctx, connect.NewRequest(&hubv1.GetServerLeaseRequest{LeaseId: "x"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a bad id: %v", err)
	}
	if _, err := callers["owner"].RequestServerLease(ctx, connect.NewRequest(&hubv1.RequestServerLeaseRequest{ServerId: "cs2-1", HolderRef: "s",
		OnBehalfOf: &hubv1.Actor{Provider: "discord", Subject: "2"}})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("on_behalf_of in person: %v", err)
	}
	if _, err := callers["lease-app"].RequestServerLease(ctx, connect.NewRequest(&hubv1.RequestServerLeaseRequest{ServerId: "cs2-1", HolderRef: "s",
		OnBehalfOf: &hubv1.Actor{Provider: "discord", Subject: "999"}})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("on_behalf_of a stranger: %v", err)
	}
}
