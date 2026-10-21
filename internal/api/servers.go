package api

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/gravel-project/gravel/drivers"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/apps"
	"github.com/gravel-project/gravel/internal/authz"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/stats"
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

// SharedPasswords is a server's join password when its manifest shares it (vm.share_password,
// #180); ok is false otherwise, and while the server is leased.
type SharedPasswords interface {
	SharedJoinPassword(srv servers.Server) (password string, ok bool)
}

// FleetReader is every live server with its health (servers.Fleet).
type FleetReader interface {
	Snapshot(ctx context.Context) ([]servers.FleetServer, error)
}

// AlarmReader is the open alarms over a fleet snapshot: the servers' and the hub's own.
type AlarmReader interface {
	Alarms(ctx context.Context, snap []servers.FleetServer) ([]servers.Alarm, error)
}

// ServerServer serves gravel.hub.v1.ServerService.
type ServerServer struct {
	hubv1connect.UnimplementedServerServiceHandler
	servers   ServerReader
	obs       Observer
	members   MemberLookup
	passwords SharedPasswords
	fleet     FleetReader
	alarms    AlarmReader
	// manifests applies the servers manifest over the API (#195); nil: ExportServers and the rest
	// answer unimplemented.
	manifests    Manifests
	applyOptions servers.ApplyOptions
	access       *Access
	logger       *slog.Logger
	now          func() time.Time
}

// NewServerServer wires the service.
func NewServerServer(srv ServerReader, obs Observer, members MemberLookup, access *Access, logger *slog.Logger) *ServerServer {
	return &ServerServer{servers: srv, obs: obs, members: members, access: access, logger: logger, now: time.Now}
}

// WithFleet gives the service the fleet's health and the alarms for GetFleetSummary.
func (s *ServerServer) WithFleet(f FleetReader, a AlarmReader) *ServerServer {
	s.fleet, s.alarms = f, a
	return s
}

// GetFleetSummary is the fleet's health, totals and open alarms; servers:read, scoped by game.
func (s *ServerServer) GetFleetSummary(ctx context.Context, _ *connect.Request[hubv1.GetFleetSummaryRequest]) (*connect.Response[hubv1.GetFleetSummaryResponse], error) {
	p, err := s.access.RequireAny(ctx, authz.ServersRead)
	if err != nil {
		return nil, err
	}
	if s.fleet == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("the fleet summary is not available on this hub"))
	}
	snap, err := s.fleet.Snapshot(ctx)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	var alarms []servers.Alarm
	if s.alarms != nil {
		if alarms, err = s.alarms.Alarms(ctx, snap); err != nil {
			return nil, mapError(ctx, s.logger, err)
		}
	}
	// A caller holding servers:read for some games sees those games' servers and their alarms;
	// the hub's own alarms are for whoever reads the events log everywhere.
	visible := map[string]bool{}
	var mine []servers.FleetServer
	for _, fs := range snap {
		if p.Has(authz.ServersRead, authz.Target{ServerID: fs.Server.ID, GameID: fs.Server.Game}) {
			mine = append(mine, fs)
			visible[fs.Server.ID] = true
		}
	}
	hubAlarms := p.Has(authz.EventsRead, authz.Target{})
	out := &hubv1.GetFleetSummaryResponse{AsOf: timestamppb.New(s.now())}
	t := servers.Totals(mine)
	out.Totals = &hubv1.FleetTotals{Servers: int32(t.Servers), Up: int32(t.Up), Degraded: int32(t.Degraded), Down: int32(t.Down), //nolint:gosec // counts of servers
		Idle: int32(t.Idle), Unknown: int32(t.Unknown), Players: int32(t.Players), Capacity: int32(t.Capacity)} //nolint:gosec // counts of servers and slots
	slices.SortStableFunc(alarms, func(a, b servers.Alarm) int {
		return cmp.Or(cmp.Compare(alarmRank(a.Severity), alarmRank(b.Severity)), a.Since.Compare(b.Since), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.ServerID, b.ServerID))
	})
	for _, a := range alarms {
		if (a.ServerID == "" && !hubAlarms) || (a.ServerID != "" && !visible[a.ServerID]) {
			continue
		}
		fa := &hubv1.FleetAlarm{Kind: a.Kind, Severity: a.Severity, ServerId: a.ServerID, Detail: a.Detail}
		if !a.Since.IsZero() {
			fa.Since = timestamppb.New(a.Since)
		}
		out.Alarms = append(out.Alarms, fa)
	}
	for _, fs := range mine {
		o := fs.Observation
		row := &hubv1.FleetServer{Id: fs.Server.ID, Name: fs.Server.Name, GameId: fs.Server.Game, GameName: fs.GameName, Trust: fs.Server.Trust,
			Driver: fs.Server.Driver, Health: fs.Health, Reason: fs.Reason, State: o.State, LeaseState: fs.LeaseState,
			PollMs: int32(o.LastPollDuration.Milliseconds()), Misses: int32(o.Misses)} //nolint:gosec // a poll's milliseconds and a count
		if !fs.Since.IsZero() {
			row.HealthSince = timestamppb.New(fs.Since)
		}
		if !o.ObservedAt.IsZero() {
			row.ObservedAt = timestamppb.New(o.ObservedAt)
			row.Players, row.MaxPlayers, row.Map = int32(o.Status.Players), int32(o.Status.MaxPlayers), o.Status.Map //nolint:gosec // slot counts
		}
		if fs.Host != nil {
			row.HostState, row.HostErrorCode = fs.Host.State, fs.Host.ErrorCode
		}
		out.Servers = append(out.Servers, row)
	}
	return connect.NewResponse(out), nil
}

func alarmRank(severity string) int {
	if severity == servers.AlarmCritical {
		return 0
	}
	return 1
}

// WithSharedPasswords lets the service give a shared join password to an app holding servers:read.
func (s *ServerServer) WithSharedPasswords(p SharedPasswords) *ServerServer {
	s.passwords = p
	return s
}

// ListGames is the enabled games; public.
func (s *ServerServer) ListGames(ctx context.Context, _ *connect.Request[hubv1.ListGamesRequest]) (*connect.Response[hubv1.ListGamesResponse], error) {
	games, err := s.servers.Games(ctx)
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	out := &hubv1.ListGamesResponse{}
	for _, g := range games {
		p := &hubv1.Game{Id: g.ID, Name: g.Name, IdentityProvider: g.IdentityProvider, NoPaidPerks: g.NoPaidPerks,
			Layout: &hubv1.GameLayout{Board: g.Layout.Board}, StatExtensions: statExtensionMessages(g.Stats.Extensions),
			Sources: slices.Clone(g.Sources), CombatBoards: stats.CombatBoards(g.Spec), ServerLess: len(g.Drivers) == 0}
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
		out.Servers = append(out.Servers, s.server(ctx, srv))
	}
	return connect.NewResponse(out), nil
}

// GetServerStatus is one server; public.
func (s *ServerServer) GetServerStatus(ctx context.Context, req *connect.Request[hubv1.GetServerStatusRequest]) (*connect.Response[hubv1.GetServerStatusResponse], error) {
	srv, err := s.lookup(ctx, req.Msg.GetServerId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.GetServerStatusResponse{Server: s.server(ctx, srv)}), nil
}

// ListServerPlayers is who was on at the last poll; a member, the owner or servers:read.
func (s *ServerServer) ListServerPlayers(ctx context.Context, req *connect.Request[hubv1.ListServerPlayersRequest]) (*connect.Response[hubv1.ListServerPlayersResponse], error) {
	if _, err := s.access.Require(ctx, authz.ServersRead, authz.Target{ServerID: req.Msg.GetServerId()}); err != nil {
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

// server is the public view: never the endpoint or the credential file. The one exception is a
// shared join password, for an app holding servers:read while the server is up (sharedPassword).
func (s *ServerServer) server(ctx context.Context, srv servers.Server) *hubv1.Server {
	p := &hubv1.Server{Id: srv.ID, Name: srv.Name, GameId: srv.Game, Location: srv.Location, Trust: srv.Trust, Driver: srv.Driver}
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
			st.RotationMap, st.NextRotationMap = x.RotationMap, x.NextRotationMap
			if j := x.Join; j.Method != "" && j.Value != "" {
				st.Join = &hubv1.ServerJoin{Method: j.Method, Value: j.Value, Hint: j.Hint}
				if o.State == servers.StateOK && o.Reachable {
					st.Join.Password = s.sharedPassword(ctx, srv)
				}
			}
			counts, known := teamCounts(o)
			for _, t := range x.Teams {
				ts := &hubv1.TeamScore{Name: t.Name, Color: t.Color, Score: t.Score}
				if known {
					ts.Players = proto.Int32(counts[strings.ToLower(t.TeamKey())])
				}
				st.Teams = append(st.Teams, ts)
			}
		}
	}
	p.Status = st
	return p
}

// sharedPassword is the server's shared join password for an app whose token carries servers:read;
// "" for any other caller (anonymous, a member, a session, the web pages in process) and for a
// server that shares none (ADR-0010 §1: public reads carry no credential).
func (s *ServerServer) sharedPassword(ctx context.Context, srv servers.Server) string {
	if s.passwords == nil {
		return ""
	}
	if a, ok := apps.FromContext(ctx); !ok || !a.Has(apps.ScopeServersRead) {
		return ""
	}
	if _, ok := apps.MemberFromContext(ctx); ok {
		return "" // a member's token an app holds is the member, not the app
	}
	pw, ok := s.passwords.SharedJoinPassword(srv)
	if !ok {
		return ""
	}
	return pw
}

// teamCounts are the players on each team at the last poll, by the team's key in lower case; known
// is false when the hub can't tell: the server's players aren't read, or players are on but none
// carries a team (a game that doesn't say).
func teamCounts(o servers.Observation) (counts map[string]int32, known bool) {
	if !slices.Contains(o.Capabilities, drivers.CapPlayers) {
		return nil, false
	}
	counts = map[string]int32{}
	for _, p := range o.Players {
		if p.Team != "" {
			known = true
			counts[strings.ToLower(p.Team)]++
		}
	}
	return counts, known || len(o.Players) == 0
}

// statExtensionMessages are a game's stat extensions as the API shows them (ADR-0033), with the
// type's default spelled out.
func statExtensionMessages(exts []servers.StatExtension) []*hubv1.StatExtension {
	out := make([]*hubv1.StatExtension, 0, len(exts))
	for _, e := range exts {
		out = append(out, &hubv1.StatExtension{Name: e.Name, Label: e.Label, Type: e.TypeOrDefault(), Unit: e.Unit, Aggregate: e.Aggregate})
	}
	return out
}
