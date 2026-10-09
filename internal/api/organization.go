// Package api implements the hub's Connect services over the domain packages. It maps domain
// errors to Connect codes and never leaks internal error text to clients.
package api

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
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
		return nil, mapError(ctx, s.logger, err)
	}
	return connect.NewResponse(&hubv1.GetOrganizationResponse{Organization: toProto(o)}), nil
}

// ClaimOwnership consumes the one-time owner-claim token and makes the caller the owner.
func (s *OrganizationServer) ClaimOwnership(ctx context.Context, req *connect.Request[hubv1.ClaimOwnershipRequest]) (*connect.Response[hubv1.ClaimOwnershipResponse], error) {
	uid, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Msg.GetToken()) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("token is required"))
	}
	o, err := s.org.Claim(ctx, req.Msg.GetToken(), uid)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	return connect.NewResponse(&hubv1.ClaimOwnershipResponse{Organization: toProto(o)}), nil
}

// GetOrganizationSettings returns the settings; public, the pages render the theme before login.
func (s *OrganizationServer) GetOrganizationSettings(ctx context.Context, _ *connect.Request[hubv1.GetOrganizationSettingsRequest]) (*connect.Response[hubv1.GetOrganizationSettingsResponse], error) {
	set, at, err := s.org.Settings(ctx)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	return connect.NewResponse(&hubv1.GetOrganizationSettingsResponse{Settings: settingsToProto(set, at)}), nil
}

// UpdateOrganizationSettings replaces the settings; the owner only.
func (s *OrganizationServer) UpdateOrganizationSettings(ctx context.Context, req *connect.Request[hubv1.UpdateOrganizationSettingsRequest]) (*connect.Response[hubv1.UpdateOrganizationSettingsResponse], error) {
	if err := requireOwner(ctx, s.org, s.logger); err != nil {
		return nil, err
	}
	set, err := s.org.UpdateSettings(ctx, settingsFromProto(req.Msg.GetSettings()))
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	_, at, err := s.org.Settings(ctx)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	return connect.NewResponse(&hubv1.UpdateOrganizationSettingsResponse{Settings: settingsToProto(set, at)}), nil
}

func settingsToProto(set org.Settings, at time.Time) *hubv1.OrganizationSettings {
	tokens := func(t org.Tokens) *hubv1.ThemeTokens {
		return &hubv1.ThemeTokens{Accent: t.Accent, Background: t.Background, Foreground: t.Foreground, Muted: t.Muted, Line: t.Line, Ok: t.OK, Err: t.Err}
	}
	p := &hubv1.OrganizationSettings{
		Version: int32(set.Version), //nolint:gosec // a small constant
		Theme:   &hubv1.Theme{Light: tokens(set.Theme.Light), Dark: tokens(set.Theme.Dark), Font: set.Theme.Font, LogoUrl: set.Theme.LogoURL, FaviconUrl: set.Theme.FaviconURL},
	}
	for _, l := range set.Nav {
		p.Nav = append(p.Nav, &hubv1.NavLink{Label: l.Label, Url: l.URL, Placement: l.Placement, Role: l.Role})
	}
	if !at.IsZero() {
		p.UpdatedAt = timestamppb.New(at)
	}
	return p
}

func settingsFromProto(p *hubv1.OrganizationSettings) org.Settings {
	tokens := func(t *hubv1.ThemeTokens) org.Tokens {
		return org.Tokens{Accent: t.GetAccent(), Background: t.GetBackground(), Foreground: t.GetForeground(), Muted: t.GetMuted(), Line: t.GetLine(), OK: t.GetOk(), Err: t.GetErr()}
	}
	set := org.Settings{
		Version: int(p.GetVersion()),
		Theme:   org.Theme{Light: tokens(p.GetTheme().GetLight()), Dark: tokens(p.GetTheme().GetDark()), Font: p.GetTheme().GetFont(), LogoURL: p.GetTheme().GetLogoUrl(), FaviconURL: p.GetTheme().GetFaviconUrl()},
	}
	if set.Version == 0 {
		set.Version = org.SettingsVersion
	}
	for _, l := range p.GetNav() {
		set.Nav = append(set.Nav, org.NavLink{Label: l.GetLabel(), URL: l.GetUrl(), Placement: l.GetPlacement(), Role: l.GetRole()})
	}
	return set
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
	if o.OwnerUserID != nil {
		p.OwnerUserId = o.OwnerUserID.String()
	}
	return p
}
