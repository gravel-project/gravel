package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/store"
)

// The operator's admin procedures (#195): what gravel-hub did on the host, over the API, for staff
// in person.
func TestAdminApps(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	jo := r.user(t, "Jo", "1")
	sam := r.user(t, "Sam", "2")
	if _, err := r.org.Claim(ctx, r.token, store.Actor{UserID: &jo.ID}); err != nil {
		t.Fatal(err)
	}
	owner := hubv1connect.NewAppServiceClient(cookieClient(http.DefaultClient, r.login(t, jo.ID)), r.srv.URL)
	member := hubv1connect.NewAppServiceClient(cookieClient(http.DefaultClient, r.login(t, sam.ID)), r.srv.URL)

	if _, err := member.ListApps(ctx, connect.NewRequest(&hubv1.ListAppsRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a member lists apps: %v", err)
	}
	if _, err := owner.CreateApp(ctx, connect.NewRequest(&hubv1.CreateAppRequest{Name: "Muster", Scopes: []string{"servers:fly"}})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a bad scope: %v", err)
	}
	created, err := owner.CreateApp(ctx, connect.NewRequest(&hubv1.CreateAppRequest{Name: "Muster", Scopes: []string{"identity:read", "stats:read"},
		RedirectUris: []string{"https://muster.example/callback"}}))
	if err != nil {
		t.Fatal(err)
	}
	a := created.Msg.GetApp()
	if created.Msg.GetClientSecret() == "" || a.GetClientId() == "" || a.GetName() != "Muster" || len(a.GetScopes()) != 2 || a.GetRevokedAt() != nil {
		t.Fatalf("created: %v", created.Msg)
	}
	// Recorded as the owner's, through the API (app.created).
	if got := r.appSt.Actors["app.created"]; len(got) != 1 || *got[0].UserID != jo.ID || got[0].Via != store.ViaAPI {
		t.Errorf("recorded: %+v", got)
	}
	list, err := owner.ListApps(ctx, connect.NewRequest(&hubv1.ListAppsRequest{}))
	if err != nil || len(list.Msg.GetApps()) != 1 || list.Msg.GetApps()[0].GetClientId() != a.GetClientId() {
		t.Fatalf("list: %v %v", list, err)
	}
	if strings.Contains(list.Msg.String(), created.Msg.GetClientSecret()) {
		t.Error("ListApps answers the secret")
	}
	if _, err := owner.RevokeApp(ctx, connect.NewRequest(&hubv1.RevokeAppRequest{ClientId: a.GetClientId()})); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.RevokeApp(ctx, connect.NewRequest(&hubv1.RevokeAppRequest{ClientId: a.GetClientId()})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("revoked twice: %v", err)
	}
}

func TestAdminServersManifest(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	jo := r.user(t, "Jo", "1")
	if _, err := r.org.Claim(ctx, r.token, store.Actor{UserID: &jo.ID}); err != nil {
		t.Fatal(err)
	}
	// The host applied the manifest first.
	m := servers.Manifest{Games: []servers.GameRef{{ID: "wardogs"}}, Servers: []servers.Server{
		{ID: "wd-1", Name: "War Dogs #1", Game: "wardogs", Driver: "wardogs", Location: "slc", Endpoint: endpoint, CredentialFile: credFile, Trust: servers.TrustOfficial},
		{ID: "wd-2", Name: "War Dogs #2", Game: "wardogs", Driver: "wardogs", Location: "slc", Endpoint: endpoint, CredentialFile: credFile, Trust: servers.TrustCommunity}}}
	if _, err := r.srvs.Apply(ctx, m, servers.ApplyOptions{By: store.Actor{Via: store.ViaCLI}}); err != nil {
		t.Fatal(err)
	}
	c := hubv1connect.NewServerServiceClient(cookieClient(http.DefaultClient, r.login(t, jo.ID)), r.srv.URL)

	exp, err := c.ExportServers(ctx, connect.NewRequest(&hubv1.ExportServersRequest{}))
	if err != nil || !strings.Contains(exp.Msg.GetManifest(), "id: wd-1") {
		t.Fatalf("export: %v %v", exp, err)
	}
	text := exp.Msg.GetManifest()
	renamed := strings.Replace(text, "name: 'War Dogs #1'", "name: War Dogs One", 1)
	if chk, err := c.CheckServers(ctx, connect.NewRequest(&hubv1.CheckServersRequest{Manifest: renamed})); err != nil || chk.Msg.GetSummary() != "1 games [wardogs], 2 servers [wd-1 wd-2]" {
		t.Errorf("check: %v %v", chk, err)
	}
	if _, err := c.CheckServers(ctx, connect.NewRequest(&hubv1.CheckServersRequest{Manifest: "servers: [{id: X}]"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("an invalid manifest: %v", err)
	}

	// A dry run says what would change and changes nothing.
	before := len(r.srvSt.Recorded)
	dry, err := c.ApplyServers(ctx, connect.NewRequest(&hubv1.ApplyServersRequest{Manifest: renamed, DryRun: true}))
	if err != nil || dry.Msg.GetApplied() || len(dry.Msg.GetChanges()) != 1 || dry.Msg.GetChanges()[0] != "server wd-1 changed" || len(r.srvSt.Recorded) != before {
		t.Fatalf("dry run: %v %v", dry, err)
	}
	// What only the host may change is refused, naming each.
	stolen := strings.Replace(strings.Replace(text, "credential_file: "+credFile, "credential_file: /run/secrets/gravel-db-url", 1), endpoint, "https://attacker.example", 1)
	_, err = c.ApplyServers(ctx, connect.NewRequest(&hubv1.ApplyServersRequest{Manifest: stolen}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "server wd-1: its endpoint") || !strings.Contains(err.Error(), "server wd-1: its credential_file") {
		t.Errorf("a host-only change: %v", err)
	}
	// The rename applies, recorded as the owner's.
	got, err := c.ApplyServers(ctx, connect.NewRequest(&hubv1.ApplyServersRequest{Manifest: renamed}))
	if err != nil || !got.Msg.GetApplied() || len(got.Msg.GetChanges()) != 1 {
		t.Fatalf("apply: %v %v", got, err)
	}
	rec := r.srvSt.Recorded[len(r.srvSt.Recorded)-1]
	if rec.By.UserID == nil || *rec.By.UserID != jo.ID || rec.By.Via != store.ViaAPI || len(rec.Servers) != 1 || rec.Servers[0] != "wd-1" {
		t.Errorf("recorded: %+v", rec)
	}
	// Removing a server is the API's to do.
	two := strings.Index(renamed, "    - id: wd-2")
	if two < 0 {
		t.Fatalf("export layout:\n%s", renamed)
	}
	if got, err := c.ApplyServers(ctx, connect.NewRequest(&hubv1.ApplyServersRequest{Manifest: renamed[:two]})); err != nil || len(got.Msg.GetChanges()) != 1 || got.Msg.GetChanges()[0] != "server wd-2 removed" {
		t.Errorf("remove: %v %v", got, err)
	}
}

func TestAdminEraseAndSessions(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	jo := r.user(t, "Jo", "1")
	sam := r.user(t, "Sam", "2")
	if _, err := r.org.Claim(ctx, r.token, store.Actor{UserID: &jo.ID}); err != nil {
		t.Fatal(err)
	}
	stats := hubv1connect.NewStatsServiceClient(cookieClient(http.DefaultClient, r.login(t, jo.ID)), r.srv.URL)
	samStats := hubv1connect.NewStatsServiceClient(cookieClient(http.DefaultClient, r.login(t, sam.ID)), r.srv.URL)
	if _, err := samStats.EraseIdentity(ctx, connect.NewRequest(&hubv1.EraseIdentityRequest{Provider: "steam", Subject: "765611900000001"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a member erases: %v", err)
	}
	if _, err := stats.EraseIdentity(ctx, connect.NewRequest(&hubv1.EraseIdentityRequest{Provider: "steam"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("no subject: %v", err)
	}
	got, err := stats.EraseIdentity(ctx, connect.NewRequest(&hubv1.EraseIdentityRequest{Provider: " steam ", Subject: "765611900000001"}))
	if err != nil || got.Msg.GetMatchRows() != 3 || !got.Msg.GetPseudonymDeleted() {
		t.Fatalf("erase: %v %v", got, err)
	}
	if len(r.erased) != 1 || r.erased[0].key != (store.PlayerKey{Provider: "steam", Subject: "765611900000001"}) || *r.erased[0].by.UserID != jo.ID {
		t.Errorf("erased: %+v", r.erased)
	}

	// Sam is logged out everywhere by the owner; their session stops working.
	samCookie := r.login(t, sam.ID)
	ids := hubv1connect.NewIdentityServiceClient(cookieClient(http.DefaultClient, r.login(t, jo.ID)), r.srv.URL)
	samIDs := hubv1connect.NewIdentityServiceClient(cookieClient(http.DefaultClient, samCookie), r.srv.URL)
	if _, err := samIDs.RevokeUserSessions(ctx, connect.NewRequest(&hubv1.RevokeUserSessionsRequest{UserId: jo.ID.String()})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a member revokes the owner: %v", err)
	}
	n, err := ids.RevokeUserSessions(ctx, connect.NewRequest(&hubv1.RevokeUserSessionsRequest{UserId: sam.ID.String()}))
	if err != nil || n.Msg.GetRevoked() < 1 {
		t.Fatalf("revoke: %v %v", n, err)
	}
	if _, err := samIDs.GetMe(ctx, connect.NewRequest(&hubv1.GetMeRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("Sam's session after the revoke: %v", err)
	}
	if _, err := ids.RevokeUserSessions(ctx, connect.NewRequest(&hubv1.RevokeUserSessionsRequest{UserId: "6f1c2f8e-0000-4000-8000-000000000000"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("an unknown member: %v", err)
	}
}
