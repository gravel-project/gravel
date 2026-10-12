package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/apps"
	"github.com/gravel-project/gravel/internal/store"
)

// Who may do what to a host. The rig has no cloud-VM server, so a caller the service admits gets
// not_found, and one it refuses gets unauthenticated or permission_denied first.
func TestHostServiceCallers(t *testing.T) {
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
	client := func(c *http.Client) hubv1connect.ServerHostServiceClient {
		return hubv1connect.NewServerHostServiceClient(c, r.srv.URL)
	}
	callers := map[string]hubv1connect.ServerHostServiceClient{
		"anonymous": client(http.DefaultClient),
		"member":    client(cookieClient(http.DefaultClient, r.login(t, member.ID))),
		"moderator": client(cookieClient(http.DefaultClient, r.login(t, mod.ID))),
		"owner":     client(cookieClient(http.DefaultClient, r.login(t, owner.ID))),
		"reader":    client(bearerClient(http.DefaultClient, r.bearer(t, "reader", apps.ScopeServersRead))),
		"bot":       client(bearerClient(http.DefaultClient, r.bearer(t, "bot", apps.ScopeServersModerate))),
	}
	type call func(hubv1connect.ServerHostServiceClient) error
	calls := map[string]call{
		"get": func(c hubv1connect.ServerHostServiceClient) error {
			_, err := c.GetServerHost(ctx, connect.NewRequest(&hubv1.GetServerHostRequest{ServerId: "wd-1"}))
			return err
		},
		"park": func(c hubv1connect.ServerHostServiceClient) error {
			_, err := c.SetServerHostDesired(ctx, connect.NewRequest(&hubv1.SetServerHostDesiredRequest{ServerId: "wd-1", Desired: "parked"}))
			return err
		},
		"expect": func(c hubv1connect.ServerHostServiceClient) error {
			_, err := c.SetServerHostExpectation(ctx, connect.NewRequest(&hubv1.SetServerHostExpectationRequest{ServerId: "wd-1", Players: 10, Until: timestamppb.New(time.Now().Add(time.Hour))}))
			return err
		},
		"resize": func(c hubv1connect.ServerHostServiceClient) error {
			_, err := c.ResizeServerHost(ctx, connect.NewRequest(&hubv1.ResizeServerHostRequest{ServerId: "wd-1", Size: "c-4"}))
			return err
		},
		"unpin": func(c hubv1connect.ServerHostServiceClient) error {
			_, err := c.UnpinServerHostSize(ctx, connect.NewRequest(&hubv1.UnpinServerHostSizeRequest{ServerId: "wd-1"}))
			return err
		},
		"snapshot": func(c hubv1connect.ServerHostServiceClient) error {
			_, err := c.SnapshotServerHostVolume(ctx, connect.NewRequest(&hubv1.SnapshotServerHostVolumeRequest{ServerId: "wd-1"}))
			return err
		},
		"destroy": func(c hubv1connect.ServerHostServiceClient) error {
			_, err := c.DestroyServerHost(ctx, connect.NewRequest(&hubv1.DestroyServerHostRequest{ServerId: "wd-1"}))
			return err
		},
	}
	const (
		admitted = connect.CodeNotFound
		denied   = connect.CodePermissionDenied
		anon     = connect.CodeUnauthenticated
	)
	want := map[string]map[string]connect.Code{
		"anonymous": {"get": anon, "park": anon, "expect": anon, "resize": anon, "unpin": anon, "snapshot": anon, "destroy": anon},
		"member":    {"get": admitted, "park": denied, "expect": denied, "resize": denied, "unpin": denied, "snapshot": denied, "destroy": denied},
		"moderator": {"get": admitted, "park": admitted, "expect": admitted, "resize": denied, "unpin": denied, "snapshot": denied, "destroy": denied},
		"owner":     {"get": admitted, "park": admitted, "expect": admitted, "resize": admitted, "unpin": admitted, "snapshot": admitted, "destroy": admitted},
		"reader":    {"get": admitted, "park": denied, "expect": denied, "resize": denied, "unpin": denied, "snapshot": denied, "destroy": denied},
		"bot":       {"get": denied, "park": admitted, "expect": admitted, "resize": denied, "unpin": denied, "snapshot": denied, "destroy": denied},
	}
	for who, c := range callers {
		for name, f := range calls {
			if got := connect.CodeOf(f(c)); got != want[who][name] {
				t.Errorf("%s %s: %v, want %v", who, name, got, want[who][name])
			}
		}
	}
	if _, err := callers["owner"].SetServerHostDesired(ctx, connect.NewRequest(&hubv1.SetServerHostDesiredRequest{ServerId: "wd-1", Desired: "off"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a desired state that is not one: %v", err)
	}
	if _, err := callers["owner"].ListServerHostEvents(ctx, connect.NewRequest(&hubv1.ListServerHostEventsRequest{ServerId: "wd-1", PageToken: "bm9wZQ"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a page token the hub never issued: %v", err)
	}
	if n := len(r.audit.Entries()); n != 0 {
		t.Errorf("%d audit entries for servers that are not cloud-VM servers", n)
	}
}
