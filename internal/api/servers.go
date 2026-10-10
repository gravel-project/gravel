package api

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/apps"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/session"
	"github.com/gravel-project/gravel/internal/store"
)

// ServerReader is what the API reads of the games and servers.
type ServerReader interface {
	Games(ctx context.Context) ([]servers.Game, error)
	Servers(ctx context.Context) ([]servers.Server, error)
	Server(ctx context.Context, id string) (servers.Server, error)
}

// Observer is the monitor's last observation of a server.
type Observer interface {
	Observation(id string) (servers.Observation, bool)
}

// MemberLookup resolves a provider identity to the member it is linked to.
type MemberLookup interface {
	Lookup(ctx context.Context, provider, subject string) (identity.User, error)
}

// ServerServer serves gravel.hub.v1.ServerService.
type ServerServer struct {
	hubv1connect.UnimplementedServerServiceHandler
	servers ServerReader
	obs     Observer
	members MemberLookup
	logger  *slog.Logger
}

// NewServerServer wires the service.
func NewServerServer(srv ServerReader, obs Observer, members MemberLookup, logger *slog.Logger) *ServerServer {
	return &ServerServer{servers: srv, obs: obs, members: members, logger: logger}
}

// ListGames is the enabled games; public.
func (s *ServerServer) ListGames(ctx context.Context, _ *connect.Request[hubv1.ListGamesRequest]) (*connect.Response[hubv1.ListGamesResponse], error) {
	games, err := s.servers.Games(ctx)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	out := &hubv1.ListGamesResponse{}
	for _, g := range games {
		p := &hubv1.Game{Id: g.ID, Name: g.Name, IdentityProvider: g.IdentityProvider, NoPaidPerks: g.NoPaidPerks}
		for _, b := range g.Bands {
			p.Bands = append(p.Bands, &hubv1.Band{Section: b.Section, Key: b.Key, Min: b.Min, Max: b.Max,
				MaxLength: b.MaxLength, Hosts: b.Hosts, UnlessSet: b.UnlessSet})
		}
		out.Games = append(out.Games, p)
	}
	return connect.NewResponse(out), nil
}

// ListServers is every live server with its last observation; public.
func (s *ServerServer) ListServers(ctx context.Context, _ *connect.Request[hubv1.ListServersRequest]) (*connect.Response[hubv1.ListServersResponse], error) {
	all, err := s.servers.Servers(ctx)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	out := &hubv1.ListServersResponse{}
	for _, srv := range all {
		out.Servers = append(out.Servers, s.server(srv))
	}
	return connect.NewResponse(out), nil
}

// GetServerStatus is one server; public.
func (s *ServerServer) GetServerStatus(ctx context.Context, req *connect.Request[hubv1.GetServerStatusRequest]) (*connect.Response[hubv1.GetServerStatusResponse], error) {
	srv, err := s.lookup(ctx, req.Msg.GetServerId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.GetServerStatusResponse{Server: s.server(srv)}), nil
}

// ListServerPlayers is who was on at the last poll; a member, the owner or servers:read.
func (s *ServerServer) ListServerPlayers(ctx context.Context, req *connect.Request[hubv1.ListServerPlayersRequest]) (*connect.Response[hubv1.ListServerPlayersResponse], error) {
	if err := requireMemberOrScope(ctx, apps.ScopeServersRead); err != nil {
		return nil, err
	}
	srv, err := s.lookup(ctx, req.Msg.GetServerId())
	if err != nil {
		return nil, err
	}
	out := &hubv1.ListServerPlayersResponse{}
	o, ok := s.obs.Observation(srv.ID)
	if !ok || o.ObservedAt.IsZero() {
		return connect.NewResponse(out), nil
	}
	out.ObservedAt = timestamppb.New(o.ObservedAt)
	for _, p := range o.Players {
		sp := &hubv1.ServerPlayer{
			Name: p.Name, Provider: p.Identity.Provider, Subject: p.Identity.Subject, Team: p.Team,
			Kills: int32(p.Kills), Deaths: int32(p.Deaths), PingMs: int32(p.PingMs), //nolint:gosec // game counters
		}
		if p.Identity.Subject != "" {
			u, err := s.members.Lookup(ctx, p.Identity.Provider, p.Identity.Subject)
			switch {
			case err == nil:
				sp.UserId = u.ID.String()
			case !errors.Is(err, store.ErrNotFound):
				return nil, mapError(ctx, s.logger, err)
			}
		}
		out.Players = append(out.Players, sp)
	}
	return connect.NewResponse(out), nil
}

func (s *ServerServer) lookup(ctx context.Context, id string) (servers.Server, error) {
	if strings.TrimSpace(id) == "" {
		return servers.Server{}, connect.NewError(connect.CodeInvalidArgument, errors.New("server_id is required"))
	}
	srv, err := s.servers.Server(ctx, id)
	if err != nil {
		return servers.Server{}, mapError(ctx, s.logger, err)
	}
	return srv, nil
}

// server is the public view: never the endpoint or the credential file.
func (s *ServerServer) server(srv servers.Server) *hubv1.Server {
	p := &hubv1.Server{Id: srv.ID, Name: srv.Name, GameId: srv.Game, Location: srv.Location, Trust: srv.Trust}
	if sd := srv.Seeding; sd != nil {
		p.Seeding = &hubv1.Seeding{
			Threshold: int32(sd.Threshold), //nolint:gosec // bounded by validation
			Hours:     sd.Hours, Quiet: sd.Quiet, Timezone: sd.Timezone,
			Cooldown: durationpb.New(time.Duration(sd.Cooldown)),
		}
	}
	st := &hubv1.ServerStatus{State: servers.StateUnknown, RotationIndex: -1, NextRotationIndex: -1}
	if o, ok := s.obs.Observation(srv.ID); ok {
		st.State, st.Reachable, st.Build, st.Capabilities = o.State, o.Reachable, o.Build, o.Capabilities
		if !o.ObservedAt.IsZero() {
			x := o.Status
			st.ObservedAt = timestamppb.New(o.ObservedAt)
			st.Players, st.MaxPlayers = int32(x.Players), int32(x.MaxPlayers) //nolint:gosec // slot counts
			st.Map, st.Experiences, st.Lighting = x.Map, x.Experiences, x.Lighting
			st.ScoreTick = int32(x.ScoreTick)                                                           //nolint:gosec // a small period
			st.RotationIndex, st.NextRotationIndex = int32(x.RotationIndex), int32(x.NextRotationIndex) //nolint:gosec // rotation positions
			for _, t := range x.Teams {
				st.Teams = append(st.Teams, &hubv1.TeamScore{Name: t.Name, Color: t.Color, Score: t.Score})
			}
		}
	}
	p.Status = st
	return p
}

// requireMemberOrScope admits a logged-in member (the owner is one) or an app whose token carries
// the scope. CodeUnauthenticated without any credential, CodePermissionDenied for an app without
// the scope.
func requireMemberOrScope(ctx context.Context, scope string) error {
	if a, ok := apps.FromContext(ctx); ok {
		if a.Has(scope) {
			return nil
		}
		return connect.NewError(connect.CodePermissionDenied, errors.New("the app lacks the scope "+scope))
	}
	if _, ok := session.FromContext(ctx); ok {
		return nil
	}
	return connect.NewError(connect.CodeUnauthenticated, errors.New("log in, or call as an app with "+scope))
}
