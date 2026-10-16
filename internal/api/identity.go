package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/apps"
	"github.com/gravel-project/gravel/internal/authz"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/identity/proof"
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
	access *Access
	logger *slog.Logger
	// baseURL is what password links are built on (WithBaseURL).
	baseURL string

	// Proof of control (SetProofs); nil when the hub takes none.
	proofs       *proof.Service
	proofServers ServerReader
	proofObs     Observer
}

// NewIdentityServer wires the services.
func NewIdentityServer(ids *identity.Service, sess *session.Manager, orgSvc *org.Service, access *Access, logger *slog.Logger) *IdentityServer {
	return &IdentityServer{ids: ids, sess: sess, org: orgSvc, access: access, logger: logger}
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
	if err := s.recoveryAddress(ctx, uid, u); err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.GetMeResponse{User: u}), nil
}

// recoveryAddress puts the recovery address on the caller's local identity (gravel#234), for a
// session only: an app acting for the member never sees it.
func (s *IdentityServer) recoveryAddress(ctx context.Context, uid uuid.UUID, u *hubv1.User) error {
	if _, app := apps.MemberFromContext(ctx); app {
		return nil
	}
	for _, i := range u.GetIdentities() {
		if i.GetProvider() != identity.ProviderLocal {
			continue
		}
		a, err := s.ids.RecoveryAddressOf(ctx, uid)
		if errors.Is(err, identity.ErrNoLocalLogin) {
			return nil
		}
		if err != nil {
			return mapError(ctx, s.logger, err)
		}
		i.RecoveryEmail = a.Email
		if a.VerifiedAt != nil {
			i.RecoveryEmailVerifiedAt = timestamppb.New(*a.VerifiedAt)
		}
	}
	return nil
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

// Logout ends the calling session and clears its cookie.
func (s *IdentityServer) Logout(ctx context.Context, _ *connect.Request[hubv1.LogoutRequest]) (*connect.Response[hubv1.LogoutResponse], error) {
	sess, ok := session.FromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("log in first"))
	}
	if err := s.sess.Delete(ctx, sess); err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	resp := connect.NewResponse(&hubv1.LogoutResponse{})
	resp.Header().Add("Set-Cookie", s.sess.Cookie(session.CookieSession, "", 0).String())
	return resp, nil
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

// LookupUser resolves a (provider, subject) pair to its user: an app with identity:read (a
// role-sync reconciler), or the owner.
func (s *IdentityServer) LookupUser(ctx context.Context, req *connect.Request[hubv1.LookupUserRequest]) (*connect.Response[hubv1.LookupUserResponse], error) {
	if _, err := s.access.Require(ctx, authz.IdentityRead, authz.Target{}); err != nil {
		return nil, err
	}
	o, err := s.org.Get(ctx)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
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

// Page bounds for the listings.
const (
	defaultPageSize  = 100
	maxPageSize      = 500
	defaultEventPage = 100
	maxEventPage     = 1000
)

// ListUsers pages through the members with their identities, oldest first. The page token is
// the last user's (created_at, id), opaque to callers.
func (s *IdentityServer) ListUsers(ctx context.Context, req *connect.Request[hubv1.ListUsersRequest]) (*connect.Response[hubv1.ListUsersResponse], error) {
	if _, err := s.access.Require(ctx, authz.IdentityRead, authz.Target{}); err != nil {
		return nil, err
	}
	size := int(req.Msg.GetPageSize())
	switch {
	case size < 0:
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("page_size must not be negative"))
	case size == 0:
		size = defaultPageSize
	case size > maxPageSize:
		size = maxPageSize
	}
	after, err := decodePageToken(req.Msg.GetPageToken())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	o, err := s.org.Get(ctx)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	// One more than the page says whether there is a next page.
	users, err := s.ids.ListUsers(ctx, after, size+1)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	resp := &hubv1.ListUsersResponse{}
	if len(users) > size {
		users = users[:size]
		last := users[len(users)-1]
		resp.NextPageToken = encodePageToken(store.UserCursor{CreatedAt: last.CreatedAt, ID: last.ID})
	}
	for _, u := range users {
		resp.Users = append(resp.Users, toProtoUser(u, o))
	}
	return connect.NewResponse(resp), nil
}

// ListIdentityEvents returns the identity log after a position, oldest first.
func (s *IdentityServer) ListIdentityEvents(ctx context.Context, req *connect.Request[hubv1.ListIdentityEventsRequest]) (*connect.Response[hubv1.ListIdentityEventsResponse], error) {
	if _, err := s.access.Require(ctx, authz.IdentityRead, authz.Target{}); err != nil {
		return nil, err
	}
	after := req.Msg.GetAfterId()
	if after < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("after_id must not be negative"))
	}
	limit := int(req.Msg.GetLimit())
	switch {
	case limit < 0:
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("limit must not be negative"))
	case limit == 0:
		limit = defaultEventPage
	case limit > maxEventPage:
		limit = maxEventPage
	}
	events, head, err := s.ids.ListEvents(ctx, after, limit)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	resp := &hubv1.ListIdentityEventsResponse{NextAfterId: after, HeadId: head}
	for _, e := range events {
		resp.Events = append(resp.Events, &hubv1.IdentityEvent{
			Id: e.ID, UserId: e.UserID.String(), Provider: e.Provider, Subject: e.Subject, Event: e.Event, At: timestamppb.New(e.At),
		})
		resp.NextAfterId = e.ID
	}
	return connect.NewResponse(resp), nil
}

// A page token is the base64 of "<created_at RFC 3339 nano>|<user id>", nothing a caller
// should build; an unparseable one is an invalid argument.
func encodePageToken(c store.UserCursor) string {
	return base64.RawURLEncoding.EncodeToString([]byte(c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID.String()))
}

func decodePageToken(token string) (*store.UserCursor, error) {
	if token == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, errors.New("page_token is not one this hub issued")
	}
	at, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return nil, errors.New("page_token is not one this hub issued")
	}
	created, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return nil, errors.New("page_token is not one this hub issued")
	}
	uid, err := uuid.Parse(id)
	if err != nil {
		return nil, errors.New("page_token is not one this hub issued")
	}
	return &store.UserCursor{CreatedAt: created, ID: uid}, nil
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
		Roles:       u.Roles,
	}
	if u.LastLoginAt != nil {
		p.LastLoginAt = timestamppb.New(*u.LastLoginAt)
	}
	for _, c := range authz.ForMember(u.ID, p.Owner, u.AuthzGrants()).Capabilities() {
		p.Capabilities = append(p.Capabilities, string(c))
	}
	for _, g := range u.Grants {
		p.Grants = append(p.Grants, toProtoGrant(g))
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

// SetUserRole grants or revokes a member's role: a caller holding roles:grant (the owner, in
// person).
func (s *IdentityServer) SetUserRole(ctx context.Context, req *connect.Request[hubv1.SetUserRoleRequest]) (*connect.Response[hubv1.SetUserRoleResponse], error) {
	// roles:grant somewhere gets the caller to the rules, which check its scope (ADR-0027 §5).
	p, err := s.access.RequireAny(ctx, authz.RolesGrant)
	if err != nil {
		return nil, err
	}
	uid, err := uuid.Parse(req.Msg.GetUserId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("user_id is not a member id"))
	}
	u, err := s.ids.SetRole(ctx, p, uid, req.Msg.GetRole(), strings.TrimSpace(req.Msg.GetGameId()), req.Msg.GetGranted())
	switch {
	case errors.Is(err, identity.ErrUnknownRole):
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("role %q is not one of %s", req.Msg.GetRole(), strings.Join(store.Roles, ", ")))
	case errors.Is(err, identity.ErrUnknownGame):
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("game %q is not one this hub has", req.Msg.GetGameId()))
	case errors.Is(err, identity.ErrGrantRefused):
		return nil, connect.NewError(connect.CodePermissionDenied, err)
	case err != nil:
		return nil, mapError(ctx, s.logger, err)
	}
	o, err := s.org.Get(ctx)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	return connect.NewResponse(&hubv1.SetUserRoleResponse{User: toProtoUser(u, o)}), nil
}

// ListRoleGrants lists who holds which role where.
func (s *IdentityServer) ListRoleGrants(ctx context.Context, req *connect.Request[hubv1.ListRoleGrantsRequest]) (*connect.Response[hubv1.ListRoleGrantsResponse], error) {
	if _, err := s.access.Require(ctx, authz.IdentityRead, authz.Target{}); err != nil {
		return nil, err
	}
	var ids []uuid.UUID
	if raw := req.Msg.GetUserId(); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("user_id is not a member id"))
		}
		ids = []uuid.UUID{id}
	}
	grants, err := s.ids.RoleGrants(ctx, ids)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	out := &hubv1.ListRoleGrantsResponse{}
	for _, g := range grants {
		out.Grants = append(out.Grants, toProtoGrant(g))
	}
	return connect.NewResponse(out), nil
}

func toProtoGrant(g store.RoleGrant) *hubv1.RoleGrant {
	p := &hubv1.RoleGrant{UserId: g.UserID.String(), Role: g.Role, GameId: g.GameID, GrantedAt: timestamppb.New(g.GrantedAt)}
	if g.GrantedBy != nil {
		p.GrantedByUserId = g.GrantedBy.String()
	}
	if g.GrantedByApp != nil {
		p.GrantedByAppId = g.GrantedByApp.String()
	}
	return p
}
