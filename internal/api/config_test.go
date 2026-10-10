package api_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/gravel-project/gravel/drivers"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/apps"
)

func TestServerConfigIsTheOwnersOrAnAppsWithTheScope(t *testing.T) {
	r := newRig(t)
	r.withServer(t)
	r.drv.caps = append(allCaps, drivers.CapConfigRead, drivers.CapConfigWrite)
	ctx := context.Background()
	get := &hubv1.GetServerConfigRequest{ServerId: "wd-1"}

	member := r.user(t, "Member", "2")
	asMember := hubv1connect.NewServerConfigServiceClient(cookieClient(http.DefaultClient, r.login(t, member.ID)), r.srv.URL)
	if _, err := asMember.GetServerConfig(ctx, connect.NewRequest(get)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a member: %v", err)
	}
	moderator := hubv1connect.NewServerConfigServiceClient(bearerClient(http.DefaultClient, r.bearer(t, "mod", apps.ScopeServersModerate)), r.srv.URL)
	if _, err := moderator.GetServerConfig(ctx, connect.NewRequest(get)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("an app with servers:moderate only: %v", err)
	}

	cfg := hubv1connect.NewServerConfigServiceClient(bearerClient(http.DefaultClient, r.bearer(t, "deploy", apps.ScopeServersConfigure)), r.srv.URL)
	doc, err := cfg.GetServerConfig(ctx, connect.NewRequest(get))
	if err != nil || doc.Msg.GetRevision() != "r1" || !strings.Contains(doc.Msg.GetText(), "<redacted>") || doc.Msg.GetSections()[0].GetLocked()[0] != "Port: RCONPort" {
		t.Fatalf("config = %v, %v", doc, err)
	}
	plan, err := cfg.PlanServerConfig(ctx, connect.NewRequest(&hubv1.PlanServerConfigRequest{ServerId: "wd-1", Text: "[K]\r\nScorePeriod=27\r\n"}))
	if err != nil || plan.Msg.GetRevision() != "r1" || !plan.Msg.GetResult().GetOk() || plan.Msg.GetChanges()[0].GetKey() != "ScorePeriod" {
		t.Fatalf("plan = %v, %v", plan, err)
	}
	// The hub hands the driver its (adopted, empty) ban list and the game's bands.
	if r.drv.draft.Bans == nil || len(r.drv.draft.Bands) != 5 || r.drv.draft.Text != "[K]\r\nScorePeriod=27\r\n" {
		t.Errorf("draft = %+v", r.drv.draft)
	}
	// Every kind of band reaches the driver whole.
	for _, b := range r.drv.draft.Bands {
		if (b.Key == "ServerImageURL" && len(b.Hosts) != 4) || (b.Key == "ServerName" && (b.MaxLength == nil || *b.MaxLength != 64)) ||
			(b.Key == "MaxReservedSlots" && b.UnlessSet != "DefaultReservedPlayerIds") {
			t.Errorf("band = %+v", b)
		}
	}
	if len(r.audit.Entries()) != 0 {
		t.Error("a plan was audited")
	}
	applied, err := cfg.ApplyServerConfig(ctx, connect.NewRequest(&hubv1.ApplyServerConfigRequest{ServerId: "wd-1", Text: "[K]\r\nScorePeriod=27\r\n",
		Revision: "r1", Reason: "wardogs-server@abc123", OnBehalfOf: &hubv1.Actor{Provider: "github", Subject: "jomkz"}}))
	if err != nil || applied.Msg.GetResult().GetRevision() != "r1+1" || applied.Msg.GetAuditId() != 1 {
		t.Fatalf("apply = %v, %v", applied, err)
	}
	e := r.audit.Entries()[0]
	if e.Action != "apply_config" || e.Outcome != "ok" || e.Reason != "wardogs-server@abc123" || e.OnBehalfSubject != "jomkz" || string(e.Detail) != `{"revision":"r1"}` {
		t.Errorf("entry = %+v", e)
	}

	owner := r.user(t, "Owner", "1")
	if _, err := r.org.Claim(ctx, r.token, owner.ID); err != nil {
		t.Fatal(err)
	}
	asOwner := hubv1connect.NewServerConfigServiceClient(cookieClient(http.DefaultClient, r.login(t, owner.ID)), r.srv.URL)
	if _, err := asOwner.GetServerConfig(ctx, connect.NewRequest(get)); err != nil {
		t.Errorf("the owner: %v", err)
	}
}

func TestServerConfigErrors(t *testing.T) {
	r := newRig(t)
	r.withServer(t)
	ctx := context.Background()
	cfg := hubv1connect.NewServerConfigServiceClient(bearerClient(http.DefaultClient, r.bearer(t, "deploy", apps.ScopeServersConfigure)), r.srv.URL)
	plan := func() error {
		_, err := cfg.PlanServerConfig(ctx, connect.NewRequest(&hubv1.PlanServerConfigRequest{ServerId: "wd-1", Text: "[K]\r\nX=1\r\n"}))
		return err
	}
	apply := func() error {
		_, err := cfg.ApplyServerConfig(ctx, connect.NewRequest(&hubv1.ApplyServerConfigRequest{ServerId: "wd-1", Text: "[K]\r\nX=1\r\n", Revision: "r1"}))
		return err
	}

	r.drv.caps = allCaps // no configuration routes
	if err := plan(); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("not offered: %v", err)
	}
	r.drv.caps = append(allCaps, drivers.CapConfigRead, drivers.CapConfigWrite)
	if _, err := cfg.PlanServerConfig(ctx, connect.NewRequest(&hubv1.PlanServerConfigRequest{ServerId: "wd-1"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("an empty document: %v", err)
	}
	r.drv.errs["plan"] = &drivers.InvalidConfigError{Problems: []drivers.ConfigProblem{{Section: "K", Key: "ScorePeriod", Code: drivers.ProblemOutOfBand, Message: "60 is outside the band [18, 30]"}}}
	if err := plan(); connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "out_of_band") || !strings.Contains(err.Error(), "[18, 30]") {
		t.Errorf("out of band: %v", err)
	}
	for _, tc := range []struct {
		err  error
		code connect.Code
	}{
		{fmt.Errorf("%w: moved", drivers.ErrConfigConflict), connect.CodeAborted},
		{fmt.Errorf("%w: 422 from 203.0.113.10:7789", drivers.ErrConfigRejected), connect.CodeInvalidArgument},
		{fmt.Errorf("%w: 203.0.113.10:7789", drivers.ErrCredentialRefused), connect.CodeUnavailable},
	} {
		r.drv.errs["apply"] = tc.err
		if err := apply(); connect.CodeOf(err) != tc.code || strings.Contains(err.Error(), "203.0.113.10") {
			t.Errorf("%v: answered %v", tc.err, err)
		}
	}
}
