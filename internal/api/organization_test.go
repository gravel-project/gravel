package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/api"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/org/orgtest"
)

func newServer(t *testing.T) (*httptest.Server, *orgtest.FakeStore, string) {
	t.Helper()
	fake := &orgtest.FakeStore{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := org.New(fake, 15*time.Minute, logger)
	_, token, err := svc.EnsureBuiltin(context.Background(), "Test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle(hubv1connect.NewOrganizationServiceHandler(api.NewOrganizationServer(svc, logger)))
	srv := newH2CServer(mux)
	t.Cleanup(srv.Close)
	return srv, fake, token
}

func TestClaimFlowOverGRPC(t *testing.T) {
	srv, _, token := newServer(t)
	client := hubv1connect.NewOrganizationServiceClient(h2cClient(), srv.URL, connect.WithGRPC())
	ctx := context.Background()

	got, err := client.GetOrganization(ctx, connect.NewRequest(&hubv1.GetOrganizationRequest{}))
	if err != nil || got.Msg.GetOrganization().GetOwned() || got.Msg.GetOrganization().GetName() != "Test" {
		t.Fatalf("get: %v %v", err, got)
	}

	_, err = client.ClaimOwnership(ctx, connect.NewRequest(&hubv1.ClaimOwnershipRequest{Token: "wrong"}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("wrong token: %v", err)
	}
	_, err = client.ClaimOwnership(ctx, connect.NewRequest(&hubv1.ClaimOwnershipRequest{Token: "  "}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("blank token: %v", err)
	}

	claimed, err := client.ClaimOwnership(ctx, connect.NewRequest(&hubv1.ClaimOwnershipRequest{Token: token}))
	if err != nil || !claimed.Msg.GetOrganization().GetOwned() || claimed.Msg.GetOrganization().GetClaimedAt() == nil {
		t.Fatalf("claim: %v %v", err, claimed)
	}

	_, err = client.ClaimOwnership(ctx, connect.NewRequest(&hubv1.ClaimOwnershipRequest{Token: token}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("second claim: %v", err)
	}
}

func TestHTTPJSON(t *testing.T) {
	srv, _, token := newServer(t)
	post := func(proc, body string) (int, map[string]any) {
		t.Helper()
		resp, err := http.Post(srv.URL+"/gravel.hub.v1.OrganizationService/"+proc, "application/json", strings.NewReader(body)) //nolint:noctx // test
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		err = json.NewDecoder(resp.Body).Decode(&out)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, out
	}
	code, out := post("GetOrganization", `{}`)
	if code != http.StatusOK || out["organization"].(map[string]any)["owned"] != nil {
		t.Errorf("json get: %d %v", code, out)
	}
	code, out = post("ClaimOwnership", `{"token":"`+token+`"}`)
	if code != http.StatusOK || out["organization"].(map[string]any)["owned"] != true {
		t.Errorf("json claim: %d %v", code, out)
	}
	code, out = post("ClaimOwnership", `{"token":"`+token+`"}`)
	if code != http.StatusBadRequest || out["code"] != "failed_precondition" {
		t.Errorf("json second claim: %d %v", code, out)
	}
}

func TestInternalErrorsAreOpaque(t *testing.T) {
	srv, fake, _ := newServer(t)
	fake.ErrOn = "get"
	client := hubv1connect.NewOrganizationServiceClient(http.DefaultClient, srv.URL)
	_, err := client.GetOrganization(context.Background(), connect.NewRequest(&hubv1.GetOrganizationRequest{}))
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeInternal || strings.Contains(ce.Message(), "db down") {
		t.Errorf("want opaque internal error, got %v", err)
	}
}
