package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
)

func TestClaimFlowOverGRPC(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	anon := hubv1connect.NewOrganizationServiceClient(h2cClient(), r.srv.URL, connect.WithGRPC())

	got, err := anon.GetOrganization(ctx, connect.NewRequest(&hubv1.GetOrganizationRequest{}))
	if err != nil || got.Msg.GetOrganization().GetOwned() || got.Msg.GetOrganization().GetName() != "Test" {
		t.Fatalf("get: %v %v", err, got)
	}
	_, err = anon.ClaimOwnership(ctx, connect.NewRequest(&hubv1.ClaimOwnershipRequest{Token: r.token}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("a claim needs a login: %v", err)
	}

	u := r.user(t, "Jo", "1")
	client := hubv1connect.NewOrganizationServiceClient(cookieClient(h2cClient(), r.login(t, u.ID)), r.srv.URL, connect.WithGRPC())
	_, err = client.ClaimOwnership(ctx, connect.NewRequest(&hubv1.ClaimOwnershipRequest{Token: "wrong"}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("wrong token: %v", err)
	}
	_, err = client.ClaimOwnership(ctx, connect.NewRequest(&hubv1.ClaimOwnershipRequest{Token: "  "}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("blank token: %v", err)
	}
	claimed, err := client.ClaimOwnership(ctx, connect.NewRequest(&hubv1.ClaimOwnershipRequest{Token: r.token}))
	if err != nil || !claimed.Msg.GetOrganization().GetOwned() || claimed.Msg.GetOrganization().GetClaimedAt() == nil || claimed.Msg.GetOrganization().GetOwnerUserId() != u.ID.String() {
		t.Fatalf("claim: %v %v", err, claimed)
	}
	_, err = client.ClaimOwnership(ctx, connect.NewRequest(&hubv1.ClaimOwnershipRequest{Token: r.token}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("second claim: %v", err)
	}
	got, _ = anon.GetOrganization(ctx, connect.NewRequest(&hubv1.GetOrganizationRequest{}))
	if got.Msg.GetOrganization().GetOwnerUserId() != u.ID.String() {
		t.Errorf("owner visible to anyone: %v", got.Msg)
	}
}

func TestHTTPJSON(t *testing.T) {
	r := newRig(t)
	u := r.user(t, "Jo", "1")
	cookie := r.login(t, u.ID)
	post := func(proc, body, cookie string) (int, map[string]any) {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, r.srv.URL+"/gravel.hub.v1.OrganizationService/"+proc, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if cookie != "" {
			req.Header.Set("Cookie", cookie)
		}
		resp, err := http.DefaultClient.Do(req)
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
	code, out := post("GetOrganization", `{}`, "")
	if code != http.StatusOK || out["organization"].(map[string]any)["owned"] != nil {
		t.Errorf("json get: %d %v", code, out)
	}
	code, out = post("ClaimOwnership", `{"token":"`+r.token+`"}`, "")
	if code != http.StatusUnauthorized || out["code"] != "unauthenticated" {
		t.Errorf("json claim without a cookie: %d %v", code, out)
	}
	code, out = post("ClaimOwnership", `{"token":"`+r.token+`"}`, cookie)
	if code != http.StatusOK || out["organization"].(map[string]any)["owned"] != true {
		t.Errorf("json claim: %d %v", code, out)
	}
	code, out = post("ClaimOwnership", `{"token":"`+r.token+`"}`, cookie)
	if code != http.StatusBadRequest || out["code"] != "failed_precondition" {
		t.Errorf("json second claim: %d %v", code, out)
	}
}

func TestInternalErrorsAreOpaque(t *testing.T) {
	r := newRig(t)
	r.orgSt.ErrOn = "get"
	client := hubv1connect.NewOrganizationServiceClient(http.DefaultClient, r.srv.URL)
	_, err := client.GetOrganization(context.Background(), connect.NewRequest(&hubv1.GetOrganizationRequest{}))
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeInternal || strings.Contains(ce.Message(), "db down") {
		t.Errorf("want opaque internal error, got %v", err)
	}
}
