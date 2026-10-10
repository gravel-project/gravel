package api_test

import (
	"context"
	"net/http"
	"testing"

	"connectrpc.com/connect"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
)

// gravel admits nothing by a host's own scope: an app holding only host:… scopes is an app with
// no gravel scope at all, refused every scoped procedure and served only what is public.
func TestHostScopesAdmitNothingInGravel(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	hc := bearerClient(http.DefaultClient, r.bearer(t, "htg-bot", "host:muster:write", "host:muster:read", "host:servers:moderate"))

	mod := hubv1connect.NewModerationServiceClient(hc, r.srv.URL)
	if _, err := mod.ListAuditLog(ctx, connect.NewRequest(&hubv1.ListAuditLogRequest{ServerId: "htg-wardogs-1"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("moderation with only host scopes: %v, want permission_denied", err)
	}
	ids := hubv1connect.NewIdentityServiceClient(hc, r.srv.URL)
	if _, err := ids.ListUsers(ctx, connect.NewRequest(&hubv1.ListUsersRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("identity reads with only host scopes: %v, want permission_denied", err)
	}
	stats := hubv1connect.NewStatsServiceClient(hc, r.srv.URL)
	if _, err := stats.RecordParticipation(ctx, connect.NewRequest(&hubv1.RecordParticipationRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("participation with only host scopes: %v, want permission_denied", err)
	}
	// What is public stays public.
	orgs := hubv1connect.NewOrganizationServiceClient(hc, r.srv.URL)
	if _, err := orgs.GetOrganizationSettings(ctx, connect.NewRequest(&hubv1.GetOrganizationSettingsRequest{})); err != nil {
		t.Errorf("public settings: %v", err)
	}
}
