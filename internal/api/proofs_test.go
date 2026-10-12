package api_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"connectrpc.com/connect"

	"github.com/gravel-project/gravel/drivers"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/apps"
	"github.com/gravel-project/gravel/internal/identity/proof"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/store"
)

// proofReader is a hub with one CS2 server, htg-cs2-1, whose players are known by Steam.
type proofReader struct{}

func (proofReader) Games(context.Context) ([]servers.Game, error) {
	return []servers.Game{{Spec: servers.Spec{ID: "cs2", Name: "Counter-Strike 2", IdentityProvider: "steam"}}}, nil
}

func (p proofReader) Servers(ctx context.Context) ([]servers.Server, error) {
	s, err := p.Server(ctx, "htg-cs2-1")
	return []servers.Server{s}, err
}

func (proofReader) Server(_ context.Context, id string) (servers.Server, error) {
	if id != "htg-cs2-1" {
		return servers.Server{}, store.ErrNotFound
	}
	return servers.Server{ID: id, Name: "HTG CS2", Game: "cs2"}, nil
}

// Who may ask for a code, on which server, and what a code's state reads (gravel#5).
func TestProofProcedures(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	jo := r.user(t, "Jo", "1")
	sam := r.user(t, "Sam", "2")
	client := func(c *http.Client) hubv1connect.IdentityServiceClient {
		return hubv1connect.NewIdentityServiceClient(c, r.srv.URL)
	}
	joC, samC := client(cookieClient(http.DefaultClient, r.login(t, jo.ID))), client(cookieClient(http.DefaultClient, r.login(t, sam.ID)))
	start := func(c hubv1connect.IdentityServiceClient, server string) (*connect.Response[hubv1.StartProofResponse], error) {
		return c.StartProof(ctx, connect.NewRequest(&hubv1.StartProofRequest{ServerId: server}))
	}

	if _, err := start(client(http.DefaultClient), "htg-cs2-1"); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("anonymous: %v", err)
	}
	if _, err := start(client(bearerClient(http.DefaultClient, r.bearer(t, "bot", apps.ScopeIdentityRead))), "htg-cs2-1"); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("an app: %v", err)
	}
	if _, err := start(client(bearerClient(http.DefaultClient, r.memberToken(t, jo.ID, []string{apps.ScopeIdentityRead}, apps.ScopeIdentityRead))), "htg-cs2-1"); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a member's token: %v", err)
	}
	if _, err := start(joC, "htg-cs2-1"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("a server the hub hasn't observed: %v", err)
	}
	r.obs.obs["htg-cs2-1"] = servers.Observation{ServerID: "htg-cs2-1", State: servers.StateOK, Reachable: true, Capabilities: []string{drivers.CapStatus}}
	if _, err := start(joC, "htg-cs2-1"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("a server whose chat the hub doesn't read: %v", err)
	}
	r.obs.obs["htg-cs2-1"] = servers.Observation{ServerID: "htg-cs2-1", State: servers.StateParked, Capabilities: []string{drivers.CapChat}}
	if _, err := start(joC, "htg-cs2-1"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("a parked server: %v", err)
	}
	r.obs.obs["htg-cs2-1"] = servers.Observation{ServerID: "htg-cs2-1", State: servers.StateOK, Reachable: true, Capabilities: []string{drivers.CapChat}}
	for _, id := range []string{"", "nope"} {
		if _, err := start(joC, id); err == nil {
			t.Errorf("server %q accepted", id)
		}
	}
	if _, err := joC.StartProof(ctx, connect.NewRequest(&hubv1.StartProofRequest{ServerId: "htg-cs2-1", Subject: "76561190000000001"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("proving again an account not linked: %v", err)
	}

	got, err := start(joC, "htg-cs2-1")
	if err != nil || len(got.Msg.GetCode()) != 9 || got.Msg.GetProvider() != "steam" || got.Msg.GetInstructions() == "" || got.Msg.GetExpiresAt() == nil {
		t.Fatalf("start: %v %v", got, err)
	}
	id := got.Msg.GetProofId()
	if p, err := joC.GetProof(ctx, connect.NewRequest(&hubv1.GetProofRequest{ProofId: id})); err != nil || p.Msg.GetState() != "open" || p.Msg.GetServerId() != "htg-cs2-1" {
		t.Errorf("get: %v %v", p, err)
	}
	if _, err := r.proofs.Present(ctx, proof.Evidence{Channel: proof.GameChatName, Scope: "htg-cs2-1", Account: proof.Account{Subject: "765"}, Text: "!link " + got.Msg.GetCode()}); err != nil {
		t.Fatal(err)
	}
	if p, err := joC.GetProof(ctx, connect.NewRequest(&hubv1.GetProofRequest{ProofId: id})); err != nil || p.Msg.GetState() != "proven" || p.Msg.GetSubject() != "765" {
		t.Errorf("after the chat line: %v %v", p, err)
	}
	if _, err := samC.GetProof(ctx, connect.NewRequest(&hubv1.GetProofRequest{ProofId: id})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("another member's proof: %v", err)
	}
	if _, err := joC.GetProof(ctx, connect.NewRequest(&hubv1.GetProofRequest{ProofId: "x"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a bad id: %v", err)
	}
	for range proof.CodesPerHour - 1 {
		if _, err := start(joC, "htg-cs2-1"); err != nil {
			t.Fatal(err)
		}
	}
	_, err = start(joC, "htg-cs2-1")
	var ce *connect.Error
	if connect.CodeOf(err) != connect.CodeResourceExhausted || !errors.As(err, &ce) || ce.Meta().Get("Retry-After") == "" {
		t.Errorf("past five an hour: %v", err)
	}
}
