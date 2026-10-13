package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/store"
)

// ListEvents reads the log by cursor with events:read; a caller holding it for one game reads that
// game's events (ADR-0028).
func TestListEvents(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	owner := r.user(t, "Owner", "1")
	if _, err := r.org.Claim(ctx, r.token, store.Actor{UserID: &owner.ID}); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 13, 1, 0, 0, 0, time.UTC)
	r.events.evs = []store.Event{
		{ID: 1, At: at, Kind: "identity.registered", Severity: "info", Summary: "Jo: registered (discord)", Detail: map[string]any{"provider": "discord"}},
		{ID: 2, At: at, Kind: "host.running", Severity: "info", ServerID: "cs2-1", GameID: "cs2", Summary: "cs2-1: starting → running"},
		{ID: 3, At: at, Kind: "moderation.kick", Severity: "notice", ServerID: "wd-1", GameID: "wardogs", Summary: "kick on wd-1: ok", ActorUserID: &owner.ID},
	}
	org := func(c *http.Client) hubv1connect.OrganizationServiceClient {
		return hubv1connect.NewOrganizationServiceClient(c, r.srv.URL)
	}

	got, err := org(cookieClient(http.DefaultClient, r.login(t, owner.ID))).ListEvents(ctx, connect.NewRequest(&hubv1.ListEventsRequest{}))
	if err != nil || len(got.Msg.GetEvents()) != 3 || got.Msg.GetHeadId() != 3 || got.Msg.GetNextAfterId() != 3 || r.events.last.Games != nil ||
		got.Msg.GetEvents()[2].GetActorUserId() != owner.ID.String() || got.Msg.GetEvents()[0].GetDetail()["provider"] != "discord" {
		t.Fatalf("the owner: %v %v", got, err)
	}
	if _, err := org(cookieClient(http.DefaultClient, r.login(t, owner.ID))).ListEvents(ctx, connect.NewRequest(&hubv1.ListEventsRequest{MinSeverity: "loud"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a severity that isn't one: %v", err)
	}

	// An operator for CS2 reads CS2's events; a plain member reads none.
	op := r.user(t, "Op", "2")
	if _, err := hubv1connect.NewIdentityServiceClient(cookieClient(http.DefaultClient, r.login(t, owner.ID)), r.srv.URL).SetUserRole(ctx,
		connect.NewRequest(&hubv1.SetUserRoleRequest{UserId: op.ID.String(), Role: store.RoleOperator, GameId: "cs2", Granted: true})); err != nil {
		t.Fatal(err)
	}
	got, err = org(cookieClient(http.DefaultClient, r.login(t, op.ID))).ListEvents(ctx, connect.NewRequest(&hubv1.ListEventsRequest{AfterId: 1}))
	if err != nil || len(got.Msg.GetEvents()) != 1 || got.Msg.GetEvents()[0].GetKind() != "host.running" || len(r.events.last.Games) != 1 || r.events.last.AfterID != 1 {
		t.Errorf("the CS2 operator: %v %v (%+v)", got, err, r.events.last)
	}
	member := r.user(t, "Member", "3")
	if _, err := org(cookieClient(http.DefaultClient, r.login(t, member.ID))).ListEvents(ctx, connect.NewRequest(&hubv1.ListEventsRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a member: %v", err)
	}
	if _, err := org(bearerClient(http.DefaultClient, r.bearer(t, "notifier", "events:read"))).ListEvents(ctx, connect.NewRequest(&hubv1.ListEventsRequest{})); err != nil {
		t.Errorf("an app with events:read: %v", err)
	}
}
