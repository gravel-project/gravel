package api_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/store"
)

func TestGetMeAndUnlink(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	anon := hubv1connect.NewIdentityServiceClient(http.DefaultClient, r.srv.URL)
	if _, err := anon.GetMe(ctx, connect.NewRequest(&hubv1.GetMeRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("anonymous GetMe: %v", err)
	}

	jo := r.user(t, "Jo", "1")
	if err := r.idSt.LinkIdentity(ctx, store.Identity{UserID: jo.ID, Provider: "steam", Subject: "7656", DisplayName: "JoSteam", VerificationMethod: identity.MethodOpenID, VerifiedAt: time.Now().Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	client := hubv1connect.NewIdentityServiceClient(cookieClient(http.DefaultClient, r.login(t, jo.ID)), r.srv.URL)
	me, err := client.GetMe(ctx, connect.NewRequest(&hubv1.GetMeRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	u := me.Msg.GetUser()
	if u.GetId() != jo.ID.String() || u.GetDisplayName() != "Jo" || u.GetOwner() || len(u.GetIdentities()) != 2 {
		t.Fatalf("me: %v", u)
	}
	if ids := u.GetIdentities(); ids[0].GetProvider() != "discord" || ids[1].GetProvider() != "steam" || ids[1].GetSubject() != "7656" || ids[1].GetVerificationMethod() != "openid" || ids[1].GetLastLoginAt() != nil || ids[0].GetLastLoginAt() == nil {
		t.Errorf("identities: %v", ids)
	}

	_, err = client.UnlinkIdentity(ctx, connect.NewRequest(&hubv1.UnlinkIdentityRequest{Provider: "steam"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("missing subject: %v", err)
	}
	_, err = client.UnlinkIdentity(ctx, connect.NewRequest(&hubv1.UnlinkIdentityRequest{Provider: "steam", Subject: "other"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("not mine: %v", err)
	}
	after, err := client.UnlinkIdentity(ctx, connect.NewRequest(&hubv1.UnlinkIdentityRequest{Provider: "steam", Subject: "7656"}))
	if err != nil || len(after.Msg.GetUser().GetIdentities()) != 1 {
		t.Fatalf("unlink: %v %v", err, after)
	}
	_, err = client.UnlinkIdentity(ctx, connect.NewRequest(&hubv1.UnlinkIdentityRequest{Provider: "discord", Subject: "1"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("last identity: %v", err)
	}
}

func TestRevokeSessions(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	jo := r.user(t, "Jo", "1")
	cookie := r.login(t, jo.ID)
	r.login(t, jo.ID)
	r.login(t, r.user(t, "Sam", "2").ID)
	client := hubv1connect.NewIdentityServiceClient(cookieClient(http.DefaultClient, cookie), r.srv.URL)
	resp, err := client.RevokeSessions(ctx, connect.NewRequest(&hubv1.RevokeSessionsRequest{}))
	if err != nil || resp.Msg.GetRevoked() != 2 {
		t.Fatalf("revoke: %v %v", err, resp)
	}
	if sc := resp.Header().Get("Set-Cookie"); !strings.Contains(sc, "gravel_session=;") || !strings.Contains(sc, "Max-Age=0") {
		t.Errorf("the response should clear the cookie: %q", sc)
	}
	if r.idSt.SessionCount() != 1 {
		t.Errorf("sam's session must survive: %d left", r.idSt.SessionCount())
	}
	if _, err := client.GetMe(ctx, connect.NewRequest(&hubv1.GetMeRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("the revoked cookie is dead: %v", err)
	}
}

func TestLogoutEndsOnlyThisSession(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	jo := r.user(t, "Jo", "1")
	cookie := r.login(t, jo.ID)
	r.login(t, jo.ID)
	anon := hubv1connect.NewIdentityServiceClient(http.DefaultClient, r.srv.URL)
	if _, err := anon.Logout(ctx, connect.NewRequest(&hubv1.LogoutRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("anonymous logout: %v", err)
	}
	client := hubv1connect.NewIdentityServiceClient(cookieClient(http.DefaultClient, cookie), r.srv.URL)
	resp, err := client.Logout(ctx, connect.NewRequest(&hubv1.LogoutRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if sc := resp.Header().Get("Set-Cookie"); !strings.Contains(sc, "gravel_session=;") || !strings.Contains(sc, "Max-Age=0") {
		t.Errorf("logout must clear the cookie: %q", sc)
	}
	if r.idSt.SessionCount() != 1 {
		t.Errorf("the other session must survive: %d left", r.idSt.SessionCount())
	}
	if _, err := client.GetMe(ctx, connect.NewRequest(&hubv1.GetMeRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("the cookie is dead after logout: %v", err)
	}
}

func TestLookupUserIsOwnerOnly(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	jo := r.user(t, "Jo", "1")
	sam := r.user(t, "Sam", "2")
	joClient := hubv1connect.NewIdentityServiceClient(cookieClient(http.DefaultClient, r.login(t, jo.ID)), r.srv.URL)
	samClient := hubv1connect.NewIdentityServiceClient(cookieClient(http.DefaultClient, r.login(t, sam.ID)), r.srv.URL)
	req := func() *connect.Request[hubv1.LookupUserRequest] {
		return connect.NewRequest(&hubv1.LookupUserRequest{Provider: "discord", Subject: "2"})
	}
	if _, err := hubv1connect.NewIdentityServiceClient(http.DefaultClient, r.srv.URL).LookupUser(ctx, req()); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("anonymous: %v", err)
	}
	if _, err := joClient.LookupUser(ctx, req()); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("before the claim nobody is owner: %v", err)
	}
	if _, err := r.org.Claim(ctx, r.token, store.Actor{UserID: &jo.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := samClient.LookupUser(ctx, req()); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a member is refused: %v", err)
	}
	got, err := joClient.LookupUser(ctx, req())
	if err != nil || got.Msg.GetUser().GetId() != sam.ID.String() || got.Msg.GetUser().GetOwner() || len(got.Msg.GetUser().GetIdentities()) != 1 {
		t.Fatalf("owner lookup: %v %v", err, got)
	}
	if _, err := joClient.LookupUser(ctx, connect.NewRequest(&hubv1.LookupUserRequest{Provider: "steam", Subject: "nope"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("unknown pair: %v", err)
	}
	if _, err := joClient.LookupUser(ctx, connect.NewRequest(&hubv1.LookupUserRequest{Provider: "", Subject: "x"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("blank provider: %v", err)
	}
	me, _ := joClient.GetMe(ctx, connect.NewRequest(&hubv1.GetMeRequest{}))
	if !me.Msg.GetUser().GetOwner() {
		t.Error("the owner sees owner: true")
	}
}

// GetMe says what the caller may do (ADR-0027): the owner every capability a person can hold, a
// moderator what moderators hold, a member the base. ListUsers says the same of each member.
func TestGetMeCapabilities(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	owner := r.user(t, "Owner", "1")
	if _, err := r.org.Claim(ctx, r.token, store.Actor{UserID: &owner.ID}); err != nil {
		t.Fatal(err)
	}
	mod := r.user(t, "Mod", "2")
	r.grant(t, owner.ID, mod.ID, store.RoleModerator)
	member := r.user(t, "Member", "3")
	caps := func(cookie string) []string {
		t.Helper()
		me, err := hubv1connect.NewIdentityServiceClient(cookieClient(http.DefaultClient, cookie), r.srv.URL).GetMe(ctx, connect.NewRequest(&hubv1.GetMeRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		return me.Msg.GetUser().GetCapabilities()
	}
	ownerCaps := caps(r.login(t, owner.ID))
	for _, c := range []string{"servers:moderate", "servers:provision", "settings:write", "roles:grant", "org:transfer"} {
		if !slices.Contains(ownerCaps, c) {
			t.Errorf("the owner lacks %q: %v", c, ownerCaps)
		}
	}
	if slices.Contains(ownerCaps, "participation:write") {
		t.Errorf("the owner holds an app's own capability: %v", ownerCaps)
	}
	if got := caps(r.login(t, mod.ID)); !slices.Equal(got, []string{"identity:read", "servers:read", "servers:moderate", "servers:lease", "stats:read", "events:read"}) {
		t.Errorf("moderator = %v", got)
	}
	if got := caps(r.login(t, member.ID)); !slices.Equal(got, []string{"servers:read"}) {
		t.Errorf("member = %v", got)
	}

	// A member's token reads the member as they are in person; the token's own scopes narrow what
	// it may call, not what the member holds.
	me, err := hubv1connect.NewIdentityServiceClient(memberClient(r.memberToken(t, mod.ID, nil)), r.srv.URL).GetMe(ctx, connect.NewRequest(&hubv1.GetMeRequest{}))
	if err != nil || !slices.Equal(me.Msg.GetUser().GetCapabilities(), []string{"identity:read", "servers:read", "servers:moderate", "servers:lease", "stats:read", "events:read"}) {
		t.Errorf("moderator through a token: %v %v", me, err)
	}

	users, err := hubv1connect.NewIdentityServiceClient(cookieClient(http.DefaultClient, r.login(t, owner.ID)), r.srv.URL).ListUsers(ctx, connect.NewRequest(&hubv1.ListUsersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range users.Msg.GetUsers() {
		if u.GetId() == member.ID.String() && !slices.Equal(u.GetCapabilities(), []string{"servers:read"}) {
			t.Errorf("ListUsers member = %v", u.GetCapabilities())
		}
	}
}
