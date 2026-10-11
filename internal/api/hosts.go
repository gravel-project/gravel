package api

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/gravel-project/gravel/drivers"
	"github.com/gravel-project/gravel/drivers/cloudvm"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/apps"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/servers"
)

// HostServer serves gravel.hub.v1.ServerHostService (ADR-0003): reads for members and apps with
// servers:read; park and unpark for the owner, a moderator or an app with servers:moderate; resize
// and destroy for the owner only.
type HostServer struct {
	hubv1connect.UnimplementedServerHostServiceHandler
	hosts  *servers.Hosts
	mod    *ModerationServer // whose moderator check park and unpark share
	org    *org.Service
	logger *slog.Logger
}

// NewHostServer wires the service.
func NewHostServer(hosts *servers.Hosts, mod *ModerationServer, orgSvc *org.Service, logger *slog.Logger) *HostServer {
	return &HostServer{hosts: hosts, mod: mod, org: orgSvc, logger: logger}
}

// GetServerHost is a server's host.
func (s *HostServer) GetServerHost(ctx context.Context, req *connect.Request[hubv1.GetServerHostRequest]) (*connect.Response[hubv1.GetServerHostResponse], error) {
	if err := requireMemberOrScope(ctx, apps.ScopeServersRead); err != nil {
		return nil, err
	}
	h, err := s.host(ctx, req.Msg.GetServerId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.GetServerHostResponse{Host: h}), nil
}

// ListServerHostEvents is a host's transitions, newest first.
func (s *HostServer) ListServerHostEvents(ctx context.Context, req *connect.Request[hubv1.ListServerHostEventsRequest]) (*connect.Response[hubv1.ListServerHostEventsResponse], error) {
	if err := requireMemberOrScope(ctx, apps.ScopeServersRead); err != nil {
		return nil, err
	}
	m := req.Msg
	before, err := decodeHostEventToken(m.GetPageToken())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if _, err := s.host(ctx, m.GetServerId()); err != nil {
		return nil, err
	}
	ev, more, err := s.hosts.Events(ctx, m.GetServerId(), before, int(m.GetPageSize()))
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	out := &hubv1.ListServerHostEventsResponse{}
	for _, e := range ev {
		x := &hubv1.HostEvent{Id: e.ID, At: timestamppb.New(e.At), FromState: e.FromState, ToState: e.ToState, Step: e.Step, Detail: e.Detail, Size: e.Size}
		if e.AuditID != nil {
			x.AuditId = *e.AuditID
		}
		out.Events = append(out.Events, x)
	}
	if more && len(ev) > 0 {
		out.NextPageToken = encodeHostEventToken(ev[len(ev)-1].ID)
	}
	return connect.NewResponse(out), nil
}

// SetServerHostDesired parks or unparks a host.
func (s *HostServer) SetServerHostDesired(ctx context.Context, req *connect.Request[hubv1.SetServerHostDesiredRequest]) (*connect.Response[hubv1.SetServerHostDesiredResponse], error) {
	m := req.Msg
	if err := s.mod.requireModerator(ctx, m.GetOnBehalfOf()); err != nil {
		return nil, err
	}
	desired := m.GetDesired()
	if desired != cloudvm.DesiredRunning && desired != cloudvm.DesiredParked {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New(`desired is "running" or "parked"`))
	}
	var ob *drivers.Identity
	if o := m.GetOnBehalfOf(); o != nil {
		ob = &drivers.Identity{Provider: o.GetProvider(), Subject: o.GetSubject()}
	}
	who, err := actor(ctx, ob)
	if err != nil {
		return nil, err
	}
	id, err := s.hosts.SetDesired(ctx, who, m.GetServerId(), desired, m.GetReason())
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	h, err := s.host(ctx, m.GetServerId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.SetServerHostDesiredResponse{Host: h, AuditId: id}), nil
}

// ResizeServerHost moves a running host to another size.
func (s *HostServer) ResizeServerHost(ctx context.Context, req *connect.Request[hubv1.ResizeServerHostRequest]) (*connect.Response[hubv1.ResizeServerHostResponse], error) {
	m := req.Msg
	who, err := s.owner(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(m.GetSize()) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("size is required"))
	}
	id, err := s.hosts.Resize(ctx, who, m.GetServerId(), m.GetSize(), m.GetForce(), m.GetReason())
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	h, err := s.host(ctx, m.GetServerId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.ResizeServerHostResponse{Host: h, AuditId: id}), nil
}

// DestroyServerHost removes a parked host's remains.
func (s *HostServer) DestroyServerHost(ctx context.Context, req *connect.Request[hubv1.DestroyServerHostRequest]) (*connect.Response[hubv1.DestroyServerHostResponse], error) {
	m := req.Msg
	who, err := s.owner(ctx)
	if err != nil {
		return nil, err
	}
	id, err := s.hosts.Destroy(ctx, who, m.GetServerId(), m.GetDeleteVolume(), m.GetReleaseAddress(), m.GetReason())
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	h, err := s.host(ctx, m.GetServerId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.DestroyServerHostResponse{Host: h, AuditId: id}), nil
}

// owner admits the owner in person and returns them as the actor.
func (s *HostServer) owner(ctx context.Context) (servers.Actor, error) {
	if _, ok := apps.FromContext(ctx); ok {
		return servers.Actor{}, connect.NewError(connect.CodePermissionDenied, errors.New("the owner only, in person"))
	}
	if err := requireOwner(ctx, s.org, s.logger); err != nil {
		return servers.Actor{}, err
	}
	return actor(ctx, nil)
}

func (s *HostServer) host(ctx context.Context, serverID string) (*hubv1.ServerHost, error) {
	if strings.TrimSpace(serverID) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("server_id is required"))
	}
	v, err := s.hosts.Host(ctx, serverID)
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	return hostMessage(v), nil
}

// hostMessage is a host as the API shows it: no instance id, no instance address, no host key.
func hostMessage(v servers.HostView) *hubv1.ServerHost {
	h := &hubv1.ServerHost{
		ServerId: v.ServerID, Location: v.Location, Provider: v.Provider, Region: v.Region, Desired: v.Desired, State: v.State, Step: v.Step,
		ErrorCode: v.ErrorCode, Attempts: int32(v.Attempts), Size: v.Size, TargetSize: v.TargetSize, DiskGib: int32(v.DiskGiB),
		VolumeGib: int32(v.VolumeGiB), Address: v.Address, AddressReserved: v.AddressReserved, HourlyPriceUsd: v.PriceHourly,
		RunningSecondsMonth: v.RunningSecondsMonth, MonthCostUsd: v.MonthCostUSD(), BootSeconds: int32(v.BootSeconds),
		ReadySeconds: int32(v.ReadySeconds), ParkAfterSeconds: int64(v.ParkAfter.Seconds()),
	}
	if !v.StateSince.IsZero() {
		h.Since = timestamppb.New(v.StateSince)
	}
	if !v.InstanceCreatedAt.IsZero() {
		h.InstanceCreatedAt = timestamppb.New(v.InstanceCreatedAt)
	}
	for _, z := range v.Sizes {
		h.Sizes = append(h.Sizes, &hubv1.HostSize{Slug: z.Slug, Vcpus: int32(z.VCPUs), MemoryMib: int32(z.MemoryMiB), DiskGib: int32(z.DiskGiB),
			PriceHourlyUsd: z.PriceHourly, PriceMonthlyUsd: z.PriceMonthly, DedicatedCpu: z.DedicatedCPU})
	}
	return h
}

func (s *HostServer) mapError(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, servers.ErrNotCloudVM), errors.Is(err, cloudvm.ErrNoSuchHost):
		return connect.NewError(connect.CodeNotFound, errors.New("no such server, or not a cloud-VM server"))
	case errors.Is(err, cloudvm.ErrPlayersOn):
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("players are on the server; pass force to resize anyway"))
	case errors.Is(err, cloudvm.ErrNotRunning), errors.Is(err, cloudvm.ErrNotParked), errors.Is(err, cloudvm.ErrRemoved):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, cloudvm.ErrInvalid), errors.Is(err, cloudvm.ErrInvalidSize), errors.Is(err, servers.ErrInvalidModeration):
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	return mapError(ctx, s.logger, err)
}

// A host-event page token is the base64 of "hostevent|<id>".
func encodeHostEventToken(id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte("hostevent|" + strconv.FormatInt(id, 10)))
}

func decodeHostEventToken(token string) (int64, error) {
	if token == "" {
		return 0, nil
	}
	bad := errors.New("page_token is not one this hub issued")
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, bad
	}
	n, ok := strings.CutPrefix(string(raw), "hostevent|")
	if !ok {
		return 0, bad
	}
	id, err := strconv.ParseInt(n, 10, 64)
	if err != nil || id <= 0 {
		return 0, bad
	}
	return id, nil
}
