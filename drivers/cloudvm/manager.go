package cloudvm

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/crypto/ssh"

	"github.com/gravel-project/gravel/cloud"
	"github.com/gravel-project/gravel/drivers"
	"github.com/gravel-project/gravel/internal/jobs"
)

// Host states (the server_hosts table's words). running and parked are where a host rests; the
// others are transitions, each made of steps the hub records as it goes.
const (
	StateAbsent     = "absent"
	StateCreating   = "creating"
	StateBooting    = "booting"
	StateStarting   = "starting"
	StateRunning    = "running"
	StateParking    = "parking"
	StateParked     = "parked"
	StateResizing   = "resizing"
	StateDestroying = "destroying"
)

// Desired states.
const (
	DesiredRunning = "running"
	DesiredParked  = "parked"
	DesiredAbsent  = "absent"
)

// Errors a request returns.
var (
	ErrNoSuchHost  = errors.New("cloudvm: no cloud-VM server by that id")
	ErrNotRunning  = errors.New("cloudvm: the server's host is not running")
	ErrPlayersOn   = errors.New("cloudvm: players are on the server")
	ErrNotParked   = errors.New("cloudvm: only a parked host can be destroyed; park it or remove the server from the manifest first")
	ErrRemoved     = errors.New("cloudvm: the server was removed from the manifest; add it back to run it")
	ErrInvalidSize = errors.New("cloudvm: the host cannot be resized to that size")
	ErrInvalid     = errors.New("cloudvm: invalid request")
)

// Timings. A tick is one pass of a host's state machine; a step that waits (a boot, the game's
// first download) is looked at again on the next tick.
const (
	TickEvery        = 15 * time.Second
	ReconcileEvery   = 30 * time.Second
	BootTimeout      = 20 * time.Minute
	DriftEvery       = 5 * time.Minute
	EgressEvery      = 10 * time.Minute
	OperationWait    = 2 * time.Minute
	ParkWarning      = 10 * time.Second
	maxStepsPerTick  = 32
	backoffBase      = 30 * time.Second
	backoffMax       = 10 * time.Minute
	gameCallTimeout  = 10 * time.Second
	remoteRunTimeout = time.Minute
)

// Location is a cloud a server's host is made in, with its secrets read.
type Location struct {
	ID, Provider, Region, Project string
	// Image is the OS image; empty is the provider's default (Options.DefaultImages).
	Image        string
	ExtraSSHKeys []string
	SSHFrom      []string
	Token        string
	SSHKey       ssh.Signer
}

// ServerSpec is one cloud-VM server as the hub wants it, its secrets read.
type ServerSpec struct {
	ID, Name, Game string
	// GameName is the game's display name; IdentityProvider who its players are keyed by.
	GameName         string
	IdentityProvider string
	Location         Location
	Profile          Profile
	// Size is the size a host is made at; a manual resize lasts until the host is parked.
	Size            string
	VolumeGiB       int
	AddressReserved bool
	// ParkAfter parks the host after the server has been empty this long; zero never does.
	ParkAfter time.Duration
	Settings  map[string]string
	// The secrets: RCON, the join password and publisher token ("" for none), the feed URL with
	// its token.
	RCONPassword, JoinPassword, PublisherToken, FeedURL string
	// Removed is a server taken out of the manifest: its host is parked and stays until an owner
	// destroys it.
	Removed bool
}

// Source lists the cloud-VM servers, removed ones with a host included.
type Source interface {
	CloudVMServers(ctx context.Context) ([]ServerSpec, error)
}

// Intent is what an owner or moderator asked of a host, or the hub itself (an idle park). It is
// kept apart from the state the tick writes, so a request is never lost to a tick in flight.
type Intent struct {
	Desired        string
	TargetSize     string
	DestroyVolume  bool
	ReleaseAddress bool
	// RequestAuditID is the audit row of the request the next transition carries out; 0 for none.
	RequestAuditID int64
}

// Host is the hub's record of one server's host (a server_hosts row).
type Host struct {
	ServerID   string
	LocationID string
	Intent
	State           string
	Step            string
	Size            string
	DiskGiB         int
	InstanceID      string
	InstanceAddress string
	VolumeID        string
	Address         string
	AddressReserved bool
	FirewallID      string
	Egress          []string
	Operation       *cloud.Operation
	HostKey         string
	EnvSHA256       []byte
	Attempts        int
	NextAttemptAt   time.Time
	ErrorCode       string
	LastError       string
	StateSince      time.Time
	// InstanceCreatedAt is when the current instance was asked for.
	InstanceCreatedAt time.Time
	EmptySince        time.Time
	BootSeconds       int
	ReadySeconds      int
}

// Event is one transition of a host.
type Event struct {
	ServerID    string
	At          time.Time
	From, To    string
	Step        string
	Detail      string
	Size        string
	PriceHourly float64
	AuditID     int64
}

// HostStore keeps hosts and their events.
type HostStore interface {
	Hosts(ctx context.Context) ([]Host, error)
	SaveHost(ctx context.Context, h Host) error
	AppendEvent(ctx context.Context, e Event) error
}

// Remote runs a command on a host over SSH, as the runtime user, with stdin. hostKey is the key
// the host presented (an authorized_keys line), returned even when the command failed, so the
// hub can trust it on first use. A host presenting another key than t.HostKey is
// ErrHostKeyChanged.
type Remote interface {
	Run(ctx context.Context, t RemoteTarget, stdin io.Reader, command string) (out []byte, hostKey string, err error)
}

// RemoteTarget is where and as whom Remote runs.
type RemoteTarget struct {
	Address string
	// Port is the SSH port; 0 is 22 (a local VM's forwarded port in a drill).
	Port   int
	User   string
	Signer ssh.Signer
	// HostKey is the key the host must present; "" trusts the first one.
	HostKey string
}

// Options are the Manager's collaborators. Providers and Remote are required.
type Options struct {
	// Providers makes the provider for a location.
	Providers func(ctx context.Context, l Location) (cloud.Provider, error)
	Remote    Remote
	// Egress is the hub's own addresses (CIDRs), which the hosts' firewalls admit RCON and SSH
	// from.
	Egress func(ctx context.Context) ([]string, error)
	// DefaultImages are the OS images by provider name, for a location without one.
	DefaultImages map[string]string
	Runner        *jobs.Runner
	Logger        *slog.Logger
	Registerer    prometheus.Registerer
	Now           func() time.Time
	// Sleep waits (the park warning); a test makes it return at once.
	Sleep func(ctx context.Context, d time.Duration)
	// Wake runs a host's state machine soon after a request; nil runs a tick in the background.
	Wake func(serverID string)
}

// Manager owns every cloud-VM server's host.
type Manager struct {
	src   Source
	store HostStore
	games map[string]GameFactory
	opt   Options
	log   *slog.Logger
	met   *metrics

	mu        sync.Mutex
	loaded    bool
	hosts     map[string]*hostRT
	specs     map[string]ServerSpec
	providers map[string]providerEntry
	egress    []string
	egressAt  time.Time
	started   map[string]bool
}

type providerEntry struct {
	key [32]byte
	p   cloud.Provider
}

// hostRT is a host and what the hub knows of it at runtime.
type hostRT struct {
	tickMu sync.Mutex // one tick at a time
	saveMu sync.Mutex // one write at a time, always of the latest

	mu          sync.Mutex
	h           Host
	game        Game
	gameAddr    string
	gamePass    string
	players     int  // humans at the last good status
	known       bool // players is from a status read while running
	bans        map[drivers.Identity]bool
	lastStatus  GameStatus
	statusAt    time.Time
	lastDrift   time.Time
	lastAccount time.Time
	op          string    // the operation under way ("create", "park", …), for its metrics
	opAt        time.Time // when it started
	failing     string    // the error code the failing gauge shows
}

// NewManager builds a manager; Start schedules its jobs.
func NewManager(src Source, st HostStore, games map[string]GameFactory, opt Options) *Manager {
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Sleep == nil {
		opt.Sleep = func(ctx context.Context, d time.Duration) {
			select {
			case <-ctx.Done():
			case <-time.After(d):
			}
		}
	}
	if opt.Registerer == nil {
		opt.Registerer = prometheus.NewRegistry()
	}
	return &Manager{
		src: src, store: st, games: games, opt: opt, log: opt.Logger, met: newMetrics(opt.Registerer),
		hosts: map[string]*hostRT{}, specs: map[string]ServerSpec{}, providers: map[string]providerEntry{}, started: map[string]bool{},
	}
}

// Start schedules the reconcile job, which starts a job per host.
func (m *Manager) Start() {
	m.opt.Runner.Start(jobs.Job{Name: "cloudvm_reconcile", Every: ReconcileEvery, Immediate: true, Run: m.Reconcile})
}

func hostJob(id string) string { return "cloudvm_host:" + id }

// Reconcile rereads the servers, loads the hosts once, refreshes the hub's egress, and starts a
// job for every host that has work (a live server, or a removed one whose host still exists).
func (m *Manager) Reconcile(ctx context.Context) error {
	specs, err := m.src.CloudVMServers(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	loaded := m.loaded
	m.mu.Unlock()
	if !loaded {
		rows, err := m.store.Hosts(ctx)
		if err != nil {
			return err
		}
		m.mu.Lock()
		for _, h := range rows {
			m.hosts[h.ServerID] = &hostRT{h: h}
			m.met.setState(h)
		}
		m.loaded = true
		m.mu.Unlock()
	}
	if err := m.refreshEgress(ctx, false); err != nil {
		m.log.Warn("could not learn the hub's public address; the hosts' firewalls keep their last pin", "error", err.Error())
	}
	now := m.opt.Now()
	var start []string
	m.mu.Lock()
	seen := map[string]bool{}
	for _, s := range specs {
		seen[s.ID] = true
		old, had := m.specs[s.ID]
		m.specs[s.ID] = s
		rt, ok := m.hosts[s.ID]
		if !ok {
			if s.Removed {
				continue // never made, nothing to do
			}
			rt = &hostRT{h: Host{ServerID: s.ID, LocationID: s.Location.ID, Intent: Intent{Desired: DesiredRunning},
				State: StateAbsent, StateSince: now}}
			m.hosts[s.ID] = rt
		}
		if had && old.RCONPassword != s.RCONPassword {
			rt.mu.Lock()
			if rt.game != nil {
				rt.game.SetPassword(s.RCONPassword)
			}
			rt.mu.Unlock()
		}
		if !m.started[s.ID] {
			m.started[s.ID] = true
			start = append(start, s.ID)
		}
	}
	var stop []string
	for id := range m.started {
		if !seen[id] {
			delete(m.started, id)
			delete(m.specs, id)
			stop = append(stop, id)
		}
	}
	m.mu.Unlock()
	for _, id := range start {
		m.opt.Runner.Start(jobs.Job{
			Name: hostJob(id), Label: "cloudvm_host", Every: TickEvery, Immediate: true, Timeout: 15 * time.Minute,
			MaxBackoff: TickEvery, // the host's own backoff is in its row
			Run:        func(ctx context.Context) error { return m.Tick(ctx, id) },
		})
	}
	for _, id := range stop {
		m.opt.Runner.Stop(hostJob(id))
		m.log.Warn("a host's server is no longer a cloud-VM server; the hub stopped looking after it", "server", id)
	}
	return nil
}

// refreshEgress reads the hub's egress when it is older than EgressEvery (or forced).
func (m *Manager) refreshEgress(ctx context.Context, force bool) error {
	m.mu.Lock()
	fresh := !force && m.egress != nil && m.opt.Now().Sub(m.egressAt) < EgressEvery
	m.mu.Unlock()
	if fresh || m.opt.Egress == nil {
		return nil
	}
	cidrs, err := m.opt.Egress(ctx)
	if err != nil {
		return err
	}
	norm, err := cloud.NormalizeCIDRs(cidrs)
	if err != nil {
		return err
	}
	m.mu.Lock()
	if !slices.Equal(norm, m.egress) && m.egress != nil {
		m.log.Info("the hub's egress changed; the hosts' firewalls follow", "egress", norm)
	}
	m.egress, m.egressAt = norm, m.opt.Now()
	m.mu.Unlock()
	return nil
}

func (m *Manager) currentEgress() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.egress)
}

func (m *Manager) host(id string) (*hostRT, ServerSpec, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rt, ok := m.hosts[id]
	s, sok := m.specs[id]
	return rt, s, ok && sok
}

// provider is the location's provider, made again when its token or project changes.
func (m *Manager) provider(ctx context.Context, l Location) (cloud.Provider, error) {
	key := sha256.Sum256([]byte(l.Provider + "\x00" + l.Project + "\x00" + l.Token))
	m.mu.Lock()
	e, ok := m.providers[l.ID]
	m.mu.Unlock()
	if ok && e.key == key {
		return e.p, nil
	}
	p, err := m.opt.Providers(ctx, l)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.providers[l.ID] = providerEntry{key: key, p: p}
	m.mu.Unlock()
	return p, nil
}

// snapshot is a copy of the host.
func (rt *hostRT) snapshot() Host {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return cloneHost(rt.h)
}

func cloneHost(h Host) Host {
	h.Egress = slices.Clone(h.Egress)
	h.EnvSHA256 = slices.Clone(h.EnvSHA256)
	if h.Operation != nil {
		op := *h.Operation
		h.Operation = &op
	}
	return h
}

// Host is a copy of a server's host; ok is false for a server with none.
func (m *Manager) Host(id string) (Host, bool) {
	m.mu.Lock()
	rt, ok := m.hosts[id]
	m.mu.Unlock()
	if !ok {
		return Host{}, false
	}
	return rt.snapshot(), true
}

// Players is the humans on a server at its last status read, and whether that read was while the
// host ran.
func (m *Manager) Players(id string) (int, bool) {
	m.mu.Lock()
	rt, ok := m.hosts[id]
	m.mu.Unlock()
	if !ok {
		return 0, false
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.players, rt.known
}

// persist writes the host's latest state. saveMu makes the last write the latest one.
func (m *Manager) persist(ctx context.Context, rt *hostRT) error {
	rt.saveMu.Lock()
	defer rt.saveMu.Unlock()
	h := rt.snapshot()
	if err := m.store.SaveHost(ctx, h); err != nil {
		return fmt.Errorf("cloudvm: save host %s: %w", h.ServerID, err)
	}
	return nil
}

// request changes a host's intent and wakes its state machine.
func (m *Manager) request(ctx context.Context, id string, change func(h *Host, s ServerSpec) error) (Host, error) {
	rt, s, ok := m.host(id)
	if !ok {
		return Host{}, ErrNoSuchHost
	}
	rt.mu.Lock()
	h := cloneHost(rt.h)
	if err := change(&h, s); err != nil {
		rt.mu.Unlock()
		return Host{}, err
	}
	h.Attempts, h.NextAttemptAt = 0, time.Time{}
	rt.h = h
	rt.mu.Unlock()
	if err := m.persist(ctx, rt); err != nil {
		return Host{}, err
	}
	if m.opt.Wake != nil {
		m.opt.Wake(id)
	} else {
		go func() {
			tctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			if err := m.Tick(tctx, id); err != nil {
				m.log.Debug("a host's tick after a request failed; its job retries", "server", id, "error", err.Error())
			}
		}()
	}
	return rt.snapshot(), nil
}

// SetDesired asks for a host to run or to be parked. auditID is the request's audit row (0 for
// none); the transition it causes carries it.
func (m *Manager) SetDesired(ctx context.Context, id, desired string, auditID int64) (Host, error) {
	if desired != DesiredRunning && desired != DesiredParked {
		return Host{}, fmt.Errorf("%w: desired %q is not %s or %s", ErrInvalid, desired, DesiredRunning, DesiredParked)
	}
	return m.request(ctx, id, func(h *Host, s ServerSpec) error {
		if desired == DesiredRunning && s.Removed {
			return ErrRemoved
		}
		h.Desired, h.RequestAuditID = desired, auditID
		h.DestroyVolume, h.ReleaseAddress = false, false
		return nil
	})
}

// Resize asks for a running host to move to another size at once (the game is stopped, the
// instance resized and started again). With players on it refuses unless force.
func (m *Manager) Resize(ctx context.Context, id, size string, force bool, auditID int64) (Host, error) {
	rt, s, ok := m.host(id)
	if !ok {
		return Host{}, ErrNoSuchHost
	}
	h := rt.snapshot()
	if h.State != StateRunning {
		return Host{}, ErrNotRunning
	}
	if size == h.Size {
		return Host{}, fmt.Errorf("%w: the host is %s already", ErrInvalidSize, size)
	}
	allowed, err := m.Sizes(ctx, id)
	if err != nil {
		return Host{}, err
	}
	if !slices.ContainsFunc(allowed, func(z cloud.Size) bool { return z.Slug == size }) {
		return Host{}, fmt.Errorf("%w: %s (from %s in %s)", ErrInvalidSize, size, h.Size, s.Location.Region)
	}
	if n, known := m.Players(id); !force && (n > 0 || !known) {
		return Host{}, ErrPlayersOn
	}
	return m.request(ctx, id, func(h *Host, _ ServerSpec) error {
		if h.State != StateRunning {
			return ErrNotRunning
		}
		h.TargetSize, h.RequestAuditID = size, auditID
		return nil
	})
}

// Destroy asks for a parked host's remains to go: the instance if any, and the firewall; the data
// volume and the reserved address only when asked.
func (m *Manager) Destroy(ctx context.Context, id string, deleteVolume, releaseAddress bool, auditID int64) (Host, error) {
	return m.request(ctx, id, func(h *Host, s ServerSpec) error {
		parkedWanted := h.Desired == DesiredParked || h.Desired == DesiredAbsent || s.Removed
		if !parkedWanted || (h.State != StateParked && h.State != StateAbsent) {
			return ErrNotParked
		}
		h.Desired, h.DestroyVolume, h.ReleaseAddress, h.RequestAuditID = DesiredAbsent, deleteVolume, releaseAddress, auditID
		return nil
	})
}

// Sizes are the sizes a host can be resized to now: offered in its region, and allowed from its
// size and root disk by the provider.
func (m *Manager) Sizes(ctx context.Context, id string) ([]cloud.Size, error) {
	rt, s, ok := m.host(id)
	if !ok {
		return nil, ErrNoSuchHost
	}
	h := rt.snapshot()
	p, err := m.provider(ctx, s.Location)
	if err != nil {
		return nil, err
	}
	all, err := p.Sizes(ctx)
	if err != nil {
		return nil, err
	}
	from, ok := sizeBySlug(all, h.Size)
	if !ok {
		return nil, nil
	}
	if h.DiskGiB > 0 {
		from.DiskGiB = h.DiskGiB
	}
	var out []cloud.Size
	for _, z := range all {
		if z.Slug != h.Size && slices.Contains(z.Regions, s.Location.Region) && p.CanResize(from, z) == nil {
			out = append(out, z)
		}
	}
	return out, nil
}

func sizeBySlug(all []cloud.Size, slug string) (cloud.Size, bool) {
	i := slices.IndexFunc(all, func(z cloud.Size) bool { return z.Slug == slug })
	if i < 0 {
		return cloud.Size{}, false
	}
	return all[i], true
}

// Price is the hourly price of a host's size, 0 when unknown.
func (m *Manager) Price(ctx context.Context, id, size string) float64 {
	_, s, ok := m.host(id)
	if !ok {
		return 0
	}
	p, err := m.provider(ctx, s.Location)
	if err != nil {
		return 0
	}
	all, err := p.Sizes(ctx)
	if err != nil {
		return 0
	}
	z, _ := sizeBySlug(all, size)
	return z.PriceHourly
}

// Lines takes a batch of log lines a server pushed (the ingest route's tap): the game reads who
// connected, and a banned identity is kicked as it connects.
func (m *Manager) Lines(serverID string, body []byte) {
	m.mu.Lock()
	rt, ok := m.hosts[serverID]
	m.mu.Unlock()
	if !ok {
		return
	}
	rt.mu.Lock()
	g := rt.game
	bans := rt.bans
	rt.mu.Unlock()
	if g == nil {
		return
	}
	m.met.logBatches.WithLabelValues(serverID).Inc()
	for _, p := range g.Log(body) {
		if !bans[p.Identity] {
			continue
		}
		go func(p GamePlayer) {
			ctx, cancel := context.WithTimeout(context.Background(), gameCallTimeout)
			defer cancel()
			if err := g.Kick(ctx, p.UserID, "You are banned from this server."); err != nil {
				m.log.Warn("a banned player connected and could not be kicked", "server", serverID, "player", p.Identity.Subject, "error", err.Error())
				return
			}
			m.met.bansEnforced.WithLabelValues(serverID).Inc()
			m.log.Info("kicked a banned player as they connected", "server", serverID, "player", p.Identity.Subject)
		}(p)
	}
}

// SourceAddresses are the addresses a server's log push may come from: its instance's own, and its
// reserved address, which carries the instance's outbound traffic once the provider's egress
// script has run (cloud.Provider.ReservedEgressScript). ok is false while no instance runs.
func (m *Manager) SourceAddresses(serverID string) ([]netip.Addr, bool) {
	h, ok := m.Host(serverID)
	if !ok || h.InstanceAddress == "" {
		return nil, false
	}
	a, err := netip.ParseAddr(h.InstanceAddress)
	if err != nil {
		return nil, false
	}
	out := []netip.Addr{a}
	if h.AddressReserved && h.Address != "" && h.Address != h.InstanceAddress {
		if r, err := netip.ParseAddr(h.Address); err == nil {
			out = append(out, r)
		}
	}
	return out, true
}

// setBans replaces the identities a server kicks as they connect.
func (m *Manager) setBans(serverID string, ids []drivers.Identity) {
	m.mu.Lock()
	rt, ok := m.hosts[serverID]
	m.mu.Unlock()
	if !ok {
		return
	}
	set := make(map[drivers.Identity]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	rt.mu.Lock()
	rt.bans = set
	rt.mu.Unlock()
}

// gameForLocked is the host's game control at the instance's address, made again when the
// address or the password changes. Call with rt.mu held.
func (m *Manager) gameForLocked(rt *hostRT, s ServerSpec, instanceAddress string) (Game, error) {
	if instanceAddress == "" {
		return nil, ErrNotRunning
	}
	addr := net.JoinHostPort(instanceAddress, strconv.Itoa(s.Profile.RCONPort))
	if rt.game != nil && rt.gameAddr == addr {
		if rt.gamePass != s.RCONPassword {
			rt.game.SetPassword(s.RCONPassword)
			rt.gamePass = s.RCONPassword
		}
		return rt.game, nil
	}
	f, ok := m.games[s.Game]
	if !ok {
		return nil, fmt.Errorf("cloudvm: no game control for %s", s.Game)
	}
	g, err := f(addr, s.RCONPassword, s.Profile, m.log.With("server", s.ID))
	if err != nil {
		return nil, err
	}
	rt.game, rt.gameAddr, rt.gamePass = g, addr, s.RCONPassword
	return g, nil
}

// Status reads a running server's game and records who is on (what the idle park counts from).
// A host that is not running is a *drivers.HostStateError naming its phase.
func (m *Manager) Status(ctx context.Context, id string) (GameStatus, error) {
	rt, s, ok := m.host(id)
	if !ok {
		return GameStatus{}, ErrNoSuchHost
	}
	h := rt.snapshot()
	if h.State != StateRunning {
		return GameStatus{}, &drivers.HostStateError{Phase: phaseOf(h.State)}
	}
	rt.mu.Lock()
	g, err := m.gameForLocked(rt, s, h.InstanceAddress)
	rt.mu.Unlock()
	if err != nil {
		return GameStatus{}, err
	}
	st, err := g.Status(ctx)
	if err != nil {
		return GameStatus{}, err
	}
	m.observe(rt, st, m.opt.Now())
	return st, nil
}

// phaseOf is the phase a driver reports for a host that is not running.
func phaseOf(state string) string {
	switch state {
	case StateParked, StateAbsent:
		return drivers.HostParked
	}
	return drivers.HostProvisioning
}

// observe records a status read of a running host: the players, and since when it is empty.
func (m *Manager) observe(rt *hostRT, st GameStatus, now time.Time) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.players, rt.known, rt.lastStatus, rt.statusAt = st.Players, true, st, now
	switch {
	case st.Players > 0:
		rt.h.EmptySince = time.Time{}
	case rt.h.EmptySince.IsZero():
		rt.h.EmptySince = now
	}
}

// commit merges a tick's work into the host and writes it with its events. A request that came in
// while the tick ran keeps its intent; otherwise the tick's intent stands (it may have consumed a
// request).
func (m *Manager) commit(ctx context.Context, rt *hostRT, before, after Host, events []Event) error {
	rt.mu.Lock()
	if rt.h.Intent != before.Intent {
		after.Intent = rt.h.Intent
	}
	after.EmptySince = rt.h.EmptySince
	changed := !reflect.DeepEqual(rt.h, after)
	rt.h = after
	rt.mu.Unlock()
	if changed {
		if err := m.persist(ctx, rt); err != nil {
			return err
		}
		m.met.setState(after)
		m.met.setPrice(after.ServerID, after.Size, m.Price(ctx, after.ServerID, after.Size), after.State == StateRunning)
	}
	for _, e := range events {
		if err := m.store.AppendEvent(ctx, e); err != nil {
			m.log.Error("a host transition was not recorded", "server", e.ServerID, "from", e.From, "to", e.To, "error", err.Error())
		}
	}
	return nil
}
