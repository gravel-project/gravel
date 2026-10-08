// Package api implements the hub's Connect services over the domain packages. It maps domain
// errors to Connect codes and never leaks internal error text to clients.
package api

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/httpx"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/store"
)

// OrganizationServer serves gravel.hub.v1.OrganizationService.
type OrganizationServer struct {
	hubv1connect.UnimplementedOrganizationServiceHandler
	org    *org.Service
	logger *slog.Logger
}

// NewOrganizationServer wires the service.
func NewOrganizationServer(svc *org.Service, logger *slog.Logger) *OrganizationServer {
	return &OrganizationServer{org: svc, logger: logger}
}

// GetOrganization returns the built-in organization.
func (s *OrganizationServer) GetOrganization(ctx context.Context, _ *connect.Request[hubv1.GetOrganizationRequest]) (*connect.Response[hubv1.GetOrganizationResponse], error) {
	o, err := s.org.Get(ctx)
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	return connect.NewResponse(&hubv1.GetOrganizationResponse{Organization: toProto(o)}), nil
}

// ClaimOwnership consumes the one-time owner-claim token.
func (s *OrganizationServer) ClaimOwnership(ctx context.Context, req *connect.Request[hubv1.ClaimOwnershipRequest]) (*connect.Response[hubv1.ClaimOwnershipResponse], error) {
	if strings.TrimSpace(req.Msg.GetToken()) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("token is required"))
	}
	o, err := s.org.Claim(ctx, req.Msg.GetToken())
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	return connect.NewResponse(&hubv1.ClaimOwnershipResponse{Organization: toProto(o)}), nil
}

func (s *OrganizationServer) mapError(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, org.ErrAlreadyOwned):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, org.ErrInvalidToken):
		return connect.NewError(connect.CodePermissionDenied, err)
	case errors.Is(err, store.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, errors.New("the hub has no organization yet"))
	}
	s.logger.ErrorContext(ctx, "internal error", "error", err.Error(), "request_id", httpx.RequestIDFromContext(ctx))
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}

func toProto(o store.Organization) *hubv1.Organization {
	p := &hubv1.Organization{
		Id:        o.ID.String(),
		Name:      o.Name,
		Owned:     o.Owned(),
		CreatedAt: timestamppb.New(o.CreatedAt),
	}
	if o.ClaimedAt != nil {
		p.ClaimedAt = timestamppb.New(*o.ClaimedAt)
	}
	return p
}
