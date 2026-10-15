package api

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/gravel-project/gravel/drivers"
	"github.com/gravel-project/gravel/drivers/cloudvm"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/authz"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/spend"
)

// HostServer serves gravel.hub.v1.ServerHostService (ADR-0003): reads with servers:read (every
// member holds it); park, unpark, reboot and the expectation with servers:operate; resize, unpin, snapshot
// and destroy with servers:provision (ADR-0027).
type HostServer struct {
	hubv1connect.UnimplementedServerHostServiceHandler
	hosts  *servers.Hosts
	access *Access
	logger *slog.Logger
	spend  *spend.Service // the cloud spend (ADR-0034); nil: none
}

// NewHostServer wires the service.
func NewHostServer(hosts *servers.Hosts, access *Access, logger *slog.Logger) *HostServer {
	return &HostServer{hosts: hosts, access: access, logger: logger}
}

// GetServerHost is a server's host.
func (s *HostServer) GetServerHost(ctx context.Context, req *connect.Request[hubv1.GetServerHostRequest]) (*connect.Response[hubv1.GetServerHostResponse], error) {
	if _, err := s.access.Require(ctx, authz.ServersRead, authz.Target{ServerID: req.Msg.GetServerId()}); err != nil {
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
	if _, err := s.access.Require(ctx, authz.ServersRead, authz.Target{ServerID: req.Msg.GetServerId()}); err != nil {
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
	if _, err := s.access.RequireFor(ctx, authz.ServersOperate, authz.Target{ServerID: m.GetServerId()}, m.GetOnBehalfOf()); err != nil {
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
	done, err := s.hosts.SetDesired(ctx, who, m.GetServerId(), desired, m.GetReason())
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	h, err := s.host(ctx, m.GetServerId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.SetServerHostDesiredResponse{Host: h, AuditId: done.AuditID, EventId: done.EventID}), nil
}

// RebootServerHost stops a running host's instance and starts it again (gravel#202):
// servers:operate, like park and unpark.
func (s *HostServer) RebootServerHost(ctx context.Context, req *connect.Request[hubv1.RebootServerHostRequest]) (*connect.Response[hubv1.RebootServerHostResponse], error) {
	m := req.Msg
	if _, err := s.access.RequireFor(ctx, authz.ServersOperate, authz.Target{ServerID: m.GetServerId()}, m.GetOnBehalfOf()); err != nil {
		return nil, err
	}
	var ob *drivers.Identity
	if o := m.GetOnBehalfOf(); o != nil {
		ob = &drivers.Identity{Provider: o.GetProvider(), Subject: o.GetSubject()}
	}
	who, err := actor(ctx, ob)
	if err != nil {
		return nil, err
	}
	done, err := s.hosts.Reboot(ctx, who, m.GetServerId(), m.GetForce(), m.GetReason())
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	h, err := s.host(ctx, m.GetServerId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.RebootServerHostResponse{Host: h, AuditId: done.AuditID, EventId: done.EventID}), nil
}

// SetServerHostExpectation says how many players a host should expect until when (#172).
func (s *HostServer) SetServerHostExpectation(ctx context.Context, req *connect.Request[hubv1.SetServerHostExpectationRequest]) (*connect.Response[hubv1.SetServerHostExpectationResponse], error) {
	m := req.Msg
	if _, err := s.access.RequireFor(ctx, authz.ServersOperate, authz.Target{ServerID: m.GetServerId()}, m.GetOnBehalfOf()); err != nil {
		return nil, err
	}
	var until time.Time
	switch {
	case m.GetPlayers() < 0:
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("players is 0 (clear) or more"))
	case m.GetPlayers() > 0 && !m.GetUntil().IsValid():
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("until is required with players"))
	case m.GetPlayers() > 0:
		until = m.GetUntil().AsTime()
	}
	var ob *drivers.Identity
	if o := m.GetOnBehalfOf(); o != nil {
		ob = &drivers.Identity{Provider: o.GetProvider(), Subject: o.GetSubject()}
	}
	who, err := actor(ctx, ob)
	if err != nil {
		return nil, err
	}
	done, _, err := s.hosts.SetExpectation(ctx, who, m.GetServerId(), int(m.GetPlayers()), until, m.GetReason())
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	h, err := s.host(ctx, m.GetServerId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.SetServerHostExpectationResponse{Host: h, AuditId: done.AuditID, EventId: done.EventID}), nil
}

// ResizeServerHost moves a running host to another size.
func (s *HostServer) ResizeServerHost(ctx context.Context, req *connect.Request[hubv1.ResizeServerHostRequest]) (*connect.Response[hubv1.ResizeServerHostResponse], error) {
	m := req.Msg
	who, err := s.provisioner(ctx, m.GetServerId())
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(m.GetSize()) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("size is required"))
	}
	done, err := s.hosts.Resize(ctx, who, m.GetServerId(), m.GetSize(), m.GetForce(), m.GetPin(), m.GetReason())
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	h, err := s.host(ctx, m.GetServerId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.ResizeServerHostResponse{Host: h, AuditId: done.AuditID, EventId: done.EventID}), nil
}

// UnpinServerHostSize lets the scaling policy decide a host's size again.
func (s *HostServer) UnpinServerHostSize(ctx context.Context, req *connect.Request[hubv1.UnpinServerHostSizeRequest]) (*connect.Response[hubv1.UnpinServerHostSizeResponse], error) {
	m := req.Msg
	who, err := s.provisioner(ctx, m.GetServerId())
	if err != nil {
		return nil, err
	}
	done, err := s.hosts.Unpin(ctx, who, m.GetServerId(), m.GetReason())
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	h, err := s.host(ctx, m.GetServerId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.UnpinServerHostSizeResponse{Host: h, AuditId: done.AuditID, EventId: done.EventID}), nil
}

// SnapshotServerHostVolume copies a parked host's data volume into a snapshot.
func (s *HostServer) SnapshotServerHostVolume(ctx context.Context, req *connect.Request[hubv1.SnapshotServerHostVolumeRequest]) (*connect.Response[hubv1.SnapshotServerHostVolumeResponse], error) {
	m := req.Msg
	who, err := s.provisioner(ctx, m.GetServerId())
	if err != nil {
		return nil, err
	}
	done, snap, err := s.hosts.SnapshotVolume(ctx, who, m.GetServerId(), strings.TrimSpace(m.GetName()), m.GetReason())
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	h, err := s.host(ctx, m.GetServerId())
	if err != nil {
		return nil, err
	}
	out := &hubv1.VolumeSnapshot{Id: snap.ID, Name: snap.Name, Region: snap.Region, SizeGib: snap.SizeGiB, MinVolumeGib: int32(snap.MinVolumeGiB)}
	if !snap.CreatedAt.IsZero() {
		out.CreatedAt = timestamppb.New(snap.CreatedAt)
	}
	return connect.NewResponse(&hubv1.SnapshotServerHostVolumeResponse{Host: h, AuditId: done.AuditID, EventId: done.EventID, Snapshot: out}), nil
}

// DestroyServerHost removes a parked host's remains.
func (s *HostServer) DestroyServerHost(ctx context.Context, req *connect.Request[hubv1.DestroyServerHostRequest]) (*connect.Response[hubv1.DestroyServerHostResponse], error) {
	m := req.Msg
	who, err := s.provisioner(ctx, m.GetServerId())
	if err != nil {
		return nil, err
	}
	done, err := s.hosts.Destroy(ctx, who, m.GetServerId(), m.GetDeleteVolume(), m.GetReleaseAddress(), m.GetReason())
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	h, err := s.host(ctx, m.GetServerId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.DestroyServerHostResponse{Host: h, AuditId: done.AuditID, EventId: done.EventID}), nil
}

// provisioner admits a caller holding servers:provision (the owner in person; no app may hold it
// yet) and returns them as the actor.
func (s *HostServer) provisioner(ctx context.Context, serverID string) (servers.Actor, error) {
	if _, err := s.access.Require(ctx, authz.ServersProvision, authz.Target{ServerID: serverID}); err != nil {
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
		ReadySeconds: int32(v.ReadySeconds), ParkAfterSeconds: int64(v.ParkAfter.Seconds()), Scaling: v.Scaling != nil,
		TierPlayers: int32(v.TierPlayers), PinnedSize: v.PinnedSize, PendingSize: v.PendingSize, PendingReason: v.PendingReason,
	}
	for _, r := range v.Ladder {
		h.Ladder = append(h.Ladder, &hubv1.HostTier{Size: r.Size, Players: int32(r.Players), PriceHourlyUsd: r.PriceHourly})
	}
	if !v.PendingSince.IsZero() {
		h.PendingSince = timestamppb.New(v.PendingSince)
	}
	if !v.LastResizeAt.IsZero() {
		h.LastResizeAt = timestamppb.New(v.LastResizeAt)
	}
	if v.ExpectedPlayers > 0 {
		h.ExpectedPlayers, h.ExpectedUntil, h.ExpectedReason = int32(v.ExpectedPlayers), timestamppb.New(v.ExpectedUntil), v.ExpectedReason //nolint:gosec // a player count
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

// WithSpend serves GetSpend from the cloud spend (ADR-0034); without it GetSpend is unimplemented.
func (s *HostServer) WithSpend(sp *spend.Service) *HostServer {
	s.spend = sp
	return s
}

// GetSpend is a month's cloud spend (ADR-0034): servers:provision held everywhere, since the
// spend is the whole hub's and a game-scoped grant sees one game's servers only.
func (s *HostServer) GetSpend(ctx context.Context, req *connect.Request[hubv1.GetSpendRequest]) (*connect.Response[hubv1.GetSpendResponse], error) {
	if _, err := s.access.Require(ctx, authz.ServersProvision, authz.Target{}); err != nil {
		return nil, err
	}
	if s.spend == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("this hub keeps no cloud spend"))
	}
	r, err := s.spend.Report(ctx, req.Msg.GetMonth())
	if errors.Is(err, spend.ErrInvalidMonth) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("month is YYYY-MM"))
	}
	if err != nil {
		return nil, mapError(ctx, s.logger, err)
	}
	out := &hubv1.GetSpendResponse{Month: r.Month.Format("2006-01"), Timezone: r.Zone, Current: r.Current, Billed: r.Billed, BilledUsd: r.BilledUSD,
		EstimatedUsd: r.EstimatedUSD, ProjectedUsd: r.ProjectedUSD, ProjectedFrom: r.ProjectedFrom,
		Available: len(r.Locations) > 0 || r.Billed || r.EstimatedUSD > 0}
	if !r.BilledAsOf.IsZero() {
		out.BilledAsOf = timestamppb.New(r.BilledAsOf)
	}
	for _, l := range r.Locations {
		out.Locations = append(out.Locations, &hubv1.SpendLocation{Id: l.ID, Provider: l.Provider, Billed: l.Billed, Reason: l.Reason, ReadAt: timestamppb.New(l.ReadAt)})
	}
	for _, d := range r.Days {
		out.Days = append(out.Days, &hubv1.SpendDay{Date: d.Date.Format(time.DateOnly), BilledUsd: d.BilledUSD, HasBilled: d.HasBilled, EstimatedUsd: d.EstimatedUSD})
	}
	for _, c := range r.Causes {
		out.Causes = append(out.Causes, &hubv1.SpendCause{ServerId: c.ServerID, Name: c.Name, Kinds: c.Kinds, BilledUsd: c.BilledUSD, EstimatedUsd: c.EstimatedUSD})
	}
	return connect.NewResponse(out), nil
}
