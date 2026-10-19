package api

// The operator's admin procedures (#195, ADR-0031): what `gravel-hub` did against Postgres on the
// host, over the API behind its capability. The host's commands stay for bootstrap (before anyone
// can log in) and for what only the host may change (HostOnly). Each is a member's, in person: no
// app may hold these capabilities, and members' tokens are refused (memberProcedures).

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/internal/apps"
	"github.com/gravel-project/gravel/internal/authz"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/store"
)

// WithAccess lets the service check the operator's procedures (CreateApp, ListApps, RevokeApp).
func (s *AppServer) WithAccess(a *Access) *AppServer {
	s.access = a
	return s
}

func appMessage(a store.App) *hubv1.App {
	p := &hubv1.App{ClientId: a.ClientID, Name: a.Name, Scopes: a.Scopes, RedirectUris: a.RedirectURIs, CreatedAt: timestamppb.New(a.CreatedAt)}
	if a.RevokedAt != nil {
		p.RevokedAt = timestamppb.New(*a.RevokedAt)
	}
	return p
}

// manage checks apps:manage; a service built without WithAccess refuses.
func (s *AppServer) manage(ctx context.Context) (authz.Principal, error) {
	if s.access == nil {
		return authz.Principal{}, connect.NewError(connect.CodeUnimplemented, errors.New("this hub registers no apps over the API"))
	}
	return s.access.Require(ctx, authz.AppsManage, authz.Target{})
}

// CreateApp registers an app and answers its secret once: apps:manage.
func (s *AppServer) CreateApp(ctx context.Context, req *connect.Request[hubv1.CreateAppRequest]) (*connect.Response[hubv1.CreateAppResponse], error) {
	p, err := s.manage(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	created, err := s.apps.Create(ctx, changedBy(ctx, p), m.GetName(), m.GetScopes(), m.GetRedirectUris()...)
	switch {
	case errors.Is(err, apps.ErrInvalidName), errors.Is(err, apps.ErrInvalidScope), errors.Is(err, apps.ErrInvalidRedirectURI):
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	case err != nil:
		return nil, mapError(ctx, s.logger, err)
	}
	return connect.NewResponse(&hubv1.CreateAppResponse{App: appMessage(created.App), ClientSecret: created.Secret}), nil
}

// ListApps lists every app: apps:manage.
func (s *AppServer) ListApps(ctx context.Context, _ *connect.Request[hubv1.ListAppsRequest]) (*connect.Response[hubv1.ListAppsResponse], error) {
	if _, err := s.manage(ctx); err != nil {
		return nil, err
	}
	list, err := s.apps.List(ctx)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	out := &hubv1.ListAppsResponse{}
	for _, a := range list {
		out.Apps = append(out.Apps, appMessage(a))
	}
	return connect.NewResponse(out), nil
}

// RevokeApp revokes an app: apps:manage.
func (s *AppServer) RevokeApp(ctx context.Context, req *connect.Request[hubv1.RevokeAppRequest]) (*connect.Response[hubv1.RevokeAppResponse], error) {
	p, err := s.manage(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.apps.Revoke(ctx, changedBy(ctx, p), strings.TrimSpace(req.Msg.GetClientId())); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("no app with that client id, or it is already revoked"))
		}
		return nil, mapError(ctx, s.logger, err)
	}
	return connect.NewResponse(&hubv1.RevokeAppResponse{}), nil
}

// Manifests is the servers manifest as the API applies it (servers.Service).
type Manifests interface {
	Manifest(ctx context.Context) (servers.Manifest, error)
	Validate(m servers.Manifest) error
	Apply(ctx context.Context, m servers.Manifest, opt servers.ApplyOptions) ([]servers.Change, error)
}

// WithManifests lets the service export, check and apply the servers manifest; opt is how the
// hub applies one (the credential checks and the clouds), By set per call.
func (s *ServerServer) WithManifests(m Manifests, opt servers.ApplyOptions) *ServerServer {
	s.manifests, s.applyOptions = m, opt
	return s
}

func (s *ServerServer) manifestsOrRefuse(ctx context.Context) (authz.Principal, error) {
	p, err := s.access.Require(ctx, authz.ServersManage, authz.Target{})
	if err != nil {
		return p, err
	}
	if s.manifests == nil {
		return p, connect.NewError(connect.CodeUnimplemented, errors.New("this hub applies no servers manifest over the API"))
	}
	return p, nil
}

// ExportServers is the stored manifest: servers:manage.
func (s *ServerServer) ExportServers(ctx context.Context, _ *connect.Request[hubv1.ExportServersRequest]) (*connect.Response[hubv1.ExportServersResponse], error) {
	if _, err := s.manifestsOrRefuse(ctx); err != nil {
		return nil, err
	}
	m, err := s.manifests.Manifest(ctx)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	b, err := m.Marshal()
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	return connect.NewResponse(&hubv1.ExportServersResponse{Manifest: string(b)}), nil
}

// parse reads and validates a manifest from a request: invalid_argument names every problem.
func (s *ServerServer) parse(text string) (servers.Manifest, error) {
	m, err := servers.ParseManifest([]byte(text))
	if err == nil {
		err = s.manifests.Validate(m)
	}
	if err != nil {
		return servers.Manifest{}, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return m, nil
}

// CheckServers validates a manifest against this hub and changes nothing: servers:manage.
func (s *ServerServer) CheckServers(ctx context.Context, req *connect.Request[hubv1.CheckServersRequest]) (*connect.Response[hubv1.CheckServersResponse], error) {
	if _, err := s.manifestsOrRefuse(ctx); err != nil {
		return nil, err
	}
	m, err := s.parse(req.Msg.GetManifest())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.CheckServersResponse{Summary: servers.Summary(m)}), nil
}

// ApplyServers applies a manifest, or with dry_run says what would change: servers:manage, and
// never what only the host may change (servers.HostOnly).
func (s *ServerServer) ApplyServers(ctx context.Context, req *connect.Request[hubv1.ApplyServersRequest]) (*connect.Response[hubv1.ApplyServersResponse], error) {
	p, err := s.manifestsOrRefuse(ctx)
	if err != nil {
		return nil, err
	}
	want, err := s.parse(req.Msg.GetManifest())
	if err != nil {
		return nil, err
	}
	current, err := s.manifests.Manifest(ctx)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	if host := servers.HostOnly(current, want); len(host) > 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"only the hub's host changes these, with gravel-hub servers apply (they name files on the host, or where their secrets go): %s", strings.Join(host, "; ")))
	}
	opt := s.applyOptions
	opt.DryRun, opt.By = req.Msg.GetDryRun(), changedBy(ctx, p)
	changes, err := s.manifests.Apply(ctx, want, opt)
	switch {
	case errors.Is(err, servers.ErrInvalidManifest), errors.Is(err, servers.ErrCredentialFile):
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	case err != nil:
		return nil, mapError(ctx, s.logger, err)
	}
	out := &hubv1.ApplyServersResponse{Applied: !opt.DryRun && len(changes) > 0}
	for _, c := range changes {
		out.Changes = append(out.Changes, c.String())
	}
	return connect.NewResponse(out), nil
}

// Eraser erases a player's identity from the stats (stats.Erase over the store).
type Eraser func(ctx context.Context, key store.PlayerKey, by store.Actor) (store.Erasure, error)

// WithEraser lets the service erase identities (EraseIdentity).
func (s *StatsServer) WithEraser(e Eraser) *StatsServer {
	s.erase = e
	return s
}

// EraseIdentity erases a player's identity from the stats: stats:erase, in person only.
func (s *StatsServer) EraseIdentity(ctx context.Context, req *connect.Request[hubv1.EraseIdentityRequest]) (*connect.Response[hubv1.EraseIdentityResponse], error) {
	p, err := s.access.Require(ctx, authz.StatsErase, authz.Target{})
	if err != nil {
		return nil, err
	}
	if _, err := sessionCaller(ctx); err != nil {
		return nil, err
	}
	if s.erase == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("this hub erases no identities over the API"))
	}
	key := store.PlayerKey{Provider: strings.TrimSpace(req.Msg.GetProvider()), Subject: strings.TrimSpace(req.Msg.GetSubject())}
	if key.Provider == "" || key.Subject == "" || key.Provider == store.ErasedProvider {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("provider and subject are required"))
	}
	got, err := s.erase(ctx, key, changedBy(ctx, p))
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	return connect.NewResponse(&hubv1.EraseIdentityResponse{MatchRows: int32(got.MatchRows), MonthlyRows: int32(got.MonthlyRows), //nolint:gosec // row counts
		ParticipationRows: int32(got.ParticipationRows), ParticipationMonthlyRows: int32(got.ParticipationMonthlyRows), ResultRows: int32(got.ResultRows), SessionRows: int32(got.SessionRows), ActivityMonthlyRows: int32(got.ActivityMonthlyRows), //nolint:gosec // row counts
		EventRows: int32(got.EventRows), IngestBatches: int32(got.IngestBatches), PseudonymDeleted: got.Pseudonym}), nil //nolint:gosec // row counts
}

// RevokeUserSessions logs a member out everywhere: members:manage.
func (s *IdentityServer) RevokeUserSessions(ctx context.Context, req *connect.Request[hubv1.RevokeUserSessionsRequest]) (*connect.Response[hubv1.RevokeUserSessionsResponse], error) {
	p, err := s.access.Require(ctx, authz.MembersManage, authz.Target{})
	if err != nil {
		return nil, err
	}
	uid, err := uuid.Parse(strings.TrimSpace(req.Msg.GetUserId()))
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("user_id must be a member's id"))
	}
	n, err := s.sess.RevokeFor(ctx, uid, changedBy(ctx, p))
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	return connect.NewResponse(&hubv1.RevokeUserSessionsResponse{Revoked: int32(n)}), nil //nolint:gosec // a member's sessions
}
