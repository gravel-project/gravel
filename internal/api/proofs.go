package api

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/gravel-project/gravel/drivers"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/internal/identity/proof"
)

// Proof of control (gravel#5): a code the member types in a game server's chat.

// SetProofs gives the service proof of control: the proofs, and where to read which servers take a
// code in chat (a server whose status has drivers.CapChat) and at which provider.
func (s *IdentityServer) SetProofs(p *proof.Service, srv ServerReader, obs Observer) {
	s.proofs, s.proofServers, s.proofObs = p, srv, obs
}

// StartProof draws a code for the caller to type in a server's chat.
func (s *IdentityServer) StartProof(ctx context.Context, req *connect.Request[hubv1.StartProofRequest]) (*connect.Response[hubv1.StartProofResponse], error) {
	uid, err := sessionCaller(ctx)
	if err != nil {
		return nil, err
	}
	if s.proofs == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("this hub doesn't take proofs in game"))
	}
	serverID := strings.TrimSpace(req.Msg.GetServerId())
	provider, err := s.chatServer(ctx, serverID)
	if err != nil {
		return nil, err
	}
	if c, ok := s.proofs.Channel(proof.GameChatName); !ok || c.Provider() != provider {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("this server's players are known by "+provider+", which this hub doesn't prove in chat"))
	}
	st, err := s.proofs.Start(ctx, uid, proof.GameChatName, serverID, strings.TrimSpace(req.Msg.GetSubject()))
	if err != nil {
		return nil, s.proofError(ctx, err)
	}
	return connect.NewResponse(&hubv1.StartProofResponse{ProofId: st.ID.String(), Code: st.Code, ExpiresAt: timestamppb.New(st.ExpiresAt),
		Instructions: st.Instructions, Provider: provider}), nil
}

// GetProof is one of the caller's proofs.
func (s *IdentityServer) GetProof(ctx context.Context, req *connect.Request[hubv1.GetProofRequest]) (*connect.Response[hubv1.GetProofResponse], error) {
	uid, err := sessionCaller(ctx)
	if err != nil {
		return nil, err
	}
	if s.proofs == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("this hub doesn't take proofs in game"))
	}
	id, err := uuid.Parse(req.Msg.GetProofId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("proof_id is not a proof's id"))
	}
	p, err := s.proofs.Get(ctx, uid, id)
	if err != nil {
		return nil, s.proofError(ctx, err)
	}
	return connect.NewResponse(&hubv1.GetProofResponse{ProofId: p.ID.String(), ServerId: p.Scope, Provider: p.Provider,
		ExpiresAt: timestamppb.New(p.ExpiresAt), State: p.State, Subject: p.ProvenSubject}), nil
}

// chatServer is the provider a server's players are known by, when it takes a code in chat now.
func (s *IdentityServer) chatServer(ctx context.Context, id string) (string, error) {
	if id == "" {
		return "", connect.NewError(connect.CodeInvalidArgument, errors.New("server_id is required"))
	}
	srv, err := s.proofServers.Server(ctx, id)
	if err != nil {
		return "", mapError(ctx, s.logger, err)
	}
	o, ok := s.proofObs.Observation(id)
	if !ok || !slices.Contains(o.Capabilities, drivers.CapChat) {
		return "", connect.NewError(connect.CodeFailedPrecondition, errors.New("the hub doesn't read this server's chat"))
	}
	if o.State != "ok" || !o.Reachable {
		return "", connect.NewError(connect.CodeFailedPrecondition, errors.New("the server isn't running; start it, or pick another"))
	}
	games, err := s.proofServers.Games(ctx)
	if err != nil {
		return "", mapError(ctx, s.logger, err)
	}
	for _, g := range games {
		if g.ID == srv.Game {
			return g.IdentityProvider, nil
		}
	}
	return "", connect.NewError(connect.CodeFailedPrecondition, errors.New("the server's game isn't enabled"))
}

func (s *IdentityServer) proofError(ctx context.Context, err error) error {
	var tm *proof.TooManyError
	switch {
	case errors.As(err, &tm):
		e := connect.NewError(connect.CodeResourceExhausted, err)
		e.Meta().Set("Retry-After", strconv.Itoa(int(tm.RetryAfter.Round(time.Second)/time.Second)))
		return e
	case errors.Is(err, proof.ErrNotLinked):
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("that account isn't linked to you: leave subject empty to link a new one"))
	case errors.Is(err, proof.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, errors.New("no such proof"))
	case errors.Is(err, proof.ErrUnknownChannel):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return mapError(ctx, s.logger, err)
}
