package servers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/gravel-project/gravel/drivers/cloudvm"
	"github.com/gravel-project/gravel/internal/store"
)

// Server leases (gravel#20, ADR-0023): an app (a PUG bot, Muster) or a moderator takes a server for a
// match with its own password, settings, plugin set and match config; the hub starts it, says when
// it is ready, and parks it and gives it back after. A lease is a request against a server or a pool
// of servers the manifest marks leasable (vm.lease); the provisioner behind it today takes a standing
// server. A fresh server per lease (#177) and Agones (K1) are provisioners still to come, behind the
// same requests.

// Lease audit actions.
const (
	ActionLeaseRequest = "lease_request"
	ActionLeaseRelease = "lease_release"
)

// LeaseEvery is how often the lease job looks at every live lease.
const LeaseEvery = 5 * time.Second

// PluginWait is how long a running game has to show its plugin set loaded before the lease falls
// back to a vanilla server.
const PluginWait = 3 * time.Minute

// Limits on a request.
const (
	MaxLeaseRef         = 128
	MaxLeaseMatchConfig = 64 << 10
	MaxLeasePlayers     = 1000
	MaxLeaseReason      = 200
)

// Errors a lease request or release returns.
var (
	ErrLeasesOff     = errors.New("servers: this hub leases no server (it has no lease key)")
	ErrInvalidLease  = errors.New("servers: invalid lease request")
	ErrNoSuchLease   = errors.New("servers: no such lease")
	ErrNoFreeServer  = errors.New("servers: no server in the pool is free")
	ErrServerBusy    = errors.New("servers: the server is busy")
	ErrNotLeasable   = errors.New("servers: the server can't be leased")
	ErrLeaseFinished = errors.New("servers: the lease is over")
)

// LeaseRows are the lease rows.
type LeaseRows interface {
	CreateServerLease(ctx context.Context, l store.ServerLease) error
	UpdateServerLease(ctx context.Context, l store.ServerLease) error
	GetServerLease(ctx context.Context, orgID, id uuid.UUID) (store.ServerLease, error)
	LiveLeaseForRef(ctx context.Context, orgID uuid.UUID, holderKind, holder, ref string) (store.ServerLease, error)
	ListServerLeases(ctx context.Context, orgID uuid.UUID, serverID string, liveOnly bool, limit int) ([]store.ServerLease, error)
}

// MatchCheck checks a lease's match config against a game's format (cloudvm.LeaseProfile.MatchFormat).
type MatchCheck func(config []byte) error

// Leases are the hub's server leases.
type Leases struct {
	svc    *Service
	hosts  *Hosts
	rows   LeaseRows
	audit  AuditStore
	orgID  uuid.UUID
	key    []byte
	logger *slog.Logger
	now    func() time.Time
	checks map[string]MatchCheck

	mu   sync.Mutex
	live map[string]store.ServerLease // a server's live lease, by server id
	subs map[uuid.UUID]map[chan struct{}]bool
	met  *leaseMetrics
}

// NewLeases builds the leases; key is the hub's lease key (none: the hub leases nothing).
func NewLeases(svc *Service, hosts *Hosts, rows LeaseRows, audit AuditStore, orgID uuid.UUID, key []byte, reg prometheus.Registerer, logger *slog.Logger) *Leases {
	l := &Leases{svc: svc, hosts: hosts, rows: rows, audit: audit, orgID: orgID, key: key, logger: logger, now: time.Now,
		checks: map[string]MatchCheck{}, live: map[string]store.ServerLease{}, subs: map[uuid.UUID]map[chan struct{}]bool{}, met: newLeaseMetrics(reg)}
	hosts.SetOverlay(l.overlay)
	return l
}

// SetMatchCheck registers the check for a match config format ("matchzy").
func (l *Leases) SetMatchCheck(format string, c MatchCheck) { l.checks[format] = c }

// WithClock replaces the clock (tests).
func (l *Leases) WithClock(now func() time.Time) *Leases {
	l.now = now
	return l
}

// LeaseRequest is what a holder asks for.
type LeaseRequest struct {
	// ServerID or Pool: one of them.
	ServerID, Pool string
	// HolderRef is the holder's own name for what the lease is for (a session id): one live lease
	// per holder and reference, and asking again gets the same lease.
	HolderRef       string
	ExpectedPlayers int
	Settings        map[string]string
	Plugins         []string
	MatchConfig     []byte
	Reason          string
}

// Lease is a lease as its holder sees it: the row, and how to join.
type Lease struct {
	store.ServerLease
	// Password is the lease's join password; only in the answer to the request that holds it.
	Password string
	// Address is where players join (host:port); empty until the server has an address.
	Address string
	// Connect is what a player types to join, password included when Password is.
	Connect string
}

// holder is who asks: an app (its id) or a user in person.
func holder(a Actor) (kind, id string, err error) {
	switch {
	case a.AppID != nil:
		return store.LeaseHolderApp, a.AppID.String(), nil
	case a.UserID != nil:
		return store.LeaseHolderUser, a.UserID.String(), nil
	}
	return "", "", fmt.Errorf("%w: a user or an app must ask", ErrInvalidLease)
}

// Request takes a server for the holder, or answers the holder's live lease for the same
// reference again. The server is the one named, or the first free one in the pool: a running
// empty one first, then a parked one, never one with players on.
func (l *Leases) Request(ctx context.Context, actor Actor, req LeaseRequest) (Lease, error) {
	if len(l.key) == 0 {
		return Lease{}, ErrLeasesOff
	}
	kind, holderID, err := holder(actor)
	if err != nil {
		return Lease{}, err
	}
	if err := checkLeaseRequest(req); err != nil {
		return Lease{}, err
	}
	if prior, err := l.rows.LiveLeaseForRef(ctx, l.orgID, kind, holderID, req.HolderRef); err == nil {
		return l.view(ctx, prior, true), nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return Lease{}, err
	}
	candidates, err := l.candidates(ctx, req)
	if err != nil {
		return Lease{}, err
	}
	busy := ""
	for _, c := range candidates {
		row, err := l.take(ctx, actor, kind, holderID, req, c)
		switch {
		case errors.Is(err, ErrServerBusy) || errors.Is(err, store.ErrLeaseServerTaken):
			busy = err.Error()
			continue
		case errors.Is(err, store.ErrLeaseRefTaken): // the same request, twice at once
			prior, err := l.rows.LiveLeaseForRef(ctx, l.orgID, kind, holderID, req.HolderRef)
			if err != nil {
				return Lease{}, err
			}
			return l.view(ctx, prior, true), nil
		case err != nil:
			return Lease{}, err
		}
		return l.view(ctx, row, true), nil
	}
	if req.Pool != "" {
		return Lease{}, fmt.Errorf("%w: %s", ErrNoFreeServer, req.Pool)
	}
	return Lease{}, fmt.Errorf("%w: %s", ErrServerBusy, busy)
}

func checkLeaseRequest(req LeaseRequest) error {
	var errs []string
	if (req.ServerID == "") == (req.Pool == "") {
		errs = append(errs, "a server or a pool, one of them")
	}
	if !printable(req.HolderRef) || len(req.HolderRef) > MaxLeaseRef {
		errs = append(errs, fmt.Sprintf("holder_ref is required: printable, at most %d bytes", MaxLeaseRef))
	}
	if req.ExpectedPlayers < 0 || req.ExpectedPlayers > MaxLeasePlayers {
		errs = append(errs, fmt.Sprintf("expected_players is 0–%d", MaxLeasePlayers))
	}
	if len(req.MatchConfig) > MaxLeaseMatchConfig {
		errs = append(errs, fmt.Sprintf("the match config is over %d bytes", MaxLeaseMatchConfig))
	}
	if len(req.Reason) > MaxLeaseReason || strings.ContainsAny(req.Reason, "\n\r") {
		errs = append(errs, fmt.Sprintf("the reason is one line of at most %d bytes", MaxLeaseReason))
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalidLease, strings.Join(errs, "; "))
	}
	return nil
}

func printable(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// candidate is a leasable server with its Game spec.
type candidate struct {
	srv  Server
	spec Spec
}

// candidates are the servers a request may take, in the order to try them, each checked against
// the request: the named one, or the pool's running empty ones then the rest.
func (l *Leases) candidates(ctx context.Context, req LeaseRequest) ([]candidate, error) {
	all, err := l.svc.Servers(ctx)
	if err != nil {
		return nil, err
	}
	var out []candidate
	for _, s := range all {
		if s.Driver != cloudvm.Name || s.VM == nil || s.VM.Lease == nil {
			if s.ID == req.ServerID {
				return nil, fmt.Errorf("%w: %s has no vm.lease", ErrNotLeasable, s.ID)
			}
			continue
		}
		if (req.ServerID != "" && s.ID != req.ServerID) || (req.Pool != "" && s.VM.Lease.Pool != req.Pool) {
			continue
		}
		spec, ok := l.svc.specs[s.Game]
		if !ok || spec.CloudVM == nil {
			continue
		}
		if err := l.fits(req, s, spec); err != nil {
			return nil, err
		}
		out = append(out, candidate{srv: s, spec: spec})
	}
	if len(out) == 0 {
		if req.ServerID != "" {
			return nil, fmt.Errorf("%w: no server %s", ErrNotLeasable, req.ServerID)
		}
		return nil, fmt.Errorf("%w: no server is in the pool %q", ErrNotLeasable, req.Pool)
	}
	running := func(c candidate) bool {
		h, ok := l.hosts.mgr.Host(c.srv.ID)
		return ok && h.State == cloudvm.StateRunning
	}
	slices.SortStableFunc(out, func(a, b candidate) int {
		switch ra, rb := running(a), running(b); {
		case ra && !rb:
			return -1
		case rb && !ra:
			return 1
		}
		return strings.Compare(a.srv.ID, b.srv.ID)
	})
	return out, nil
}

// fits checks a request against a server: the settings its lease section allows and the game
// accepts, plugin sets the game has, a match config in the game's format.
func (l *Leases) fits(req LeaseRequest, s Server, spec Spec) error {
	var errs []string
	allowed := s.VM.Lease.Settings
	merged := maps.Clone(s.Settings)
	if merged == nil {
		merged = map[string]string{}
	}
	for k, v := range req.Settings {
		if !slices.Contains(allowed, k) {
			errs = append(errs, fmt.Sprintf("settings.%s: a lease of %s can't set it (vm.lease.settings)", k, s.ID))
		}
		merged[k] = v
	}
	if err := spec.CloudVM.ValidateSettings(merged, s.Name); err != nil {
		errs = append(errs, err.Error())
	}
	lp := spec.CloudVM.Lease
	for _, p := range req.Plugins {
		if lp == nil || lp.Plugins[p].Check == "" {
			errs = append(errs, fmt.Sprintf("plugins: %s has no plugin set %q", spec.Name, p))
		}
	}
	if len(req.Plugins) > 1 {
		errs = append(errs, "plugins: one set at a time")
	}
	if len(req.MatchConfig) > 0 {
		switch {
		case lp == nil || lp.MatchFile == "":
			errs = append(errs, fmt.Sprintf("match_config: %s takes none", spec.Name))
		case len(req.Plugins) == 0:
			errs = append(errs, "match_config: it needs the plugin set that loads it")
		default:
			check, ok := l.checks[lp.MatchFormat]
			if !ok && lp.MatchFormat != "" {
				errs = append(errs, fmt.Sprintf("match_config: this hub can't check the %s format", lp.MatchFormat))
			} else if ok {
				if err := check(req.MatchConfig); err != nil {
					errs = append(errs, "match_config: "+err.Error())
				}
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalidLease, strings.Join(errs, "; "))
	}
	return nil
}

// take leases one server: refused while players are on it, else the row, the audit row, the
// expectation and the start.
func (l *Leases) take(ctx context.Context, actor Actor, kind, holderID string, req LeaseRequest, c candidate) (store.ServerLease, error) {
	l.mu.Lock()
	_, leased := l.live[c.srv.ID]
	l.mu.Unlock()
	if leased {
		return store.ServerLease{}, fmt.Errorf("%w: %s is leased", ErrServerBusy, c.srv.ID)
	}
	h, ok := l.hosts.mgr.Host(c.srv.ID)
	if !ok {
		return store.ServerLease{}, fmt.Errorf("%w: %s has no host yet", ErrServerBusy, c.srv.ID)
	}
	if h.Desired == cloudvm.DesiredAbsent || h.State == cloudvm.StateDestroying {
		return store.ServerLease{}, fmt.Errorf("%w: %s is being destroyed", ErrServerBusy, c.srv.ID)
	}
	if n, _ := l.hosts.mgr.Players(c.srv.ID); n > 0 && h.State == cloudvm.StateRunning {
		return store.ServerLease{}, fmt.Errorf("%w: %s has %d players on", ErrServerBusy, c.srv.ID, n)
	}
	now := l.now().UTC()
	policy := c.srv.VM.Lease.WithDefaults()
	row := store.ServerLease{ID: uuid.New(), OrganizationID: l.orgID, ServerID: c.srv.ID, Pool: req.Pool, HolderKind: kind, Holder: holderID,
		HolderRef: req.HolderRef, State: store.LeaseStarting, Reason: req.Reason, Settings: req.Settings, Plugins: req.Plugins,
		MatchConfig: req.MatchConfig, ExpectedPlayers: req.ExpectedPlayers, CreatedAt: now, MaxUntil: now.Add(time.Duration(policy.Max))}
	auditID, err := l.auditEntry(ctx, actor, c.srv.ID, ActionLeaseRequest, req.Reason, map[string]any{"lease_id": row.ID.String(),
		"pool": req.Pool, "holder_ref": req.HolderRef, "expected_players": req.ExpectedPlayers, "plugins": req.Plugins, "settings": req.Settings})
	if err != nil {
		return store.ServerLease{}, err
	}
	row.AuditID = &auditID
	if err := l.rows.CreateServerLease(ctx, row); err != nil {
		l.finishAudit(ctx, auditID, OutcomeInvalid)
		return store.ServerLease{}, err
	}
	l.mu.Lock()
	l.live[row.ServerID] = row
	l.mu.Unlock()
	l.reconcile(ctx) // the Manager takes the lease's password and settings now, not at its next pass
	if row.ExpectedPlayers > 0 {
		if _, err := l.hosts.mgr.SetExpectation(ctx, row.ServerID, row.ExpectedPlayers, row.MaxUntil, "lease "+row.HolderRef, auditID); err != nil {
			l.logger.Warn("a lease's expected players were not set", "server", row.ServerID, "lease", row.ID, "error", err.Error())
		}
	}
	if _, err := l.hosts.mgr.SetDesired(ctx, row.ServerID, cloudvm.DesiredRunning, auditID); err != nil {
		l.finishAudit(ctx, auditID, StateError)
		return store.ServerLease{}, l.end(ctx, row, store.LeaseFailed, "the server could not start: "+err.Error())
	}
	l.finishAudit(ctx, auditID, OutcomeAccepted)
	l.met.leases.WithLabelValues(row.Pool, "requested").Inc()
	l.logger.Info("server leased", "server", row.ServerID, "lease", row.ID, "holder", holderID, "ref", row.HolderRef, "pool", row.Pool)
	l.notify(row.ID)
	return row, nil
}

// password is a lease's join password: derived from the hub's key and the lease's id, so it is
// never stored and is the same after a restart.
func (l *Leases) password(id uuid.UUID) string {
	mac := hmac.New(sha256.New, l.key)
	mac.Write([]byte("gravel lease password\x00"))
	mac.Write(id[:])
	sum := mac.Sum(nil)
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	out := make([]byte, 10)
	for i := range out {
		out[i] = alphabet[int(sum[i])%len(alphabet)]
	}
	return string(out)
}

// view is a lease with how to join; withPassword only for the holder's request.
func (l *Leases) view(ctx context.Context, row store.ServerLease, withPassword bool) Lease {
	v := Lease{ServerLease: row}
	if withPassword && row.Live() {
		v.Password = l.password(row.ID)
	}
	if h, ok := l.hosts.mgr.Host(row.ServerID); ok && h.Address != "" {
		if srv, err := l.svc.Server(ctx, row.ServerID); err == nil {
			if spec, ok := l.svc.specs[srv.Game]; ok && spec.CloudVM != nil {
				v.Address = net.JoinHostPort(h.Address, strconv.Itoa(spec.CloudVM.JoinPort))
				if lp := spec.CloudVM.Lease; lp != nil && lp.Connect != "" && v.Password != "" {
					v.Connect = strings.NewReplacer("<address>", v.Address, "<password>", v.Password).Replace(lp.Connect)
				}
			}
		}
	}
	return v
}

// Get is a lease, as anyone allowed to see it sees it (no password).
func (l *Leases) Get(ctx context.Context, id uuid.UUID) (Lease, error) {
	row, err := l.rows.GetServerLease(ctx, l.orgID, id)
	if errors.Is(err, store.ErrNotFound) {
		return Lease{}, ErrNoSuchLease
	}
	if err != nil {
		return Lease{}, err
	}
	return l.view(ctx, row, false), nil
}

// List is a server's leases ("" for every server), newest first.
func (l *Leases) List(ctx context.Context, serverID string, liveOnly bool, limit int) ([]Lease, error) {
	rows, err := l.rows.ListServerLeases(ctx, l.orgID, serverID, liveOnly, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Lease, 0, len(rows))
	for _, r := range rows {
		out = append(out, l.view(ctx, r, false))
	}
	return out, nil
}

// HeldBy reports whether the actor holds the lease (the app or the user that took it).
func HeldBy(lease Lease, a Actor) bool {
	kind, id, err := holder(a)
	return err == nil && lease.HolderKind == kind && lease.Holder == id
}

// Release gives a live lease back: the server is parked and its own password and settings come
// back at its next start. ErrLeaseFinished for a lease that is over.
func (l *Leases) Release(ctx context.Context, actor Actor, id uuid.UUID, reason string) (Lease, error) {
	row, err := l.rows.GetServerLease(ctx, l.orgID, id)
	if errors.Is(err, store.ErrNotFound) {
		return Lease{}, ErrNoSuchLease
	}
	if err != nil {
		return Lease{}, err
	}
	if !row.Live() {
		return l.view(ctx, row, false), ErrLeaseFinished
	}
	if len(reason) > MaxLeaseReason || strings.ContainsAny(reason, "\n\r") {
		return Lease{}, fmt.Errorf("%w: the reason is one line of at most %d bytes", ErrInvalidLease, MaxLeaseReason)
	}
	auditID, err := l.auditEntry(ctx, actor, row.ServerID, ActionLeaseRelease, reason, map[string]any{"lease_id": row.ID.String()})
	if err != nil {
		return Lease{}, err
	}
	why := "released by its holder"
	if !HeldBy(Lease{ServerLease: row}, actor) {
		why = "released by a moderator"
	}
	if reason != "" {
		why += ": " + reason
	}
	err = l.end(ctx, row, store.LeaseReleased, why)
	outcome := OutcomeAccepted
	if err != nil {
		outcome = StateError
	}
	l.finishAudit(ctx, auditID, outcome)
	if err != nil {
		return Lease{}, err
	}
	return l.Get(ctx, id)
}

// end finishes a lease (released or failed) and gives the server back: the expectation cleared,
// the host parked (it warns anyone on), the overlay gone, so its next start is its own again.
func (l *Leases) end(ctx context.Context, row store.ServerLease, state, reason string) error {
	now := l.now().UTC()
	row.State, row.Reason, row.ReleasedAt = state, reason, &now
	if err := l.rows.UpdateServerLease(ctx, row); err != nil {
		return err
	}
	l.mu.Lock()
	if cur, ok := l.live[row.ServerID]; ok && cur.ID == row.ID {
		delete(l.live, row.ServerID)
	}
	l.mu.Unlock()
	l.reconcile(ctx)
	if row.ExpectedPlayers > 0 {
		if _, err := l.hosts.mgr.SetExpectation(ctx, row.ServerID, 0, time.Time{}, "", 0); err != nil && !errors.Is(err, cloudvm.ErrInvalid) {
			l.logger.Warn("a lease's expected players were not cleared", "server", row.ServerID, "lease", row.ID, "error", err.Error())
		}
	}
	if h, ok := l.hosts.mgr.Host(row.ServerID); ok && h.Desired == cloudvm.DesiredRunning {
		if _, err := l.hosts.mgr.SetDesired(ctx, row.ServerID, cloudvm.DesiredParked, 0); err != nil && !errors.Is(err, cloudvm.ErrNoSuchHost) {
			l.logger.Warn("a lease's server was not parked", "server", row.ServerID, "lease", row.ID, "error", err.Error())
		}
	}
	l.met.leases.WithLabelValues(row.Pool, state).Inc()
	if row.ReadyAt == nil && state == store.LeaseFailed {
		l.met.leases.WithLabelValues(row.Pool, "never_ready").Inc()
	}
	l.logger.Info("lease over", "server", row.ServerID, "lease", row.ID, "state", state, "reason", reason)
	l.notify(row.ID)
	return nil
}

// LiveLease is a server's live lease, if it has one.
func (l *Leases) LiveLease(serverID string) (uuid.UUID, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	row, ok := l.live[serverID]
	return row.ID, ok
}

// MatchEnded marks a server's live lease's match over (a MatchZy series_end): the lease lasts its
// release_after more, so players see the result, then ends.
func (l *Leases) MatchEnded(ctx context.Context, serverID string) {
	l.mu.Lock()
	row, ok := l.live[serverID]
	l.mu.Unlock()
	if !ok || row.EndedAt != nil {
		return
	}
	now := l.now().UTC()
	row.State, row.EndedAt = store.LeaseEnding, &now
	if err := l.save(ctx, row); err != nil {
		l.logger.Warn("a lease's match end was not recorded", "server", serverID, "lease", row.ID, "error", err.Error())
	}
}

func (l *Leases) save(ctx context.Context, row store.ServerLease) error {
	if err := l.rows.UpdateServerLease(ctx, row); err != nil {
		return err
	}
	l.mu.Lock()
	prior := l.live[row.ServerID]
	l.live[row.ServerID] = row
	l.mu.Unlock()
	if prior.PluginState != row.PluginState {
		l.reconcile(ctx) // a fallback to vanilla changes the server's environment
	}
	l.notify(row.ID)
	return nil
}

// reconcile has the Manager read the servers' specs again, so a lease's overlay reaches the host
// now; a failure is logged, and its next pass picks it up.
func (l *Leases) reconcile(ctx context.Context) {
	if err := l.hosts.mgr.Reconcile(ctx); err != nil {
		l.logger.Warn("the cloud-VM servers were not read again after a lease changed", "error", err.Error())
	}
}

// overlay is what a server's live lease changes about it (Hosts.spec): the password, the settings,
// the plugin set's environment and the match config file.
func (l *Leases) overlay(s *cloudvm.ServerSpec, feedOrigin, feedToken string) {
	l.mu.Lock()
	row, ok := l.live[s.ID]
	l.mu.Unlock()
	if !ok {
		return
	}
	s.JoinPassword = l.password(row.ID)
	if len(row.Settings) > 0 {
		merged := maps.Clone(s.Settings)
		if merged == nil {
			merged = map[string]string{}
		}
		maps.Copy(merged, row.Settings)
		s.Settings = merged
	}
	lp := s.Profile.Lease
	if lp == nil || len(row.Plugins) == 0 || row.PluginState == "vanilla" {
		return
	}
	set := lp.Plugins[row.Plugins[0]]
	env := map[string]string{"GRAVEL_PLUGINS": row.Plugins[0]}
	var files []string
	for _, f := range set.Files {
		files = append(files, f.URL+"|"+f.SHA256+"|"+f.Kind+"|"+f.Dest)
	}
	env["GRAVEL_PLUGIN_FILES"] = strings.Join(files, " ")
	fill := strings.NewReplacer("{hub}", strings.TrimRight(feedOrigin, "/"), "{token}", feedToken)
	for k, v := range set.Env {
		env[k] = fill.Replace(v)
	}
	s.ExtraEnv = env
	if lp.MatchFile != "" && len(row.MatchConfig) > 0 {
		s.Files = append(s.Files, cloudvm.Hook{Path: lp.MatchFile, Content: string(row.MatchConfig)})
	}
}

// Load reads the live leases at start, so a hub that restarts mid-lease carries on.
func (l *Leases) Load(ctx context.Context) error {
	rows, err := l.rows.ListServerLeases(ctx, l.orgID, "", true, 1000)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	clear(l.live)
	for _, r := range rows {
		l.live[r.ServerID] = r
	}
	return nil
}

// Run is the lease job's pass: every live lease takes its next step.
func (l *Leases) Run(ctx context.Context) error {
	l.mu.Lock()
	rows := slices.Collect(maps.Values(l.live))
	l.mu.Unlock()
	l.met.live.Set(float64(len(rows)))
	var errs []error
	for _, row := range rows {
		if err := l.step(ctx, row); err != nil {
			errs = append(errs, fmt.Errorf("lease %s: %w", row.ID, err))
		}
	}
	return errors.Join(errs...)
}

// step moves one live lease on.
func (l *Leases) step(ctx context.Context, row store.ServerLease) error {
	now := l.now().UTC()
	srv, err := l.svc.Server(ctx, row.ServerID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && (srv.VM == nil || srv.VM.Lease == nil)) {
		return l.end(ctx, row, store.LeaseReleased, "the server was taken out of the manifest, or out of leasing")
	}
	if err != nil {
		return err
	}
	policy := srv.VM.Lease.WithDefaults()
	h, ok := l.hosts.mgr.Host(row.ServerID)
	if !ok {
		return l.end(ctx, row, store.LeaseFailed, "the server has no host")
	}
	if h.Desired != cloudvm.DesiredRunning {
		return l.end(ctx, row, store.LeaseReleased, "the server was parked")
	}
	if !now.Before(row.MaxUntil) {
		return l.end(ctx, row, store.LeaseReleased, fmt.Sprintf("the lease reached its most, %s", time.Duration(policy.Max)))
	}
	switch row.State {
	case store.LeaseStarting:
		if now.Sub(row.CreatedAt) >= time.Duration(policy.ReadyWithin) {
			why := fmt.Sprintf("not ready within %s (the host is %s", time.Duration(policy.ReadyWithin), h.State)
			if h.Step != "" {
				why += ", " + h.Step
			}
			if h.ErrorCode != "" {
				why += ", " + h.ErrorCode
			}
			return l.end(ctx, row, store.LeaseFailed, why+")")
		}
		return l.starting(ctx, row, srv, h, now)
	case store.LeaseReady:
		if !h.EmptySince.IsZero() && row.ReadyAt != nil && now.Sub(h.EmptySince) >= time.Duration(policy.IdleAfter) &&
			now.Sub(*row.ReadyAt) >= time.Duration(policy.IdleAfter) {
			return l.end(ctx, row, store.LeaseReleased, fmt.Sprintf("nobody on for %s", time.Duration(policy.IdleAfter)))
		}
	case store.LeaseEnding:
		if row.EndedAt != nil && now.Sub(*row.EndedAt) >= time.Duration(policy.ReleaseAfter) {
			return l.end(ctx, row, store.LeaseReleased, "the match is over")
		}
	}
	return nil
}

// starting waits for the game, its plugin set (or the fallback to vanilla) and its match.
func (l *Leases) starting(ctx context.Context, row store.ServerLease, srv Server, h cloudvm.Host, now time.Time) error {
	if h.State != cloudvm.StateRunning || h.Step != "" || !l.hosts.mgr.EnvCurrent(row.ServerID) {
		return nil // the game isn't up yet, or isn't on the lease's password and plugins yet
	}
	spec, ok := l.svc.specs[srv.Game]
	if !ok || spec.CloudVM == nil {
		return l.end(ctx, row, store.LeaseFailed, "the server's game has no cloudvm section")
	}
	lp := spec.CloudVM.Lease
	if len(row.Plugins) > 0 && row.PluginState == "" {
		set := lp.Plugins[row.Plugins[0]]
		out, err := l.hosts.mgr.Exec(ctx, row.ServerID, set.Check)
		switch {
		case err == nil && strings.Contains(out, set.Expect):
			row.PluginState = "loaded"
			l.met.plugins.WithLabelValues(row.Plugins[0], "loaded").Inc()
		case now.Sub(h.StateSince) < PluginWait:
			return nil // the game is up; the plugins may still be loading
		default:
			// The set didn't load (a CS2 patch broke it, a bad pin): a vanilla server still plays.
			row.PluginState = "vanilla"
			row.PluginReason = fmt.Sprintf("%s didn't show %q within %s of the game starting", set.Check, set.Expect, PluginWait)
			l.met.plugins.WithLabelValues(row.Plugins[0], "vanilla").Inc()
			l.logger.Warn("a lease's plugin set did not load; the server runs vanilla", "server", row.ServerID, "lease", row.ID, "plugins", row.Plugins[0])
			return l.save(ctx, row) // the overlay drops the set: the Manager restarts the game without it
		}
	}
	if len(row.MatchConfig) > 0 && row.PluginState == "loaded" && !row.MatchLoaded && lp != nil && lp.LoadCommand != "" {
		if _, err := l.hosts.mgr.Exec(ctx, row.ServerID, lp.LoadCommand); err != nil {
			l.logger.Warn("a lease's match did not load; trying again", "server", row.ServerID, "lease", row.ID, "error", err.Error())
			return nil
		}
		row.MatchLoaded = true
	}
	row.State, row.ReadyAt = store.LeaseReady, &now
	l.met.ready.Observe(now.Sub(row.CreatedAt).Seconds())
	l.logger.Info("leased server ready", "server", row.ServerID, "lease", row.ID, "after", now.Sub(row.CreatedAt).Round(time.Second).String())
	return l.save(ctx, row)
}

// Subscribe tells ch whenever the lease changes, until cancel.
func (l *Leases) Subscribe(id uuid.UUID) (ch <-chan struct{}, cancel func()) {
	c := make(chan struct{}, 1)
	l.mu.Lock()
	if l.subs[id] == nil {
		l.subs[id] = map[chan struct{}]bool{}
	}
	l.subs[id][c] = true
	l.mu.Unlock()
	return c, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		delete(l.subs[id], c)
		if len(l.subs[id]) == 0 {
			delete(l.subs, id)
		}
	}
}

func (l *Leases) notify(id uuid.UUID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for c := range l.subs[id] {
		select {
		case c <- struct{}{}:
		default: // a change is already waiting to be read
		}
	}
}

func (l *Leases) auditEntry(ctx context.Context, actor Actor, serverID, action, reason string, detail map[string]any) (int64, error) {
	d, err := json.Marshal(detail)
	if err != nil {
		return 0, err
	}
	entry := store.AuditEntry{OrganizationID: l.orgID, At: l.now().UTC(), ActorUserID: actor.UserID, ActorAppID: actor.AppID,
		ServerID: serverID, Action: action, Reason: reason, Detail: d, RequestID: actor.RequestID}
	if o := actor.OnBehalfOf; o != nil {
		entry.OnBehalfProvider, entry.OnBehalfSubject = o.Provider, o.Subject
	}
	return l.audit.InsertAuditEntry(ctx, entry)
}

func (l *Leases) finishAudit(ctx context.Context, id int64, outcome string) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := l.audit.FinishAuditEntry(fctx, l.orgID, id, outcome, l.now().UTC()); err != nil {
		l.logger.Error("a lease request's outcome was not recorded", "audit_id", id, "error", err.Error())
	}
}

type leaseMetrics struct {
	leases  *prometheus.CounterVec
	plugins *prometheus.CounterVec
	ready   prometheus.Histogram
	live    prometheus.Gauge
}

func newLeaseMetrics(reg prometheus.Registerer) *leaseMetrics {
	m := &leaseMetrics{
		leases: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gravel_leases_total", Help: "Server leases by pool and what happened: requested, released, failed, never_ready.",
		}, []string{"pool", "result"}),
		plugins: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gravel_lease_plugins_total", Help: "Plugin sets a lease asked for, by set and result: loaded, or vanilla after a fallback.",
		}, []string{"set", "result"}),
		ready: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "gravel_lease_ready_seconds", Help: "How long a leased server took from the request to ready.",
			Buckets: []float64{30, 60, 120, 180, 300, 600, 900, 1800},
		}),
		live: prometheus.NewGauge(prometheus.GaugeOpts{Name: "gravel_leases_live", Help: "Leases holding a server now."}),
	}
	reg.MustRegister(m.leases, m.plugins, m.ready, m.live)
	return m
}
