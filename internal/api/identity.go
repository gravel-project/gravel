package api

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/session"
	"github.com/gravel-project/gravel/internal/store"
)

// IdentityServer serves gravel.hub.v1.IdentityService: the caller's account and the lookups
// role sync needs. Every procedure needs the session cookie; the login flow itself is the
// browser surface under /auth/.
type IdentityServer struct {
	hubv1connect.UnimplementedIdentityServiceHandler
	ids    *identity.Service
	sess   *session.Manager
	org    *org.Service
	logger *slog.Logger
}

// NewIdentityServer wires the services.
func NewIdentityServer(ids *identity.Service, sess *session.Manager, orgSvc *org.Service, logger *slog.Logger) *IdentityServer {
	return &IdentityServer{ids: ids, sess: sess, org: orgSvc, logger: logger}
}

// GetMe returns the caller and their linked identities.
func (s *IdentityServer) GetMe(ctx context.Context, _ *connect.Request[hubv1.GetMeRequest]) (*connect.Response[hubv1.GetMeResponse], error) {
	uid, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	u, err := s.user(ctx, uid)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.GetMeResponse{User: u}), nil
}

// UnlinkIdentity removes one of the caller's identities; the last one is refused.
func (s *IdentityServer) UnlinkIdentity(ctx context.Context, req *connect.Request[hubv1.UnlinkIdentityRequest]) (*connect.Response[hubv1.UnlinkIdentityResponse], error) {
	uid, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	provider, subject := strings.TrimSpace(req.Msg.GetProvider()), strings.TrimSpace(req.Msg.GetSubject())
	if provider == "" || subject == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("provider and subject are required"))
	}
	if err := s.ids.Unlink(ctx, uid, provider, subject); err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	u, err := s.user(ctx, uid)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.UnlinkIdentityResponse{User: u}), nil
}

// RevokeSessions logs the caller out everywhere, this session included.
func (s *IdentityServer) RevokeSessions(ctx context.Context, _ *connect.Request[hubv1.RevokeSessionsRequest]) (*connect.Response[hubv1.RevokeSessionsResponse], error) {
	uid, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	n, err := s.sess.RevokeAll(ctx, uid)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	resp := connect.NewResponse(&hubv1.RevokeSessionsResponse{Revoked: int32(min(n, 1<<31-1))}) //nolint:gosec // bounded above
	resp.Header().Add("Set-Cookie", s.sess.Cookie(session.CookieSession, "", 0).String())
	return resp, nil
}

// LookupUser resolves a (provider, subject) pair to its user. Owner only until first-party
// apps have their own credentials (gravel#6).
func (s *IdentityServer) LookupUser(ctx context.Context, req *connect.Request[hubv1.LookupUserRequest]) (*connect.Response[hubv1.LookupUserResponse], error) {
	uid, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	o, err := s.org.Get(ctx)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	if o.OwnerUserID == nil || *o.OwnerUserID != uid {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only the owner may look users up"))
	}
	provider, subject := strings.TrimSpace(req.Msg.GetProvider()), strings.TrimSpace(req.Msg.GetSubject())
	if provider == "" || subject == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("provider and subject are required"))
	}
	u, err := s.ids.Lookup(ctx, provider, subject)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	return connect.NewResponse(&hubv1.LookupUserResponse{User: toProtoUser(u, o)}), nil
}

func (s *IdentityServer) user(ctx context.Context, uid uuid.UUID) (*hubv1.User, error) {
	u, err := s.ids.Me(ctx, uid)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	o, err := s.org.Get(ctx)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	return toProtoUser(u, o), nil
}

func toProtoUser(u identity.User, o store.Organization) *hubv1.User {
	p := &hubv1.User{
		Id:          u.ID.String(),
		DisplayName: u.DisplayName,
		Owner:       o.OwnerUserID != nil && *o.OwnerUserID == u.ID,
		CreatedAt:   timestamppb.New(u.CreatedAt),
	}
	if u.LastLoginAt != nil {
		p.LastLoginAt = timestamppb.New(*u.LastLoginAt)
	}
	for _, i := range u.Identities {
		p.Identities = append(p.Identities, toProtoIdentity(i))
	}
	return p
}

func toProtoIdentity(i store.Identity) *hubv1.Identity {
	p := &hubv1.Identity{
		Provider:           i.Provider,
		Subject:            i.Subject,
		DisplayName:        i.DisplayName,
		AvatarUrl:          i.AvatarURL,
		VerificationMethod: i.VerificationMethod,
		VerifiedAt:         timestamppb.New(i.VerifiedAt),
		LinkedAt:           timestamppb.New(i.LinkedAt),
	}
	if i.LastLoginAt != nil {
		p.LastLoginAt = timestamppb.New(*i.LastLoginAt)
	}
	return p
}
