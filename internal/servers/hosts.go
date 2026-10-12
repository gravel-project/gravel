package servers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/cloud"
	"github.com/gravel-project/gravel/drivers/cloudvm"
	"github.com/gravel-project/gravel/internal/store"
)

// Host audit actions: what an owner or moderator asked of a cloud-VM server's host.
const (
	ActionHostPark    = "host_park"
	ActionHostUnpark  = "host_unpark"
	ActionHostResize  = "host_resize"
	ActionHostUnpin   = "host_unpin"
	ActionHostDestroy = "host_destroy"
)

// Outcomes of a host request: accepted (the host's state machine carries it out, and its
// transitions name the audit row) or the reason it was refused.
const (
	OutcomeAccepted   = "accepted"
	OutcomeNotRunning = "not_running"
	OutcomePlayersOn  = "players_on"
	OutcomeNotParked  = "not_parked"
	OutcomeRemoved    = "removed"
	OutcomeInvalid    = "invalid"
)

// HostRows is what the hosts need from the database.
type HostRows interface {
	ListManagedServers(ctx context.Context, orgID uuid.UUID, includeRemoved bool) ([]store.ManagedServer, error)
	ListServerHosts(ctx context.Context, orgID uuid.UUID) ([]store.ServerHost, error)
	SaveServerHost(ctx context.Context, h store.ServerHost, now time.Time) error
	InsertServerHostEvent(ctx context.Context, e store.ServerHostEvent) (int64, error)
	ListServerHostEvents(ctx context.Context, orgID uuid.UUID, serverID string, beforeID int64, limit int) ([]store.ServerHostEvent, error)
	RunningSecondsSince(ctx context.Context, orgID uuid.UUID, serverID string, from, now time.Time) (int64, error)
	RunningCostSince(ctx context.Context, orgID uuid.UUID, serverID string, from, now time.Time) (float64, error)
}

// Hosts joins the hub's servers and database to the cloud-VM driver's Manager: it is the
// Manager's source of servers (secrets read from their files) and its store, and the service the
// API asks about hosts through, every request audited.
type Hosts struct {
	svc      *Service
	rows     HostRows
	audit    AuditStore
	orgID    uuid.UUID
	logger   *slog.Logger
	now      func() time.Time
	readFile func(string) ([]byte, error)
	mgr      *cloudvm.Manager
}

// NewHosts builds the glue; Attach gives it the Manager.
func NewHosts(svc *Service, rows HostRows, audit AuditStore, orgID uuid.UUID, logger *slog.Logger) *Hosts {
	return &Hosts{svc: svc, rows: rows, audit: audit, orgID: orgID, logger: logger, now: time.Now, readFile: os.ReadFile}
}

// Attach sets the Manager the requests go to.
func (h *Hosts) Attach(m *cloudvm.Manager) { h.mgr = m }

// firstLine is a secret file's first non-empty line, trimmed.
func (h *Hosts) firstLine(path string) (string, error) {
	b, err := h.readFile(path)
	if err != nil {
		return "", err
	}
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l, nil
		}
	}
	return "", fmt.Errorf("%s is empty", path)
}

// CloudVMServers implements cloudvm.Source: every cloud-VM server, a removed one included (its host
// is parked and stays until destroyed), with its location and secrets read. A live server whose
// secrets cannot be read is left out until they can (logged); a removed one needs only its
// location's.
func (h *Hosts) CloudVMServers(ctx context.Context) ([]cloudvm.ServerSpec, error) {
	rows, err := h.rows.ListManagedServers(ctx, h.orgID, true)
	if err != nil {
		return nil, err
	}
	locs, err := h.svc.Locations(ctx)
	if err != nil {
		return nil, err
	}
	byID := map[string]Location{}
	for _, l := range locs {
		byID[l.ID] = l
	}
	var out []cloudvm.ServerSpec
	for _, r := range rows {
		if r.Driver != cloudvm.Name {
			continue
		}
		srv, err := fromRow(r)
		if err != nil {
			h.logger.Warn("a cloud-VM server's row cannot be read; skipped", "server", r.ID, "error", err.Error())
			continue
		}
		spec, err := h.spec(srv, byID, r.RemovedAt != nil)
		if err != nil {
			h.logger.Warn("a cloud-VM server is left out until this is fixed", "server", r.ID, "error", err.Error())
			continue
		}
		out = append(out, spec)
	}
	return out, nil
}

func (h *Hosts) spec(srv Server, locs map[string]Location, removed bool) (cloudvm.ServerSpec, error) {
	game, ok := h.svc.specs[srv.Game]
	if !ok || game.CloudVM == nil {
		return cloudvm.ServerSpec{}, fmt.Errorf("game %s has no cloudvm section in this hub", srv.Game)
	}
	loc, ok := locs[srv.Location]
	if !ok {
		return cloudvm.ServerSpec{}, fmt.Errorf("location %s is not defined", srv.Location)
	}
	if srv.VM == nil {
		return cloudvm.ServerSpec{}, errors.New("no vm section")
	}
	token, err := h.firstLine(loc.TokenFile)
	if err != nil {
		return cloudvm.ServerSpec{}, fmt.Errorf("location %s: token: %w", loc.ID, err)
	}
	pem, err := h.readFile(loc.SSHKeyFile)
	if err != nil {
		return cloudvm.ServerSpec{}, fmt.Errorf("location %s: ssh key: %w", loc.ID, err)
	}
	signer, err := cloudvm.ParseSSHKey(pem)
	if err != nil {
		return cloudvm.ServerSpec{}, fmt.Errorf("location %s: %w", loc.ID, err)
	}
	s := cloudvm.ServerSpec{
		ID: srv.ID, Name: srv.Name, Game: srv.Game, GameName: game.Name, IdentityProvider: game.IdentityProvider,
		Location: cloudvm.Location{ID: loc.ID, Provider: loc.Provider, Region: loc.Region, Project: loc.Project, Image: loc.Image,
			ExtraSSHKeys: loc.ExtraSSHKeys, SSHFrom: loc.SSHFrom, Token: token, SSHKey: signer},
		Profile: *game.CloudVM, Size: srv.VM.Size, VolumeGiB: srv.VM.VolumeGiB, AddressReserved: srv.VM.Address != AddressEphemeral,
		ParkAfter: time.Duration(srv.VM.ParkAfter), Settings: srv.Settings, Removed: removed,
	}
	if sc := srv.VM.Scaling; sc != nil {
		policy := sc.Policy(game.CloudVM.Ladder)
		s.Scaling = &policy
	}
	read := func(path string) (string, error) {
		if path == "" {
			return "", nil
		}
		v, err := h.firstLine(path)
		if err != nil && removed {
			return "", nil // a removed server is only parked and destroyed
		}
		return v, err
	}
	if s.RCONPassword, err = read(srv.CredentialFile); err != nil {
		return cloudvm.ServerSpec{}, fmt.Errorf("credential: %w", err)
	}
	if s.JoinPassword, err = read(srv.VM.PasswordFile); err != nil {
		return cloudvm.ServerSpec{}, fmt.Errorf("password: %w", err)
	}
	if s.PublisherToken, err = read(srv.VM.PublisherTokenFile); err != nil {
		return cloudvm.ServerSpec{}, fmt.Errorf("publisher token: %w", err)
	}
	if srv.Feed != nil {
		feed, err := read(srv.Feed.TokenFile)
		if err != nil {
			return cloudvm.ServerSpec{}, fmt.Errorf("feed token: %w", err)
		}
		if feed != "" {
			s.FeedURL = cloudvm.FeedURL(srv.Feed.URL, feed)
		}
	}
	return s, nil
}

// Hosts implements cloudvm.HostStore.
func (h *Hosts) Hosts(ctx context.Context) ([]cloudvm.Host, error) {
	rows, err := h.rows.ListServerHosts(ctx, h.orgID)
	if err != nil {
		return nil, err
	}
	out := make([]cloudvm.Host, 0, len(rows))
	for _, r := range rows {
		host, err := hostFromRow(r)
		if err != nil {
			return nil, fmt.Errorf("servers: host %s: %w", r.ServerID, err)
		}
		out = append(out, host)
	}
	return out, nil
}

// SaveHost implements cloudvm.HostStore.
func (h *Hosts) SaveHost(ctx context.Context, host cloudvm.Host) error {
	r, err := hostToRow(h.orgID, host)
	if err != nil {
		return err
	}
	return h.rows.SaveServerHost(ctx, r, h.now().UTC())
}

// AppendEvent implements cloudvm.HostStore.
func (h *Hosts) AppendEvent(ctx context.Context, e cloudvm.Event) error {
	row := store.ServerHostEvent{OrganizationID: h.orgID, ServerID: e.ServerID, At: e.At.UTC(), FromState: e.From, ToState: e.To,
		Step: e.Step, Detail: e.Detail, Size: e.Size}
	if e.PriceHourly > 0 {
		p := e.PriceHourly
		row.PriceHourlyUSD = &p
	}
	if e.AuditID != 0 {
		a := e.AuditID
		row.AuditID = &a
	}
	_, err := h.rows.InsertServerHostEvent(ctx, row)
	return err
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func intPtr(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}

func hostToRow(org uuid.UUID, h cloudvm.Host) (store.ServerHost, error) {
	r := store.ServerHost{OrganizationID: org, ServerID: h.ServerID, LocationID: h.LocationID, Desired: h.Desired, State: h.State,
		Step: h.Step, Size: h.Size, TargetSize: h.TargetSize, DestroyVolume: h.DestroyVolume, ReleaseAddress: h.ReleaseAddress,
		DiskGiB: h.DiskGiB, InstanceID: h.InstanceID, InstanceAddress: h.InstanceAddress, VolumeID: h.VolumeID, Address: h.Address,
		AddressReserved: h.AddressReserved, FirewallID: h.FirewallID, Egress: h.Egress, HostKey: h.HostKey, EnvSHA256: h.EnvSHA256,
		Attempts: h.Attempts, NextAttemptAt: timePtr(h.NextAttemptAt), ErrorCode: h.ErrorCode, LastError: h.LastError,
		StateSince: h.StateSince.UTC(), InstanceCreatedAt: timePtr(h.InstanceCreatedAt), EmptySince: timePtr(h.EmptySince),
		BootSeconds: intPtr(h.BootSeconds), ReadySeconds: intPtr(h.ReadySeconds), PinnedSize: h.PinnedSize,
		PendingSize: h.PendingSize, PendingReason: h.PendingReason, PendingSince: timePtr(h.PendingSince),
		LastResizeAt: timePtr(h.LastResizeAt)}
	if h.RequestAuditID != 0 {
		a := h.RequestAuditID
		r.RequestAuditID = &a
	}
	if h.Operation != nil {
		b, err := json.Marshal(h.Operation)
		if err != nil {
			return store.ServerHost{}, err
		}
		r.Operation = b
	}
	return r, nil
}

func hostFromRow(r store.ServerHost) (cloudvm.Host, error) {
	h := cloudvm.Host{ServerID: r.ServerID, LocationID: r.LocationID,
		Intent: cloudvm.Intent{Desired: r.Desired, TargetSize: r.TargetSize, DestroyVolume: r.DestroyVolume, ReleaseAddress: r.ReleaseAddress,
			PinnedSize: r.PinnedSize},
		State: r.State, Step: r.Step, Size: r.Size, DiskGiB: r.DiskGiB, InstanceID: r.InstanceID, InstanceAddress: r.InstanceAddress,
		VolumeID: r.VolumeID, Address: r.Address, AddressReserved: r.AddressReserved, FirewallID: r.FirewallID, Egress: r.Egress,
		HostKey: r.HostKey, EnvSHA256: r.EnvSHA256, Attempts: r.Attempts, ErrorCode: r.ErrorCode, LastError: r.LastError,
		StateSince: r.StateSince, PendingSize: r.PendingSize, PendingReason: r.PendingReason}
	if r.RequestAuditID != nil {
		h.RequestAuditID = *r.RequestAuditID
	}
	for _, p := range []struct {
		dst *time.Time
		src *time.Time
	}{{&h.NextAttemptAt, r.NextAttemptAt}, {&h.InstanceCreatedAt, r.InstanceCreatedAt}, {&h.EmptySince, r.EmptySince},
		{&h.PendingSince, r.PendingSince}, {&h.LastResizeAt, r.LastResizeAt}} {
		if p.src != nil {
			*p.dst = *p.src
		}
	}
	if r.BootSeconds != nil {
		h.BootSeconds = *r.BootSeconds
	}
	if r.ReadySeconds != nil {
		h.ReadySeconds = *r.ReadySeconds
	}
	if len(r.Operation) > 0 && string(r.Operation) != "null" {
		var op cloud.Operation
		if err := json.Unmarshal(r.Operation, &op); err != nil {
			return cloudvm.Host{}, err
		}
		h.Operation = &op
	}
	return h, nil
}

// HostView is a host as the API shows it: the host, what it costs, and the sizes it may move to.
type HostView struct {
	cloudvm.Host
	Location, Provider, Region string
	VolumeGiB                  int
	ParkAfter                  time.Duration
	PriceHourly                float64
	RunningSecondsMonth        int64
	// MonthCost is the instance's cost this month so far, each run at its own size's price.
	MonthCost float64
	Sizes     []cloud.Size
	// Scaling is the server's policy (nil: none), Ladder its rungs with their prices, and
	// TierPlayers the players the rung the host is on is sized for (0 off the ladder).
	Scaling     *cloudvm.Scaling
	Ladder      []LadderRung
	TierPlayers int
}

// LadderRung is one rung of a host's scaling ladder as the API shows it.
type LadderRung struct {
	cloudvm.Tier
	PriceHourly float64
}

// ErrNotCloudVM is a server that is not a cloud-VM server (or has no host yet).
var ErrNotCloudVM = errors.New("servers: not a cloud-VM server")

// Host is a cloud-VM server's host as the API shows it.
func (h *Hosts) Host(ctx context.Context, serverID string) (HostView, error) {
	if h.mgr == nil {
		return HostView{}, ErrNotCloudVM
	}
	host, ok := h.mgr.Host(serverID)
	if !ok {
		return HostView{}, ErrNotCloudVM
	}
	v := HostView{Host: host, Location: host.LocationID}
	if srv, err := h.svc.Server(ctx, serverID); err == nil && srv.VM != nil {
		v.VolumeGiB, v.ParkAfter = srv.VM.VolumeGiB, time.Duration(srv.VM.ParkAfter)
	}
	if loc, err := h.svc.Location(ctx, host.LocationID); err == nil {
		v.Provider, v.Region = loc.Provider, loc.Region
	}
	if host.Size != "" {
		v.PriceHourly = h.mgr.Price(ctx, serverID, host.Size)
	}
	now := h.now().UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if secs, err := h.rows.RunningSecondsSince(ctx, h.orgID, serverID, month, now); err == nil {
		v.RunningSecondsMonth = secs
	}
	if usd, err := h.rows.RunningCostSince(ctx, h.orgID, serverID, month, now); err == nil {
		v.MonthCost = usd
	}
	if spec, ok := h.mgr.Spec(serverID); ok && spec.Scaling != nil {
		v.Scaling = spec.Scaling
		for _, t := range spec.Scaling.Ladder {
			v.Ladder = append(v.Ladder, LadderRung{Tier: t, PriceHourly: h.mgr.Price(ctx, serverID, t.Size)})
			if t.Size == host.Size {
				v.TierPlayers = t.Players
			}
		}
	}
	if host.State == cloudvm.StateRunning {
		if sizes, err := h.mgr.Sizes(ctx, serverID); err == nil {
			v.Sizes = sizes
		}
	}
	return v, nil
}

// MonthCostUSD is the host's instance cost this month so far (the volume and address are not in
// it), each run at the price of the size it ran at.
func (v HostView) MonthCostUSD() float64 { return v.MonthCost }

// Events are a page of a host's transitions, newest first.
func (h *Hosts) Events(ctx context.Context, serverID string, before int64, limit int) ([]store.ServerHostEvent, bool, error) {
	if limit <= 0 || limit > MaxAuditPage {
		limit = MaxAuditPage
	}
	ev, err := h.rows.ListServerHostEvents(ctx, h.orgID, serverID, before, limit+1)
	if err != nil {
		return nil, false, err
	}
	if len(ev) > limit {
		return ev[:limit], true, nil
	}
	return ev, false, nil
}

// request audits a host request, runs it and records its outcome.
func (h *Hosts) request(ctx context.Context, actor Actor, serverID, action, reason string, detail map[string]any, run func(auditID int64) error) (int64, error) {
	if h.mgr == nil {
		return 0, ErrNotCloudVM
	}
	if _, ok := h.mgr.Host(serverID); !ok {
		return 0, ErrNotCloudVM
	}
	if err := validate(actor, Action{Kind: ActionBroadcast, Message: "-"}); err != nil {
		return 0, err
	}
	if len(reason) > MaxReasonLength {
		return 0, fmt.Errorf("%w: the reason is over %d characters", ErrInvalidModeration, MaxReasonLength)
	}
	d, err := json.Marshal(detail)
	if err != nil {
		return 0, err
	}
	entry := store.AuditEntry{OrganizationID: h.orgID, At: h.now().UTC(), ActorUserID: actor.UserID, ActorAppID: actor.AppID,
		ServerID: serverID, Action: action, Reason: reason, Detail: d, RequestID: actor.RequestID}
	if o := actor.OnBehalfOf; o != nil {
		entry.OnBehalfProvider, entry.OnBehalfSubject = o.Provider, o.Subject
	}
	id, err := h.audit.InsertAuditEntry(ctx, entry)
	if err != nil {
		return 0, err
	}
	callErr := run(id)
	word := OutcomeAccepted
	switch {
	case callErr == nil:
	case errors.Is(callErr, cloudvm.ErrNotRunning):
		word = OutcomeNotRunning
	case errors.Is(callErr, cloudvm.ErrPlayersOn):
		word = OutcomePlayersOn
	case errors.Is(callErr, cloudvm.ErrNotParked):
		word = OutcomeNotParked
	case errors.Is(callErr, cloudvm.ErrRemoved):
		word = OutcomeRemoved
	case errors.Is(callErr, cloudvm.ErrInvalid), errors.Is(callErr, cloudvm.ErrInvalidSize):
		word = OutcomeInvalid
	default:
		word = StateError
	}
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := h.audit.FinishAuditEntry(fctx, h.orgID, id, word, h.now().UTC()); err != nil {
		h.logger.Error("a host request's outcome was not recorded", "audit_id", id, "error", err.Error())
	}
	h.logger.Info("host request", "server", serverID, "action", action, "outcome", word, "audit_id", id, "request_id", actor.RequestID)
	return id, callErr
}

// SetDesired parks or unparks a host.
func (h *Hosts) SetDesired(ctx context.Context, actor Actor, serverID, desired, reason string) (int64, error) {
	action := ActionHostUnpark
	if desired == cloudvm.DesiredParked {
		action = ActionHostPark
	}
	return h.request(ctx, actor, serverID, action, reason, map[string]any{"desired": desired}, func(id int64) error {
		_, err := h.mgr.SetDesired(ctx, serverID, desired, id)
		return err
	})
}

// Resize moves a running host to another size; with pin the size stays (the scaling policy stands
// aside and an unparked host comes back to it).
func (h *Hosts) Resize(ctx context.Context, actor Actor, serverID, size string, force, pin bool, reason string) (int64, error) {
	return h.request(ctx, actor, serverID, ActionHostResize, reason, map[string]any{"size": size, "force": force, "pin": pin}, func(id int64) error {
		_, err := h.mgr.Resize(ctx, serverID, size, force, pin, id)
		return err
	})
}

// Unpin lets the scaling policy decide the host's size again.
func (h *Hosts) Unpin(ctx context.Context, actor Actor, serverID, reason string) (int64, error) {
	return h.request(ctx, actor, serverID, ActionHostUnpin, reason, map[string]any{}, func(id int64) error {
		_, err := h.mgr.Unpin(ctx, serverID, id)
		return err
	})
}

// Destroy removes a parked host's remains.
func (h *Hosts) Destroy(ctx context.Context, actor Actor, serverID string, deleteVolume, releaseAddress bool, reason string) (int64, error) {
	return h.request(ctx, actor, serverID, ActionHostDestroy, reason, map[string]any{"delete_volume": deleteVolume, "release_address": releaseAddress}, func(id int64) error {
		_, err := h.mgr.Destroy(ctx, serverID, deleteVolume, releaseAddress, id)
		return err
	})
}
