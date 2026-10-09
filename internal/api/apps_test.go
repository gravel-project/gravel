package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/store"
)

func mustParse(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestLookupUserAsApp(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	sam := r.user(t, "Sam", "2")
	req := func() *connect.Request[hubv1.LookupUserRequest] {
		return connect.NewRequest(&hubv1.LookupUserRequest{Provider: "discord", Subject: "2"})
	}
	bot := hubv1connect.NewIdentityServiceClient(bearerClient(http.DefaultClient, r.bearer(t, "bot", "identity:read")), r.srv.URL)
	got, err := bot.LookupUser(ctx, req())
	if err != nil || got.Msg.GetUser().GetId() != sam.ID.String() {
		t.Fatalf("an app with identity:read: %v %v", err, got)
	}
	scopeless := hubv1connect.NewIdentityServiceClient(bearerClient(http.DefaultClient, r.bearer(t, "scopeless")), r.srv.URL)
	if _, err := scopeless.LookupUser(ctx, req()); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("an app without the scope: %v", err)
	}
	bad := hubv1connect.NewIdentityServiceClient(bearerClient(http.DefaultClient, "Bearer not-a-token"), r.srv.URL)
	if _, err := bad.LookupUser(ctx, req()); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("a bad token is refused before the procedure: %v", err)
	}
	// An app may not do what is the member's or the owner's.
	if _, err := bot.GetMe(ctx, connect.NewRequest(&hubv1.GetMeRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("GetMe as an app: %v", err)
	}
	orgClient := hubv1connect.NewOrganizationServiceClient(bearerClient(http.DefaultClient, r.bearer(t, "bot2", "identity:read")), r.srv.URL)
	if _, err := orgClient.UpdateOrganizationSettings(ctx, connect.NewRequest(&hubv1.UpdateOrganizationSettingsRequest{Settings: &hubv1.OrganizationSettings{Version: 1}})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("UpdateOrganizationSettings as an app: %v", err)
	}
}

func TestListUsers(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	var ids []string
	for i, name := range []string{"A", "B", "C"} {
		u := r.user(t, name, string(rune('1'+i)))
		// Distinct registration times, so the keyset order is the registration order.
		u.CreatedAt = base.Add(time.Duration(i) * time.Minute)
		r.idSt.Users[u.ID] = u
		ids = append(ids, u.ID.String())
	}
	if err := r.idSt.LinkIdentity(ctx, store.Identity{UserID: r.idSt.Users[mustParse(t, ids[1])].ID, Provider: "steam", Subject: "7656", VerificationMethod: identity.MethodOpenID, VerifiedAt: base, LinkedAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	bot := hubv1connect.NewIdentityServiceClient(bearerClient(http.DefaultClient, r.bearer(t, "bot", "identity:read")), r.srv.URL)

	page1, err := bot.ListUsers(ctx, connect.NewRequest(&hubv1.ListUsersRequest{PageSize: 2}))
	if err != nil || len(page1.Msg.GetUsers()) != 2 || page1.Msg.GetNextPageToken() == "" {
		t.Fatalf("page 1: %v %v", err, page1)
	}
	got := page1.Msg.GetUsers()
	providers := map[string]bool{}
	for _, id := range got[1].GetIdentities() {
		providers[id.GetProvider()] = true
	}
	if got[0].GetId() != ids[0] || got[1].GetId() != ids[1] || len(got[1].GetIdentities()) != 2 || !providers["steam"] || !providers["discord"] {
		t.Errorf("page 1 content: %v", got)
	}
	page2, err := bot.ListUsers(ctx, connect.NewRequest(&hubv1.ListUsersRequest{PageSize: 2, PageToken: page1.Msg.GetNextPageToken()}))
	if err != nil || len(page2.Msg.GetUsers()) != 1 || page2.Msg.GetUsers()[0].GetId() != ids[2] || page2.Msg.GetNextPageToken() != "" {
		t.Fatalf("page 2: %v %v", err, page2)
	}
	all, err := bot.ListUsers(ctx, connect.NewRequest(&hubv1.ListUsersRequest{}))
	if err != nil || len(all.Msg.GetUsers()) != 3 || all.Msg.GetNextPageToken() != "" {
		t.Errorf("default page: %v %v", err, all)
	}
	if _, err := bot.ListUsers(ctx, connect.NewRequest(&hubv1.ListUsersRequest{PageToken: "garbage"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("bad token: %v", err)
	}
	if _, err := bot.ListUsers(ctx, connect.NewRequest(&hubv1.ListUsersRequest{PageSize: -1})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("negative size: %v", err)
	}
	if _, err := hubv1connect.NewIdentityServiceClient(http.DefaultClient, r.srv.URL).ListUsers(ctx, connect.NewRequest(&hubv1.ListUsersRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("anonymous: %v", err)
	}
	member := hubv1connect.NewIdentityServiceClient(cookieClient(http.DefaultClient, r.login(t, mustParse(t, ids[0]))), r.srv.URL)
	if _, err := member.ListUsers(ctx, connect.NewRequest(&hubv1.ListUsersRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a member: %v", err)
	}
	if _, err := r.org.Claim(ctx, r.token, mustParse(t, ids[0])); err != nil {
		t.Fatal(err)
	}
	if got, err := member.ListUsers(ctx, connect.NewRequest(&hubv1.ListUsersRequest{})); err != nil || len(got.Msg.GetUsers()) != 3 {
		t.Errorf("the owner: %v %v", err, got)
	}
}

func TestListIdentityEvents(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	jo := r.user(t, "Jo", "1")
	r.user(t, "Sam", "2")
	if err := r.idSt.LinkIdentity(ctx, store.Identity{UserID: jo.ID, Provider: "steam", Subject: "7656", VerificationMethod: identity.MethodOpenID, VerifiedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	bot := hubv1connect.NewIdentityServiceClient(bearerClient(http.DefaultClient, r.bearer(t, "bot", "identity:read")), r.srv.URL)
	all, err := bot.ListIdentityEvents(ctx, connect.NewRequest(&hubv1.ListIdentityEventsRequest{}))
	if err != nil || len(all.Msg.GetEvents()) != 3 || all.Msg.GetNextAfterId() != 3 || all.Msg.GetHeadId() != 3 {
		t.Fatalf("all: %v %v", err, all)
	}
	ev := all.Msg.GetEvents()
	if ev[0].GetId() != 1 || ev[0].GetEvent() != "registered" || ev[0].GetUserId() != jo.ID.String() || ev[2].GetEvent() != "linked" || ev[2].GetProvider() != "steam" || ev[2].GetAt() == nil {
		t.Errorf("events: %v", ev)
	}
	two, err := bot.ListIdentityEvents(ctx, connect.NewRequest(&hubv1.ListIdentityEventsRequest{AfterId: 1, Limit: 1}))
	if err != nil || len(two.Msg.GetEvents()) != 1 || two.Msg.GetEvents()[0].GetId() != 2 || two.Msg.GetNextAfterId() != 2 || two.Msg.GetHeadId() != 3 {
		t.Errorf("after 1 limit 1: %v %v", err, two)
	}
	none, err := bot.ListIdentityEvents(ctx, connect.NewRequest(&hubv1.ListIdentityEventsRequest{AfterId: 3}))
	if err != nil || len(none.Msg.GetEvents()) != 0 || none.Msg.GetNextAfterId() != 3 {
		t.Errorf("after the last: %v %v", err, none)
	}
	for _, bad := range []*hubv1.ListIdentityEventsRequest{{AfterId: -1}, {Limit: -5}} {
		if _, err := bot.ListIdentityEvents(ctx, connect.NewRequest(bad)); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%v: %v", bad, err)
		}
	}
	scopeless := hubv1connect.NewIdentityServiceClient(bearerClient(http.DefaultClient, r.bearer(t, "scopeless")), r.srv.URL)
	if _, err := scopeless.ListIdentityEvents(ctx, connect.NewRequest(&hubv1.ListIdentityEventsRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("without the scope: %v", err)
	}
}
