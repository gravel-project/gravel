package api

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/gravel-project/gravel/drivers"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/apps"
	"github.com/gravel-project/gravel/internal/authz"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/store"
)

// LeaseServer serves gravel.hub.v1.ServerLeaseService (#20, ADR-0023).
type LeaseServer struct {
	hubv1connect.UnimplementedServerLeaseServiceHandler
	leases    *servers.Leases
	access    *Access
	logger    *slog.Logger
	heartbeat time.Duration
}

// NewLeaseServer wires the service.
func NewLeaseServer(leases *servers.Leases, access *Access, logger *slog.Logger) *LeaseServer {
	return &LeaseServer{leases: leases, access: access, logger: logger, heartbeat: 30 * time.Second}
}

// WithHeartbeat sets how often a watch says it is alive (tests).
func (s *LeaseServer) WithHeartbeat(d time.Duration) *LeaseServer {
	s.heartbeat = d
	return s
}

// leaser is who may lease: a caller holding servers:lease. An app may name the member a lease is
// for (on_behalf_of), who must be a member of the hub but needn't hold the capability: a PUG's
// players don't. A member's token never leases, and a member in person leases as themselves.
func (s *LeaseServer) leaser(ctx context.Context, onBehalf *hubv1.Actor) (servers.Actor, error) {
	if _, ok := apps.MemberFromContext(ctx); ok {
		return servers.Actor{}, connect.NewError(connect.CodePermissionDenied, errors.New("a member's token cannot lease a server"))
	}
	p, err := s.access.Require(ctx, authz.ServersLease, authz.Target{})
	if err != nil {
		return servers.Actor{}, err
	}
	if p.Kind != authz.App {
		if onBehalf != nil {
			return servers.Actor{}, connect.NewError(connect.CodeInvalidArgument, errors.New("on_behalf_of is an app's; a member acts as themselves"))
		}
		return actor(ctx, nil)
	}
	var ob *drivers.Identity
	if onBehalf != nil {
		if _, err := s.access.members.Lookup(ctx, onBehalf.GetProvider(), onBehalf.GetSubject()); errors.Is(err, store.ErrNotFound) {
			return servers.Actor{}, connect.NewError(connect.CodePermissionDenied, errors.New("on_behalf_of is not a member of this hub"))
		} else if err != nil {
			return servers.Actor{}, mapError(ctx, s.logger, err)
		}
		ob = &drivers.Identity{Provider: onBehalf.GetProvider(), Subject: onBehalf.GetSubject()}
	}
	return actor(ctx, ob)
}

// mayHold admits the lease's own app, or the owner and moderators in person; anyone else is told
// there is no such lease.
func (s *LeaseServer) mayHold(who servers.Actor, lease servers.Lease) error {
	if who.AppID == nil || servers.HeldBy(lease, who) {
		return nil
	}
	return connect.NewError(connect.CodeNotFound, errors.New("no such lease"))
}

// RequestServerLease takes a server for a match.
func (s *LeaseServer) RequestServerLease(ctx context.Context, req *connect.Request[hubv1.RequestServerLeaseRequest]) (*connect.Response[hubv1.RequestServerLeaseResponse], error) {
	m := req.Msg
	who, err := s.leaser(ctx, m.GetOnBehalfOf())
	if err != nil {
		return nil, err
	}
	lease, err := s.leases.Request(ctx, who, servers.LeaseRequest{ServerID: m.GetServerId(), Pool: m.GetPool(), HolderRef: m.GetHolderRef(),
		ExpectedPlayers: int(m.GetExpectedPlayers()), Settings: m.GetSettings(), Plugins: m.GetPlugins(), MatchConfig: m.GetMatchConfig(),
		Reason: m.GetReason()})
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	return connect.NewResponse(&hubv1.RequestServerLeaseResponse{Lease: leaseMessage(lease),
		Join: &hubv1.LeaseJoin{Address: lease.Address, Password: lease.Password, Connect: lease.Connect}}), nil
}

// GetServerLease is a lease.
func (s *LeaseServer) GetServerLease(ctx context.Context, req *connect.Request[hubv1.GetServerLeaseRequest]) (*connect.Response[hubv1.GetServerLeaseResponse], error) {
	lease, err := s.get(ctx, req.Msg.GetLeaseId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.GetServerLeaseResponse{Lease: leaseMessage(lease)}), nil
}

func (s *LeaseServer) get(ctx context.Context, rawID string) (servers.Lease, error) {
	who, err := s.leaser(ctx, nil)
	if err != nil {
		return servers.Lease{}, err
	}
	id, err := uuid.Parse(rawID)
	if err != nil {
		return servers.Lease{}, connect.NewError(connect.CodeInvalidArgument, errors.New("lease_id is not a lease's id"))
	}
	lease, err := s.leases.Get(ctx, id)
	if err != nil {
		return servers.Lease{}, s.mapError(ctx, err)
	}
	if err := s.mayHold(who, lease); err != nil {
		return servers.Lease{}, err
	}
	return lease, nil
}

// ListServerLeases is the leases, newest first; an app sees its own.
func (s *LeaseServer) ListServerLeases(ctx context.Context, req *connect.Request[hubv1.ListServerLeasesRequest]) (*connect.Response[hubv1.ListServerLeasesResponse], error) {
	who, err := s.leaser(ctx, nil)
	if err != nil {
		return nil, err
	}
	limit := int(req.Msg.GetLimit())
	switch {
	case limit <= 0:
		limit = 50
	case limit > 200:
		limit = 200
	}
	all, err := s.leases.List(ctx, req.Msg.GetServerId(), req.Msg.GetLiveOnly(), limit)
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	out := &hubv1.ListServerLeasesResponse{}
	for _, l := range all {
		if s.mayHold(who, l) == nil {
			out.Leases = append(out.Leases, leaseMessage(l))
		}
	}
	return connect.NewResponse(out), nil
}

// ReleaseServerLease gives a lease back.
func (s *LeaseServer) ReleaseServerLease(ctx context.Context, req *connect.Request[hubv1.ReleaseServerLeaseRequest]) (*connect.Response[hubv1.ReleaseServerLeaseResponse], error) {
	m := req.Msg
	who, err := s.leaser(ctx, m.GetOnBehalfOf())
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(m.GetLeaseId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("lease_id is not a lease's id"))
	}
	lease, err := s.leases.Get(ctx, id)
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	if err := s.mayHold(who, lease); err != nil {
		return nil, err
	}
	lease, err = s.leases.Release(ctx, who, id, m.GetReason())
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	return connect.NewResponse(&hubv1.ReleaseServerLeaseResponse{Lease: leaseMessage(lease)}), nil
}

// WatchServerLease streams a lease as it changes, until it is over.
func (s *LeaseServer) WatchServerLease(ctx context.Context, req *connect.Request[hubv1.WatchServerLeaseRequest], stream *connect.ServerStream[hubv1.WatchServerLeaseResponse]) error {
	lease, err := s.get(ctx, req.Msg.GetLeaseId())
	if err != nil {
		return err
	}
	changed, cancel := s.leases.Subscribe(lease.ID)
	defer cancel()
	beat := time.NewTicker(s.heartbeat)
	defer beat.Stop()
	for {
		// Read again after subscribing, so a change between the two isn't missed.
		if lease, err = s.leases.Get(ctx, lease.ID); err != nil {
			return s.mapError(ctx, err)
		}
		if err := stream.Send(&hubv1.WatchServerLeaseResponse{Lease: leaseMessage(lease)}); err != nil {
			return err
		}
		if !lease.Live() {
			return nil
		}
	wait:
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-changed:
				break wait
			case <-beat.C:
				if err := stream.Send(&hubv1.WatchServerLeaseResponse{}); err != nil {
					return err
				}
			}
		}
	}
}

func leaseMessage(l servers.Lease) *hubv1.ServerLease {
	m := &hubv1.ServerLease{Id: l.ID.String(), ServerId: l.ServerID, Pool: l.Pool, HolderKind: l.HolderKind, Holder: l.Holder, HolderRef: l.HolderRef,
		State: l.State, Reason: l.Reason, Settings: l.Settings, Plugins: l.Plugins, PluginState: l.PluginState, PluginReason: l.PluginReason,
		MatchLoaded: l.MatchLoaded, ExpectedPlayers: int32(l.ExpectedPlayers), CreatedAt: timestamppb.New(l.CreatedAt), //nolint:gosec // a player count
		MaxUntil: timestamppb.New(l.MaxUntil), Address: l.Address}
	for _, t := range []struct {
		dst **timestamppb.Timestamp
		src *time.Time
	}{{&m.ReadyAt, l.ReadyAt}, {&m.EndedAt, l.EndedAt}, {&m.ReleasedAt, l.ReleasedAt}} {
		if t.src != nil {
			*t.dst = timestamppb.New(*t.src)
		}
	}
	return m
}

func (s *LeaseServer) mapError(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, servers.ErrLeasesOff):
		return connect.NewError(connect.CodeUnimplemented, err)
	case errors.Is(err, servers.ErrInvalidLease):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, servers.ErrNoSuchLease):
		return connect.NewError(connect.CodeNotFound, errors.New("no such lease"))
	case errors.Is(err, servers.ErrNoFreeServer), errors.Is(err, servers.ErrServerBusy), errors.Is(err, servers.ErrLeaseFinished):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, servers.ErrNotLeasable):
		return connect.NewError(connect.CodeNotFound, err)
	}
	return mapError(ctx, s.logger, err)
}
