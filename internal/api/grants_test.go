package api_test

import (
	"context"
	"net/http"
	"testing"

	"connectrpc.com/connect"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/store"
)

// A game-scoped moderator moderates that game's servers, those added later included, and no
// other; the instance-wide audit log shows them their games' entries only (ADR-0027 §4).
func TestGameScopedGrants(t *testing.T) {
	r := newRig(t)
	r.withServer(t)
	r.drv.caps = allCaps
	ctx := context.Background()
	owner := r.user(t, "Owner", "1")
	if _, err := r.org.Claim(ctx, r.token, store.Actor{UserID: &owner.ID}); err != nil {
		t.Fatal(err)
	}
	ownerC := cookieClient(http.DefaultClient, r.login(t, owner.ID))
	ids := hubv1connect.NewIdentityServiceClient(ownerC, r.srv.URL)
	grant := func(who, role, game string) {
		t.Helper()
		if _, err := ids.SetUserRole(ctx, connect.NewRequest(&hubv1.SetUserRoleRequest{UserId: who, Role: role, GameId: game, Granted: true})); err != nil {
			t.Fatalf("grant %s %s %s: %v", who, role, game, err)
		}
	}
	wd := r.user(t, "WarDogsMod", "2")
	cs := r.user(t, "CSMod", "3")
	grant(wd.ID.String(), store.RoleModerator, "wardogs")
	grant(cs.ID.String(), store.RoleModerator, "cs2")

	me, err := hubv1connect.NewIdentityServiceClient(cookieClient(http.DefaultClient, r.login(t, wd.ID)), r.srv.URL).GetMe(ctx, connect.NewRequest(&hubv1.GetMeRequest{}))
	if err != nil || len(me.Msg.GetUser().GetRoles()) != 0 || len(me.Msg.GetUser().GetGrants()) != 1 || me.Msg.GetUser().GetGrants()[0].GetGameId() != "wardogs" {
		t.Fatalf("a scoped grant: roles are instance-wide only, grants carry the game: %v %v", me, err)
	}

	mods := func(id string) hubv1connect.ModerationServiceClient {
		u := wd
		if id == "cs" {
			u = cs
		}
		return hubv1connect.NewModerationServiceClient(cookieClient(http.DefaultClient, r.login(t, u.ID)), r.srv.URL)
	}
	kick := func(server string) *hubv1.KickPlayerRequest {
		return &hubv1.KickPlayerRequest{ServerId: server, Subject: steamA, Reason: "afk"}
	}
	if _, err := mods("wd").KickPlayer(ctx, connect.NewRequest(kick("wd-1"))); err != nil {
		t.Errorf("the War Dogs moderator on a War Dogs server: %v", err)
	}
	if _, err := mods("cs").KickPlayer(ctx, connect.NewRequest(kick("wd-1"))); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("the CS2 moderator on a War Dogs server: %v", err)
	}

	// A server of the game added after the grant is covered.
	m := servers.Manifest{Games: []servers.GameRef{{ID: "wardogs"}}, Servers: []servers.Server{
		{ID: "wd-1", Name: "War Dogs #1", Game: "wardogs", Driver: "wardogs", Location: "slc", Endpoint: endpoint, CredentialFile: credFile, Trust: servers.TrustOfficial},
		{ID: "wd-2", Name: "War Dogs #2", Game: "wardogs", Driver: "wardogs", Location: "slc", Endpoint: endpoint, CredentialFile: credFile, Trust: servers.TrustCommunity}}}
	if _, err := r.srvs.Apply(ctx, m, servers.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	// Admitted: the rig hasn't polled it, so it is unavailable, not refused.
	if _, err := mods("wd").ListServerBans(ctx, connect.NewRequest(&hubv1.ListServerBansRequest{ServerId: "wd-2"})); connect.CodeOf(err) == connect.CodePermissionDenied {
		t.Errorf("a server added later: %v", err)
	}

	// The instance-wide log: the War Dogs moderator sees War Dogs entries, the CS2 one none, a
	// member is refused.
	if log, err := mods("wd").ListAuditLog(ctx, connect.NewRequest(&hubv1.ListAuditLogRequest{})); err != nil || len(log.Msg.GetEntries()) != 1 {
		t.Errorf("the War Dogs moderator's log: %v %v", log, err)
	}
	if log, err := mods("cs").ListAuditLog(ctx, connect.NewRequest(&hubv1.ListAuditLogRequest{})); err != nil || len(log.Msg.GetEntries()) != 0 {
		t.Errorf("the CS2 moderator's log: %v %v", log, err)
	}
	if _, err := mods("cs").ListAuditLog(ctx, connect.NewRequest(&hubv1.ListAuditLogRequest{ServerId: "wd-1"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("the CS2 moderator asking for a War Dogs server's log: %v", err)
	}
	member := r.user(t, "Member", "4")
	if _, err := hubv1connect.NewModerationServiceClient(cookieClient(http.DefaultClient, r.login(t, member.ID)), r.srv.URL).ListAuditLog(ctx, connect.NewRequest(&hubv1.ListAuditLogRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a member's log: %v", err)
	}

	// ListRoleGrants says who holds what where.
	gs, err := ids.ListRoleGrants(ctx, connect.NewRequest(&hubv1.ListRoleGrantsRequest{}))
	if err != nil || len(gs.Msg.GetGrants()) != 2 || gs.Msg.GetGrants()[0].GetGrantedByUserId() != owner.ID.String() {
		t.Errorf("grants: %v %v", gs, err)
	}
	r.idSt.Games = []string{"wardogs", "cs2"}
	if _, err := ids.SetUserRole(ctx, connect.NewRequest(&hubv1.SetUserRoleRequest{UserId: wd.ID.String(), Role: "operator", GameId: "halo", Granted: true})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a game the hub doesn't have: %v", err)
	}
}

// An app holding roles:grant grants moderator, and nothing higher (Discord role sync's ceiling).
func TestAppGrantsModeratorOnly(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	owner := r.user(t, "Owner", "1")
	if _, err := r.org.Claim(ctx, r.token, store.Actor{UserID: &owner.ID}); err != nil {
		t.Fatal(err)
	}
	sam := r.user(t, "Sam", "2")
	bot := hubv1connect.NewIdentityServiceClient(bearerClient(http.DefaultClient, r.bearer(t, "sync", "roles:grant")), r.srv.URL)
	if _, err := bot.SetUserRole(ctx, connect.NewRequest(&hubv1.SetUserRoleRequest{UserId: sam.ID.String(), Role: store.RoleOperator, Granted: true})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("app granting operator: %v", err)
	}
	if _, err := bot.SetUserRole(ctx, connect.NewRequest(&hubv1.SetUserRoleRequest{UserId: sam.ID.String(), Role: store.RoleAdmin, Granted: true})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("app granting admin: %v", err)
	}
	got, err := bot.SetUserRole(ctx, connect.NewRequest(&hubv1.SetUserRoleRequest{UserId: sam.ID.String(), Role: store.RoleModerator, Granted: true}))
	if err != nil || len(got.Msg.GetUser().GetGrants()) != 1 || got.Msg.GetUser().GetGrants()[0].GetGrantedByAppId() == "" {
		t.Errorf("app granting moderator: %v %v", got, err)
	}
}
