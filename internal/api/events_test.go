package api_test

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/events"
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

// The feed's filters (#193): one game, intersected with a game-scoped reader's games; one actor;
// and who acted, by name.
func TestListEventsFilters(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	owner := r.user(t, "Owner", "1")
	if _, err := r.org.Claim(ctx, r.token, store.Actor{UserID: &owner.ID}); err != nil {
		t.Fatal(err)
	}
	app := uuid.New()
	at := time.Date(2026, 10, 14, 1, 0, 0, 0, time.UTC)
	r.events.evs = []store.Event{
		{ID: 1, At: at, Kind: "host.running", Severity: "info", ServerID: "cs2-1", GameID: "cs2"},
		{ID: 2, At: at, Kind: "moderation.kick", Severity: "notice", ServerID: "wd-1", GameID: "wardogs", ActorUserID: &owner.ID, ActorUserName: "Owner"},
		{ID: 3, At: at, Kind: "moderation.kick", Severity: "notice", ServerID: "cs2-1", GameID: "cs2", ActorUserID: &owner.ID, ActorAppID: &app,
			ActorUserName: "Owner", ActorAppName: "HTG Bot"},
		{ID: 4, At: at, Kind: "lease.ready", Severity: "info", ServerID: "cs2-1", GameID: "cs2", ActorAppID: &app, ActorAppName: "HTG Bot"},
	}
	org := func(uid uuid.UUID) hubv1connect.OrganizationServiceClient {
		return hubv1connect.NewOrganizationServiceClient(cookieClient(http.DefaultClient, r.login(t, uid)), r.srv.URL)
	}
	ids := func(res *connect.Response[hubv1.ListEventsResponse]) []int64 {
		var out []int64
		for _, e := range res.Msg.GetEvents() {
			out = append(out, e.GetId())
		}
		return out
	}

	got, err := org(owner.ID).ListEvents(ctx, connect.NewRequest(&hubv1.ListEventsRequest{GameId: " cs2 "}))
	if err != nil || !slices.Equal(ids(got), []int64{1, 3, 4}) || r.events.last.GameID != "cs2" {
		t.Errorf("game cs2: %v %v", got, err)
	}
	got, err = org(owner.ID).ListEvents(ctx, connect.NewRequest(&hubv1.ListEventsRequest{ActorUserId: owner.ID.String()}))
	if err != nil || !slices.Equal(ids(got), []int64{2, 3}) {
		t.Errorf("the owner's: %v %v", got, err)
	}
	got, err = org(owner.ID).ListEvents(ctx, connect.NewRequest(&hubv1.ListEventsRequest{ActorAppId: app.String()}))
	if err != nil || !slices.Equal(ids(got), []int64{3, 4}) {
		t.Errorf("the app's: %v %v", got, err)
	}
	names := []string{}
	got, _ = org(owner.ID).ListEvents(ctx, connect.NewRequest(&hubv1.ListEventsRequest{}))
	for _, e := range got.Msg.GetEvents() {
		names = append(names, e.GetActorName())
	}
	if want := []string{"", "Owner", "Owner through HTG Bot", "HTG Bot"}; !slices.Equal(names, want) {
		t.Errorf("actor names %q, want %q", names, want)
	}
	for _, bad := range []*hubv1.ListEventsRequest{{ActorUserId: "jo"}, {ActorAppId: "123"}} {
		if _, err := org(owner.ID).ListEvents(ctx, connect.NewRequest(bad)); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%v: %v, want invalid_argument", bad, err)
		}
	}

	// A CS2 operator asking for War Dogs gets nothing: the game filter never widens a scope.
	op := r.user(t, "Op", "2")
	if _, err := hubv1connect.NewIdentityServiceClient(cookieClient(http.DefaultClient, r.login(t, owner.ID)), r.srv.URL).SetUserRole(ctx,
		connect.NewRequest(&hubv1.SetUserRoleRequest{UserId: op.ID.String(), Role: store.RoleOperator, GameId: "cs2", Granted: true})); err != nil {
		t.Fatal(err)
	}
	got, err = org(op.ID).ListEvents(ctx, connect.NewRequest(&hubv1.ListEventsRequest{GameId: "wardogs"}))
	if err != nil || len(got.Msg.GetEvents()) != 0 || !slices.Equal(r.events.last.Games, []string{"cs2"}) {
		t.Errorf("the CS2 operator asking for War Dogs: %v %v (%+v)", got, err, r.events.last)
	}
}

// ListEventKinds is the registry, for a filter, to whoever may read some of the log.
func TestListEventKinds(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	owner := r.user(t, "Owner", "1")
	if _, err := r.org.Claim(ctx, r.token, store.Actor{UserID: &owner.ID}); err != nil {
		t.Fatal(err)
	}
	got, err := hubv1connect.NewOrganizationServiceClient(cookieClient(http.DefaultClient, r.login(t, owner.ID)), r.srv.URL).
		ListEventKinds(ctx, connect.NewRequest(&hubv1.ListEventKindsRequest{}))
	if err != nil || len(got.Msg.GetKinds()) != len(events.Kinds()) {
		t.Fatalf("%v %v", got, err)
	}
	for i, k := range got.Msg.GetKinds() {
		want := events.Kinds()[i]
		if k.GetName() != want.Name || k.GetSeverity() != string(want.Severity) || k.GetAbout() != want.About || k.GetGroup() != events.Group(want.Name) {
			t.Errorf("kind %d: %v, want %+v", i, k, want)
		}
	}
	member := r.user(t, "Member", "3")
	if _, err := hubv1connect.NewOrganizationServiceClient(cookieClient(http.DefaultClient, r.login(t, member.ID)), r.srv.URL).
		ListEventKinds(ctx, connect.NewRequest(&hubv1.ListEventKindsRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a member: %v", err)
	}
}
