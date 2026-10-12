package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/known/timestamppb"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/api"
	"github.com/gravel-project/gravel/internal/apps"
	"github.com/gravel-project/gravel/internal/store"
)

const testVerifier = "a-pkce-verifier-of-at-least-forty-three-characters-0123456789"

// memberToken signs a member in to a new app holding appScopes, asking for scopes, through the
// code flow, and returns the Authorization header value of the member's token.
func (r *rig) memberToken(t *testing.T, userID uuid.UUID, appScopes []string, scopes ...string) string {
	t.Helper()
	ctx := context.Background()
	c, err := r.apps.Create(ctx, "Muster", appScopes, "https://muster.example/cb")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(testVerifier))
	code, _, err := r.apps.Authorize(ctx, apps.AuthorizationRequest{ResponseType: "code", ClientID: c.App.ClientID, RedirectURI: "https://muster.example/cb",
		Scope: strings.Join(scopes, " "), CodeChallenge: base64.RawURLEncoding.EncodeToString(sum[:]), CodeChallengeMethod: "S256"}, userID)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := r.apps.Exchange(ctx, c.App.ClientID, c.Secret, code, "https://muster.example/cb", testVerifier)
	if err != nil {
		t.Fatal(err)
	}
	return "Bearer " + tok.Value
}

func memberClient(auth string) *http.Client { return bearerClient(http.DefaultClient, auth) }

// Signing in needs no scope: a member's token always reads its own member.
func TestMemberTokenGetMeNeedsNoScope(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	jo := r.user(t, "Jo", "1")
	c := hubv1connect.NewIdentityServiceClient(memberClient(r.memberToken(t, jo.ID, nil)), r.srv.URL)
	me, err := c.GetMe(ctx, connect.NewRequest(&hubv1.GetMeRequest{}))
	if err != nil || me.Msg.GetUser().GetId() != jo.ID.String() || me.Msg.GetUser().GetDisplayName() != "Jo" {
		t.Fatalf("GetMe: %v %v", me, err)
	}
	// The token wins over a session cookie sent with it: the call is the token's member's.
	sam := r.user(t, "Sam", "2")
	both := bearerClient(cookieClient(http.DefaultClient, r.login(t, sam.ID)), r.memberToken(t, jo.ID, nil))
	if me, err := hubv1connect.NewIdentityServiceClient(both, r.srv.URL).GetMe(ctx, connect.NewRequest(&hubv1.GetMeRequest{})); err != nil || me.Msg.GetUser().GetId() != jo.ID.String() {
		t.Errorf("token and cookie: %v %v", me, err)
	}
}

// A member's token acts as the member, inside its scopes: an owner's token without the scope for
// an owner action is refused, with it the owner's right applies, and a member's right is never
// widened by a scope.
func TestMemberTokenScopesAndTheMembersOwnRights(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	owner := r.user(t, "Owner", "1")
	if _, err := r.org.Claim(ctx, r.token, owner.ID); err != nil {
		t.Fatal(err)
	}
	member := r.user(t, "Member", "2")
	all := []string{apps.ScopeIdentityRead, apps.ScopeStatsRead, apps.ScopeServersRead, apps.ScopeServersConfigure, apps.ScopeServersModerate}
	lookup := &hubv1.LookupUserRequest{Provider: "discord", Subject: "2"}

	ownerNoScope := hubv1connect.NewIdentityServiceClient(memberClient(r.memberToken(t, owner.ID, all)), r.srv.URL)
	if _, err := ownerNoScope.LookupUser(ctx, connect.NewRequest(lookup)); connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "identity:read") {
		t.Errorf("the owner's token without identity:read: %v", err)
	}
	ownerScoped := hubv1connect.NewIdentityServiceClient(memberClient(r.memberToken(t, owner.ID, all, apps.ScopeIdentityRead)), r.srv.URL)
	if u, err := ownerScoped.LookupUser(ctx, connect.NewRequest(lookup)); err != nil || u.Msg.GetUser().GetId() != member.ID.String() {
		t.Errorf("the owner's token with identity:read: %v %v", u, err)
	}
	memberScoped := hubv1connect.NewIdentityServiceClient(memberClient(r.memberToken(t, member.ID, all, apps.ScopeIdentityRead)), r.srv.URL)
	if _, err := memberScoped.LookupUser(ctx, connect.NewRequest(lookup)); connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "owner") {
		t.Errorf("a member's token with identity:read is still a member: %v", err)
	}

	r.withServer(t)
	cfgOwnerNoScope := hubv1connect.NewServerConfigServiceClient(memberClient(r.memberToken(t, owner.ID, all, apps.ScopeStatsRead)), r.srv.URL)
	if _, err := cfgOwnerNoScope.GetServerConfig(ctx, connect.NewRequest(&hubv1.GetServerConfigRequest{ServerId: "wd-1"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("an owner action without servers:configure: %v", err)
	}
	cfgOwner := hubv1connect.NewServerConfigServiceClient(memberClient(r.memberToken(t, owner.ID, all, apps.ScopeServersConfigure)), r.srv.URL)
	if _, err := cfgOwner.GetServerConfig(ctx, connect.NewRequest(&hubv1.GetServerConfigRequest{ServerId: "wd-1"})); connect.CodeOf(err) == connect.CodePermissionDenied || connect.CodeOf(err) == connect.CodeUnauthenticated {
		t.Errorf("the owner's token with servers:configure is the owner: %v", err)
	}
	cfgMember := hubv1connect.NewServerConfigServiceClient(memberClient(r.memberToken(t, member.ID, all, apps.ScopeServersConfigure)), r.srv.URL)
	if _, err := cfgMember.GetServerConfig(ctx, connect.NewRequest(&hubv1.GetServerConfigRequest{ServerId: "wd-1"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a member's token with servers:configure: %v", err)
	}
	players := hubv1connect.NewServerServiceClient(memberClient(r.memberToken(t, member.ID, all)), r.srv.URL)
	if _, err := players.ListServerPlayers(ctx, connect.NewRequest(&hubv1.ListServerPlayersRequest{ServerId: "wd-1"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("who's on without servers:read: %v", err)
	}
	if _, err := players.ListServers(ctx, connect.NewRequest(&hubv1.ListServersRequest{})); err != nil {
		t.Errorf("a public procedure needs no scope: %v", err)
	}
	players = hubv1connect.NewServerServiceClient(memberClient(r.memberToken(t, member.ID, all, apps.ScopeServersRead)), r.srv.URL)
	if _, err := players.ListServerPlayers(ctx, connect.NewRequest(&hubv1.ListServerPlayersRequest{ServerId: "wd-1"})); err != nil {
		t.Errorf("who's on with servers:read, as a member: %v", err)
	}
}

// What a member does in person stays in person: a member's token cannot unlink, log out, change
// roles or settings, claim the hub, set the leaderboard name, or reach the apps the member allowed.
func TestMemberTokenRefusedTheAccountsOwnControls(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	owner := r.user(t, "Owner", "1")
	if _, err := r.org.Claim(ctx, r.token, owner.ID); err != nil {
		t.Fatal(err)
	}
	all := []string{apps.ScopeIdentityRead, apps.ScopeStatsRead, apps.ScopeServersRead, apps.ScopeServersConfigure, apps.ScopeServersModerate}
	auth := r.memberToken(t, owner.ID, all, all...)
	ids := hubv1connect.NewIdentityServiceClient(memberClient(auth), r.srv.URL)
	orgs := hubv1connect.NewOrganizationServiceClient(memberClient(auth), r.srv.URL)
	stats := hubv1connect.NewStatsServiceClient(memberClient(auth), r.srv.URL)
	appSvc := hubv1connect.NewAppServiceClient(memberClient(auth), r.srv.URL)
	for name, call := range map[string]func() error{
		"UnlinkIdentity": func() error {
			_, err := ids.UnlinkIdentity(ctx, connect.NewRequest(&hubv1.UnlinkIdentityRequest{Provider: "discord", Subject: "1"}))
			return err
		},
		"Logout": func() error { _, err := ids.Logout(ctx, connect.NewRequest(&hubv1.LogoutRequest{})); return err },
		"RevokeSessions": func() error {
			_, err := ids.RevokeSessions(ctx, connect.NewRequest(&hubv1.RevokeSessionsRequest{}))
			return err
		},
		"SetUserRole": func() error {
			_, err := ids.SetUserRole(ctx, connect.NewRequest(&hubv1.SetUserRoleRequest{UserId: owner.ID.String(), Role: "moderator", Granted: true}))
			return err
		},
		"UpdateOrganizationSettings": func() error {
			_, err := orgs.UpdateOrganizationSettings(ctx, connect.NewRequest(&hubv1.UpdateOrganizationSettingsRequest{Settings: &hubv1.OrganizationSettings{}}))
			return err
		},
		"ClaimOwnership": func() error {
			_, err := orgs.ClaimOwnership(ctx, connect.NewRequest(&hubv1.ClaimOwnershipRequest{Token: "x"}))
			return err
		},
		"SetBoardName": func() error {
			_, err := stats.SetBoardName(ctx, connect.NewRequest(&hubv1.SetBoardNameRequest{ShowName: true}))
			return err
		},
		"ListAppGrants": func() error {
			_, err := appSvc.ListAppGrants(ctx, connect.NewRequest(&hubv1.ListAppGrantsRequest{}))
			return err
		},
		"Authorize": func() error {
			_, err := appSvc.Authorize(ctx, connect.NewRequest(&hubv1.AuthorizeRequest{Params: &hubv1.AuthorizationParams{}}))
			return err
		},
	} {
		if err := call(); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("%s with the owner's token holding every scope: %v", name, err)
		}
	}
	// An app's own token is refused the member's procedures too.
	bot := hubv1connect.NewAppServiceClient(bearerClient(http.DefaultClient, r.bearer(t, "bot", all...)), r.srv.URL)
	if _, err := bot.ListAppGrants(ctx, connect.NewRequest(&hubv1.ListAppGrantsRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("an app's token: %v", err)
	}
}

// A member's token never satisfies an app's check: it acts on behalf of nobody, and moderation
// through it is the member's own, recorded with the member and the app.
func TestMemberTokenIsNeverTheApp(t *testing.T) {
	r := newRig(t)
	r.withServer(t)
	r.drv.caps = allCaps
	ctx := context.Background()
	owner := r.user(t, "Owner", "1")
	if _, err := r.org.Claim(ctx, r.token, owner.ID); err != nil {
		t.Fatal(err)
	}
	mod := r.user(t, "Mod", "42")
	if _, err := r.ids.SetRole(ctx, mod.ID, store.RoleModerator, true, owner.ID); err != nil {
		t.Fatal(err)
	}
	member := r.user(t, "Member", "7")
	scopes := []string{apps.ScopeServersModerate, apps.ScopeServersConfigure}
	kick := &hubv1.KickPlayerRequest{ServerId: "wd-1", Subject: steamA, Reason: "afk"}

	// A plain member's token with servers:moderate is not a bot with servers:moderate.
	asMember := hubv1connect.NewModerationServiceClient(memberClient(r.memberToken(t, member.ID, scopes, scopes...)), r.srv.URL)
	if _, err := asMember.KickPlayer(ctx, connect.NewRequest(kick)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a member's token with servers:moderate: %v", err)
	}
	// Nor may it name someone to act for, even a moderator's token naming a moderator.
	asMod := hubv1connect.NewModerationServiceClient(memberClient(r.memberToken(t, mod.ID, scopes, scopes...)), r.srv.URL)
	k := &hubv1.KickPlayerRequest{ServerId: "wd-1", Subject: steamA, Reason: "afk", OnBehalfOf: &hubv1.Actor{Provider: "discord", Subject: "42"}}
	if _, err := asMod.KickPlayer(ctx, connect.NewRequest(k)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("on_behalf_of through a member's token: %v", err)
	}
	cfg := hubv1connect.NewServerConfigServiceClient(memberClient(r.memberToken(t, owner.ID, scopes, scopes...)), r.srv.URL)
	if _, err := cfg.ApplyServerConfig(ctx, connect.NewRequest(&hubv1.ApplyServerConfigRequest{ServerId: "wd-1", Text: "x", Revision: "r1", Reason: "x",
		OnBehalfOf: &hubv1.Actor{Provider: "discord", Subject: "42"}})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("configuration on_behalf_of through a member's token: %v", err)
	}
	if len(r.drv.calls) != 0 || len(r.audit.Entries()) != 0 {
		t.Fatalf("refused calls reached the driver: %v", r.drv.calls)
	}
	// The moderator's own action through Muster is recorded with both.
	if _, err := asMod.KickPlayer(ctx, connect.NewRequest(kick)); err != nil {
		t.Fatalf("the moderator through the app: %v", err)
	}
	log, err := asMod.ListAuditLog(ctx, connect.NewRequest(&hubv1.ListAuditLogRequest{ServerId: "wd-1"}))
	if err != nil || len(log.Msg.GetEntries()) != 1 {
		t.Fatalf("log: %v %v", log, err)
	}
	e := log.Msg.GetEntries()[0]
	// (The in-memory audit store does not join the app's name; the store's own test does.)
	if e.GetUserId() != mod.ID.String() || e.GetUserName() != "Mod" || e.GetAppId() == "" || e.GetOnBehalfOf() != nil {
		t.Errorf("entry names the member and the app: %v", e)
	}
}

// App-only procedures refuse a member's token, even one that carries the app's own scope: the code
// flow never grants participation:write, and a token row holding it anyway (forged into the store
// here) is still the member's, never the app's.
func TestMemberTokenCannotRecordParticipation(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	jo := r.user(t, "Jo", "1")
	c, err := r.apps.Create(ctx, "Seeder", []string{apps.ScopeParticipationWrite}, "https://seeder.example/cb")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(testVerifier))
	_, err = r.apps.CheckAuthorization(ctx, apps.AuthorizationRequest{ResponseType: "code", ClientID: c.App.ClientID, RedirectURI: "https://seeder.example/cb",
		Scope: apps.ScopeParticipationWrite, CodeChallenge: base64.RawURLEncoding.EncodeToString(sum[:]), CodeChallengeMethod: "S256"}, jo.ID)
	var refusal *apps.AuthorizationError
	if !errors.As(err, &refusal) || refusal.Code != "invalid_scope" {
		t.Errorf("participation:write through the code flow: %v", err)
	}
	// Forge a member's token with the app's scope.
	value := "forged-member-token-with-participation-write"
	hash := sha256.Sum256([]byte(value))
	uid := jo.ID
	if err := r.appSt.CreateAppToken(ctx, store.AppToken{ID: uuid.New(), AppID: c.App.ID, TokenHash: hash[:], Scopes: []string{apps.ScopeParticipationWrite},
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), UserID: &uid}); err != nil {
		t.Fatal(err)
	}
	st := hubv1connect.NewStatsServiceClient(memberClient("Bearer "+value), r.srv.URL)
	req := &hubv1.RecordParticipationRequest{Events: []*hubv1.ParticipationEvent{{Key: "k1", Kind: "seeding",
		Identity: &hubv1.PlayerIdentity{Provider: "steam", Subject: "76561190000000001"}, OccurredAt: timestamppb.Now(), SchemaVersion: 1}}}
	if _, err := st.RecordParticipation(ctx, connect.NewRequest(req)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("RecordParticipation with a member's token: %v", err)
	}
	// The same app's own token is admitted (the control).
	tok, err := r.apps.Issue(ctx, c.App.ClientID, c.Secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	own := hubv1connect.NewStatsServiceClient(bearerClient(http.DefaultClient, "Bearer "+tok.Value), r.srv.URL)
	if _, err := own.RecordParticipation(ctx, connect.NewRequest(req)); connect.CodeOf(err) == connect.CodePermissionDenied || connect.CodeOf(err) == connect.CodeUnauthenticated {
		t.Errorf("the app's own token: %v", err)
	}
}

// A service mounted without the MemberTokens interceptor fails closed: a member's token is not
// taken as the member.
func TestMemberTokenWithoutTheInterceptorFailsClosed(t *testing.T) {
	r := newRig(t)
	jo := r.user(t, "Jo", "1")
	auth := r.memberToken(t, jo.ID, nil)
	mux := http.NewServeMux()
	mux.Handle(hubv1connect.NewIdentityServiceHandler(api.NewIdentityServer(r.ids, r.sess, r.org, slog.New(slog.NewTextHandler(io.Discard, nil)))))
	srv := newH2CServer(r.sess.Middleware(r.apps.Middleware(mux)))
	defer srv.Close()
	c := hubv1connect.NewIdentityServiceClient(memberClient(auth), srv.URL)
	if _, err := c.GetMe(context.Background(), connect.NewRequest(&hubv1.GetMeRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("GetMe without the interceptor: %v", err)
	}
}

// Every procedure of the API is classified for members' tokens: a new one is refused until it is
// added to the table on purpose, and the refused list here says what is refused on purpose.
func TestEveryProcedureIsClassifiedForMemberTokens(t *testing.T) {
	refused := []string{
		hubv1connect.IdentityServiceUnlinkIdentityProcedure, hubv1connect.IdentityServiceLogoutProcedure, hubv1connect.IdentityServiceRevokeSessionsProcedure,
		hubv1connect.IdentityServiceSetUserRoleProcedure, hubv1connect.OrganizationServiceClaimOwnershipProcedure,
		hubv1connect.OrganizationServiceUpdateOrganizationSettingsProcedure, hubv1connect.StatsServiceGetBoardNameProcedure, hubv1connect.StatsServiceSetBoardNameProcedure,
		hubv1connect.AppServicePrepareAuthorizationProcedure, hubv1connect.AppServiceAuthorizeProcedure, hubv1connect.AppServiceListAppGrantsProcedure,
		hubv1connect.AppServiceRevokeAppGrantProcedure, hubv1connect.StatsServiceRecordParticipationProcedure,
		hubv1connect.ServerHostServiceResizeServerHostProcedure, hubv1connect.ServerHostServiceUnpinServerHostSizeProcedure, hubv1connect.ServerHostServiceSnapshotServerHostVolumeProcedure, hubv1connect.ServerHostServiceDestroyServerHostProcedure,
	}
	seen := 0
	protoregistry.GlobalFiles.RangeFilesByPackage("gravel.hub.v1", func(fd protoreflect.FileDescriptor) bool {
		for i := range fd.Services().Len() {
			sd := fd.Services().Get(i)
			for j := range sd.Methods().Len() {
				proc := "/" + string(sd.FullName()) + "/" + string(sd.Methods().Get(j).Name())
				seen++
				scope, allowed := api.MemberTokenScope(proc)
				switch {
				case allowed && slices.Contains(refused, proc):
					t.Errorf("%s is both admitted (%q) and listed as refused", proc, scope)
				case !allowed && !slices.Contains(refused, proc):
					t.Errorf("%s is not classified for members' tokens: add it to memberProcedures with its scope, or to the refused list here", proc)
				case allowed && scope != "" && apps.Scopes[scope] == "":
					t.Errorf("%s needs %q, which is not a scope", proc, scope)
				}
			}
		}
		return true
	})
	if seen < 30 {
		t.Errorf("only %d procedures found: is the generated code linked?", seen)
	}
	_ = hubv1.File_gravel_hub_v1_apps_proto
}
