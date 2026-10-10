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
	"github.com/gravel-project/gravel/internal/apps"
	"github.com/gravel-project/gravel/internal/store"
)

// AppServer serves gravel.hub.v1.AppService (ADR-0015): the authorization endpoint's checks and
// consent for the /oauth/authorize page, and the apps a member allowed for the account page. A
// member uses these in person: no bearer token, an app's or a member's, may call them.
type AppServer struct {
	hubv1connect.UnimplementedAppServiceHandler
	apps   *apps.Service
	logger *slog.Logger
}

// NewAppServer wires the service.
func NewAppServer(svc *apps.Service, logger *slog.Logger) *AppServer {
	return &AppServer{apps: svc, logger: logger}
}

func authorizationRequest(p *hubv1.AuthorizationParams) apps.AuthorizationRequest {
	return apps.AuthorizationRequest{
		ResponseType: p.GetResponseType(), ClientID: p.GetClientId(), RedirectURI: p.GetRedirectUri(), Scope: p.GetScope(),
		CodeChallenge: p.GetCodeChallenge(), CodeChallengeMethod: p.GetCodeChallengeMethod(),
	}
}

func scopeInfos(scopes []string) []*hubv1.ScopeInfo {
	out := make([]*hubv1.ScopeInfo, 0, len(scopes))
	for _, sc := range scopes {
		out = append(out, &hubv1.ScopeInfo{Name: sc, Description: apps.MemberScopes[sc]})
	}
	return out
}

// authorizationError maps the refusals that must not be redirected: an unknown or revoked client
// and an unregistered redirect URI are not_found, and say which.
func (s *AppServer) authorizationError(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, apps.ErrInvalidClient):
		return connect.NewError(connect.CodeNotFound, errors.New("no such app: the client id is unknown or the app was revoked"))
	case errors.Is(err, apps.ErrInvalidRedirectURI):
		return connect.NewError(connect.CodeNotFound, errors.New("redirect_uri is not one the app registered"))
	}
	return mapError(ctx, s.logger, err)
}

// PrepareAuthorization checks a request; anyone may call it, since the page checks the app before
// sending a visitor to log in. With a session it also says whether consent is needed.
func (s *AppServer) PrepareAuthorization(ctx context.Context, req *connect.Request[hubv1.PrepareAuthorizationRequest]) (*connect.Response[hubv1.PrepareAuthorizationResponse], error) {
	uid, err := sessionCaller(ctx) // anonymous is fine; a bearer token is not
	if err != nil && connect.CodeOf(err) != connect.CodeUnauthenticated {
		return nil, err
	}
	auth, err := s.apps.CheckAuthorization(ctx, authorizationRequest(req.Msg.GetParams()), uid)
	var refusal *apps.AuthorizationError
	if errors.As(err, &refusal) {
		// The client and the redirect URI were good: the app hears about the rest.
		return connect.NewResponse(&hubv1.PrepareAuthorizationResponse{AppName: auth.App.Name, ClientId: auth.App.ClientID, ConsentRequired: true,
			Error: refusal.Code, ErrorDescription: refusal.Description}), nil
	}
	if err != nil {
		return nil, s.authorizationError(ctx, err)
	}
	return connect.NewResponse(&hubv1.PrepareAuthorizationResponse{AppName: auth.App.Name, ClientId: auth.App.ClientID,
		Scopes: scopeInfos(auth.Scopes), ConsentRequired: auth.ConsentRequired}), nil
}

// Authorize records the member's consent and issues a code.
func (s *AppServer) Authorize(ctx context.Context, req *connect.Request[hubv1.AuthorizeRequest]) (*connect.Response[hubv1.AuthorizeResponse], error) {
	uid, err := sessionCaller(ctx)
	if err != nil {
		return nil, err
	}
	code, auth, err := s.apps.Authorize(ctx, authorizationRequest(req.Msg.GetParams()), uid)
	var refusal *apps.AuthorizationError
	if errors.As(err, &refusal) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New(refusal.Code+": "+refusal.Description))
	}
	if err != nil {
		return nil, s.authorizationError(ctx, err)
	}
	return connect.NewResponse(&hubv1.AuthorizeResponse{Code: code, Scopes: auth.Scopes}), nil
}

// ListAppGrants lists the apps the member allowed.
func (s *AppServer) ListAppGrants(ctx context.Context, _ *connect.Request[hubv1.ListAppGrantsRequest]) (*connect.Response[hubv1.ListAppGrantsResponse], error) {
	uid, err := sessionCaller(ctx)
	if err != nil {
		return nil, err
	}
	grants, err := s.apps.Grants(ctx, uid)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	out := &hubv1.ListAppGrantsResponse{}
	for _, g := range grants {
		out.Grants = append(out.Grants, &hubv1.AppGrant{ClientId: g.ClientID, AppName: g.AppName, Scopes: scopeInfos(g.Scopes), GrantedAt: timestamppb.New(g.GrantedAt)})
	}
	return connect.NewResponse(out), nil
}

// RevokeAppGrant withdraws a grant and the member's tokens for the app.
func (s *AppServer) RevokeAppGrant(ctx context.Context, req *connect.Request[hubv1.RevokeAppGrantRequest]) (*connect.Response[hubv1.RevokeAppGrantResponse], error) {
	uid, err := sessionCaller(ctx)
	if err != nil {
		return nil, err
	}
	clientID := strings.TrimSpace(req.Msg.GetClientId())
	if clientID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("client_id is required"))
	}
	n, err := s.apps.RevokeGrant(ctx, uid, clientID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("you have not allowed that app"))
	}
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	return connect.NewResponse(&hubv1.RevokeAppGrantResponse{RevokedTokens: int32(min(n, 1<<31-1))}), nil //nolint:gosec // bounded above
}
