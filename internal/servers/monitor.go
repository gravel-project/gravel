package servers

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/gravel-project/gravel/drivers"
	"github.com/gravel-project/gravel/drivers/cloudvm"
	"github.com/gravel-project/gravel/internal/jobs"
)

// States an observation reports. They are the public face of a failure: the error itself, which
// may name the server's control address, goes to the log only.
const (
	StateUnknown           = "unknown"            // not polled yet
	StateOK                = "ok"                 // the last poll succeeded
	StateUnreachable       = "unreachable"        // the last polls failed (two in a row marks it unreachable)
	StateCredentialRefused = "credential_refused" // the server refused the credential; fix the file
	StateCredentialMissing = "credential_missing" // the credential file could not be read or is empty
	StateRateLimited       = "rate_limited"       // the server asked the hub to wait
	StateError             = "error"              // anything else; see the hub's log
	// A server whose host the hub makes (the cloud-VM driver) and that is not up: parked (or never
	// made), or being made, started, resized or parked. Nothing is polled until it runs.
	StateParked       = "parked"
	StateProvisioning = "provisioning"
)

// MissesUnreachable is how many failed polls in a row mark a server unreachable.
const MissesUnreachable = 2

// ReconcileEvery is how often the monitor rereads the servers (a `servers apply` from the
// command line) and their credential files (a rotation).
const ReconcileEvery = 30 * time.Second

// BuildEvery is how often the build watcher reads each server's build (ADR-0010 §4): one public
// request that needs no credential. A changed build re-reads the capabilities.
const BuildEvery = time.Minute

// Observation is what the monitor last saw of a server.
type Observation struct {
	ServerID string
	State    string
	// Reachable turns false after MissesUnreachable failed polls in a row, and true on the next
	// success.
	Reachable bool
	// ObservedAt is the last successful poll; zero when there was none.
	ObservedAt   time.Time
	Build        string
	Capabilities []string
	Status       drivers.Status
	Players      []drivers.Player

	// What the fleet's health is judged on (gravel#190, health.go).
	// StateSince is when State last changed.
	StateSince time.Time
	// WatchedSince is when the monitor started watching the server (its definition last changed).
	WatchedSince time.Time
	// PollInterval is the server's poll interval.
	PollInterval time.Duration
	// Misses is the failed polls in a row; a rate-limited poll is not one.
	Misses int
	// FailingSince is the first failed poll of the current run of failures; zero after a success.
	FailingSince time.Time
	// LastPollDuration is how long the last poll took, answered or not.
	LastPollDuration time.Duration
	// LastError is the state of the last failed poll ("unreachable", "credential_refused", ...),
	// public like State; the error itself is in the log. LastErrorAt is when.
	LastError   string
	LastErrorAt time.Time
}

// Monitor polls every live server through its driver.
type Monitor struct {
	svc       *Service
	registry  drivers.Registry
	runner    *jobs.Runner
	logger    *slog.Logger
	userAgent string
	now       func() time.Time
	readFile  func(string) ([]byte, error)

	reachable    *prometheus.GaugeVec
	players      *prometheus.GaugeVec
	maxPlayers   *prometheus.GaugeVec
	observed     *prometheus.GaugeVec
	buildInfo    *prometheus.GaugeVec
	buildChanges *prometheus.CounterVec
	pollDuration *prometheus.HistogramVec

	events HealthEvents

	sink    ObservationSink
	primer  Primer
	mu      sync.Mutex
	workers map[string]*worker
	obs     map[string]Observation
}

type worker struct {
	def    Server
	drv    drivers.Driver
	cred   [32]byte // hash of the credential the driver holds
	misses int
	state  string // last state logged, so a lasting failure is logged once
	// reported is the last health event written for the server ("" none, or after a recovery):
	// events are written on a transition only, and a restarted hub picks up where the log is.
	reported string
}

// NewMonitor builds a monitor and registers its metrics. Call Start before the runner runs.
func NewMonitor(svc *Service, registry drivers.Registry, runner *jobs.Runner, reg prometheus.Registerer, userAgent string, logger *slog.Logger) *Monitor {
	labels := []string{"server", "game"}
	m := &Monitor{
		svc: svc, registry: registry, runner: runner, logger: logger, userAgent: userAgent,
		now: time.Now, readFile: os.ReadFile,
		reachable: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gravel_server_reachable", Help: "1 while the server answers its polls, 0 after two failed polls in a row.",
		}, labels),
		players: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gravel_server_players", Help: "Players on the server at the last successful poll.",
		}, labels),
		maxPlayers: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gravel_server_max_players", Help: "Public player slots at the last successful poll.",
		}, labels),
		observed: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gravel_server_last_observed_timestamp_seconds", Help: "When the server was last polled successfully.",
		}, labels),
		buildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gravel_driver_build_info", Help: "The build a server runs, as its driver last read it; always 1.",
		}, []string{"server", "game", "build"}),
		buildChanges: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gravel_driver_build_changes_total", Help: "Builds the watcher saw a server change to (a game update); each re-read the capabilities.",
		}, labels),
		pollDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gravel_server_poll_duration_seconds", Help: "How long a server's poll took, answered or not; a poll over 2 s makes the server degraded.",
			Buckets: []float64{.05, .1, .25, .5, 1, 2, 5, 10, 30},
		}, labels),
		workers: map[string]*worker{},
		obs:     map[string]Observation{},
	}
	reg.MustRegister(m.reachable, m.players, m.maxPlayers, m.observed, m.buildInfo, m.buildChanges, m.pollDuration)
	return m
}

// Start schedules the reconcile job, which starts and stops the poll jobs.
func (m *Monitor) Start() {
	m.runner.Start(jobs.Job{Name: "servers_reconcile", Every: ReconcileEvery, Immediate: true, Run: m.Reconcile})
}

// Observation is the last observation of a live server; ok is false for a server the monitor
// does not watch.
func (m *Monitor) Observation(id string) (Observation, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.obs[id]
	if !ok {
		return Observation{}, false
	}
	o.Capabilities = slices.Clone(o.Capabilities)
	o.Players = slices.Clone(o.Players)
	return o, true
}

// Driver is the driver the monitor runs for a live server and the capabilities its first good
// poll read (nil before one); moderation calls go through it, so a server has one client and one
// token. ok is false for a server the monitor does not watch.
func (m *Monitor) Driver(id string) (drivers.Driver, []string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.workers[id]
	if !ok {
		return nil, nil, false
	}
	return w.drv, slices.Clone(m.obs[id].Capabilities), true
}

func pollJobName(id string) string  { return "server_poll:" + id }
func buildJobName(id string) string { return "server_build:" + id }

// Reconcile makes the poll jobs match the live servers: a new or changed server gets a fresh
// driver, a rotated credential file reaches its driver, and a removed server stops.
func (m *Monitor) Reconcile(ctx context.Context) error {
	srvs, err := m.svc.Servers(ctx)
	if err != nil {
		return err
	}
	games := map[string]bool{}
	gs, err := m.svc.Games(ctx)
	if err != nil {
		return err
	}
	for _, g := range gs {
		games[g.ID] = true
	}
	live := map[string]bool{}
	var errs []error
	for _, def := range srvs {
		live[def.ID] = true
		if !games[def.Game] {
			continue // its game has no spec in this hub; Games logged it
		}
		cred, readErr := m.credential(def)
		sum := sha256.Sum256([]byte(cred))

		m.mu.Lock()
		w, ok := m.workers[def.ID]
		m.mu.Unlock()
		switch {
		case !ok || !reflect.DeepEqual(w.def, def):
			if err := m.startWorker(def, cred, sum); err != nil {
				errs = append(errs, err)
				continue
			}
			src := def.CredentialFile
			if readErr != nil {
				src += " (unreadable: " + readErr.Error() + ")"
			}
			m.logger.Info("watching server", "server", def.ID, "game", def.Game, "driver", def.Driver, "location", def.Location,
				"poll_interval", time.Duration(def.PollInterval).String(), "credential", src)
		case w.cred != sum:
			w.drv.SetCredential(cred)
			m.mu.Lock()
			w.cred = sum
			m.mu.Unlock()
			m.logger.Info("server credential changed; the driver has the new one", "server", def.ID, "credential", def.CredentialFile)
		}
	}
	m.mu.Lock()
	var gone []Server
	for id, w := range m.workers {
		if !live[id] {
			gone = append(gone, w.def)
		}
	}
	m.mu.Unlock()
	for _, def := range gone {
		m.stopWorker(def)
		m.logger.Info("stopped watching server", "server", def.ID)
	}
	return errors.Join(errs...)
}

// credential is the file's content without surrounding whitespace; "" when it cannot be read.
func (m *Monitor) credential(def Server) (string, error) {
	b, err := m.readFile(def.CredentialFile)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func (m *Monitor) startWorker(def Server, cred string, sum [32]byte) error {
	factory, ok := m.registry[def.Driver]
	if !ok {
		return fmt.Errorf("servers: server %s: no driver %q", def.ID, def.Driver)
	}
	drv, err := factory(drivers.Target{ServerID: def.ID, Game: def.Game, Endpoint: def.Endpoint, Credential: cred, UserAgent: m.userAgent, Logger: m.logger.With("server", def.ID)})
	if err != nil {
		return fmt.Errorf("servers: server %s: %w", def.ID, err)
	}
	m.mu.Lock()
	primer := m.primer
	m.mu.Unlock()
	if primer != nil {
		// A driver that enforces the hub's ban list starts with it; a failure is logged, and the
		// next ban or unban hands the list over again.
		pctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := primer(pctx, def, drv); err != nil {
			m.logger.Warn("could not prime the server's driver", "server", def.ID, "error", err.Error())
		}
		cancel()
	}
	m.mu.Lock()
	if old, ok := m.workers[def.ID]; ok && old.def.Game != def.Game {
		m.deleteMetricsLocked(old.def)
	}
	events := m.events
	m.mu.Unlock()
	reported := ""
	if events != nil {
		// Where the log left the server, so a restart neither repeats an event nor misses a recovery.
		ectx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		last, err := events.LastHealthEvent(ectx, def.ID)
		cancel()
		if err != nil {
			m.logger.Warn("could not read the server's last health event", "server", def.ID, "error", err.Error())
		} else if last != "" && last != EventRecovered {
			reported = last
		}
	}
	now := m.now()
	m.mu.Lock()
	m.workers[def.ID] = &worker{def: def, drv: drv, cred: sum, reported: reported}
	o, ok := m.obs[def.ID]
	if !ok {
		o = Observation{ServerID: def.ID, State: StateUnknown, StateSince: now}
	}
	o.WatchedSince, o.PollInterval = now, time.Duration(def.PollInterval)
	m.obs[def.ID] = o
	m.mu.Unlock()
	every := time.Duration(def.PollInterval)
	m.runner.Start(jobs.Job{
		Name: pollJobName(def.ID), Label: "server_poll", Every: every, Immediate: true,
		MaxBackoff: min(every*8, 2*time.Minute),
		Run:        func(ctx context.Context) error { return m.poll(ctx, def.ID) },
	})
	m.runner.Start(jobs.Job{
		Name: buildJobName(def.ID), Label: "server_build", Every: BuildEvery, MaxBackoff: 5 * time.Minute,
		Run: func(ctx context.Context) error { return m.watchBuild(ctx, def.ID) },
	})
	return nil
}

func (m *Monitor) stopWorker(def Server) {
	m.runner.Stop(pollJobName(def.ID))
	m.runner.Stop(buildJobName(def.ID))
	m.mu.Lock()
	delete(m.workers, def.ID)
	delete(m.obs, def.ID)
	m.deleteMetricsLocked(def)
	m.mu.Unlock()
}

func (m *Monitor) deleteMetricsLocked(def Server) {
	for _, g := range []*prometheus.GaugeVec{m.reachable, m.players, m.maxPlayers, m.observed} {
		g.DeleteLabelValues(def.ID, def.Game)
	}
	m.pollDuration.DeleteLabelValues(def.ID, def.Game)
	m.buildInfo.DeletePartialMatch(prometheus.Labels{"server": def.ID})
	m.buildChanges.DeleteLabelValues(def.ID, def.Game)
}

// watchBuild reads a server's build and, when it changed since the observation's, re-reads the
// capabilities so the API and moderation follow the new build. Before the first good poll it
// does nothing: that poll reads both. A failure is the runner's to retry; the poll owns the
// server's state.
func (m *Monitor) watchBuild(ctx context.Context, id string) error {
	m.mu.Lock()
	w, ok := m.workers[id]
	old := m.obs[id].Build
	m.mu.Unlock()
	if !ok || old == "" {
		return nil
	}
	build, err := w.drv.Build(ctx)
	if errors.Is(err, drivers.ErrHostNotRunning) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("server %s: build: %s", id, stateOf(err))
	}
	if build == old {
		return nil
	}
	caps, err := w.drv.Capabilities(ctx)
	if err != nil {
		return fmt.Errorf("server %s: capabilities: %s", id, stateOf(err))
	}
	m.mu.Lock()
	if m.workers[id] != w {
		m.mu.Unlock()
		return nil
	}
	o := m.obs[id]
	before := o.Capabilities
	o.Build, o.Capabilities = build, caps
	m.obs[id] = o
	def := w.def
	m.setBuildLocked(def, build)
	m.buildChanges.WithLabelValues(def.ID, def.Game).Inc()
	m.mu.Unlock()
	added, removed := setDiff(before, caps)
	m.logger.Warn("server build changed; capabilities read again", "server", id, "from", old, "to", build,
		"capabilities_added", added, "capabilities_removed", removed)
	return nil
}

// setBuildLocked makes the build info gauge name one build for the server.
func (m *Monitor) setBuildLocked(def Server, build string) {
	m.buildInfo.DeletePartialMatch(prometheus.Labels{"server": def.ID})
	m.buildInfo.WithLabelValues(def.ID, def.Game, build).Set(1)
}

// setDiff is what b has that a lacks, and what a has that b lacks.
func setDiff(a, b []string) (added, removed []string) {
	for _, x := range b {
		if !slices.Contains(a, x) {
			added = append(added, x)
		}
	}
	for _, x := range a {
		if !slices.Contains(b, x) {
			removed = append(removed, x)
		}
	}
	return added, removed
}

// poll reads one server's capabilities (the first time), status and players, and records the
// observation. A failure is returned to the runner, which retries with backoff.
func (m *Monitor) poll(ctx context.Context, id string) error {
	m.mu.Lock()
	w, ok := m.workers[id]
	var caps []string
	if ok {
		caps = m.obs[id].Capabilities
	}
	m.mu.Unlock()
	if !ok {
		return nil // stopped between ticks
	}
	start := m.now()
	took := func() time.Duration {
		d := max(m.now().Sub(start), 0)
		m.pollDuration.WithLabelValues(w.def.ID, w.def.Game).Observe(d.Seconds())
		return d
	}

	build := ""
	if caps == nil {
		var err error
		if build, err = w.drv.Build(ctx); err == nil {
			caps, err = w.drv.Capabilities(ctx)
		}
		if err != nil {
			return m.failed(ctx, w, err, took())
		}
	}
	st, err := w.drv.Status(ctx)
	if err != nil {
		return m.failed(ctx, w, err, took())
	}
	var players []drivers.Player
	if slices.Contains(caps, drivers.CapPlayers) {
		if players, err = w.drv.Players(ctx); err != nil {
			return m.failed(ctx, w, err, took())
		}
	}

	d := took()
	now := m.now()
	m.mu.Lock()
	if m.workers[id] != w {
		m.mu.Unlock()
		return nil // replaced while polling
	}
	o := m.obs[id]
	if o.State != StateOK {
		o.StateSince = now
	}
	o.State, o.Reachable, o.ObservedAt = StateOK, true, now
	o.Status, o.Players, o.Capabilities = st, players, caps
	o.Misses, o.FailingSince, o.LastPollDuration = 0, time.Time{}, d
	if build != "" {
		o.Build = build
		m.setBuildLocked(w.def, build)
	}
	m.obs[id] = o
	w.misses = 0
	recovered := w.state != "" && w.state != StateOK
	w.state = StateOK
	def := w.def
	report := w.reported != ""
	m.reachable.WithLabelValues(def.ID, def.Game).Set(1)
	m.players.WithLabelValues(def.ID, def.Game).Set(float64(st.Players))
	m.maxPlayers.WithLabelValues(def.ID, def.Game).Set(float64(st.MaxPlayers))
	m.observed.WithLabelValues(def.ID, def.Game).Set(float64(now.UnixNano()) / 1e9)
	sink := m.sink
	m.mu.Unlock()
	if recovered {
		m.logger.Info("server answering again", "server", id)
	}
	if report {
		m.report(ctx, w, EventRecovered, now, map[string]any{"poll_ms": d.Milliseconds()})
	}
	if sink != nil {
		// The stats store (ADR-0012) records the match from it; its trouble is its own, never the
		// poll's.
		sink.Observe(ctx, def, o)
	}
	return nil
}

// ObservationSink receives every good poll's observation, with the server it is of: the stats
// store's way in (ADR-0012). Observe runs on the poll's goroutine and must not block for long.
type ObservationSink interface {
	Observe(ctx context.Context, server Server, o Observation)
}

// Primer runs once for every driver the monitor starts, before its first poll: the hub's ban
// list handed to a driver that enforces it (Moderation.Prime).
type Primer func(ctx context.Context, server Server, drv drivers.Driver) error

// SetPrimer sets what runs for every new driver; nil runs nothing.
func (m *Monitor) SetPrimer(f Primer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.primer = f
}

// SetSink sets where good polls are sent; nil sends them nowhere.
func (m *Monitor) SetSink(s ObservationSink) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sink = s
}

// hostDown records a server whose host is not running: its phase is the state, it is not
// reachable, and the poll is not a failure (nothing to retry; the host's own machine runs).
func (m *Monitor) hostDown(w *worker, err error) error {
	state := StateProvisioning
	var hs *drivers.HostStateError
	if errors.As(err, &hs) && hs.Phase == drivers.HostParked {
		state = StateParked
	} else if errors.As(err, &hs) && hs.Phase == drivers.HostError {
		state = StateError
	}
	m.mu.Lock()
	if m.workers[w.def.ID] != w {
		m.mu.Unlock()
		return nil
	}
	o := m.obs[w.def.ID]
	if o.State != state {
		o.StateSince = m.now()
	}
	o.State, o.Reachable, o.Players = state, false, nil
	o.Status = drivers.Status{}
	o.Misses, o.FailingSince = 0, time.Time{}
	m.obs[w.def.ID] = o
	w.misses = 0
	first := w.state != state
	w.state = state
	m.reachable.WithLabelValues(w.def.ID, w.def.Game).Set(0)
	m.players.WithLabelValues(w.def.ID, w.def.Game).Set(0)
	m.mu.Unlock()
	if first {
		m.logger.Info("server's host is not running", "server", w.def.ID, "state", state)
	}
	return nil
}

// failed records a failed poll and returns the error for the runner.
func (m *Monitor) failed(ctx context.Context, w *worker, err error, took time.Duration) error {
	if errors.Is(err, drivers.ErrHostNotRunning) {
		return m.hostDown(w, err)
	}
	if errors.Is(err, cloudvm.ErrNotLoaded) {
		// The cloud-VM manager has not read its hosts yet (a hub just started): nothing is known,
		// so nothing is recorded; the next tick asks again (#229).
		m.logger.Debug("the server's cloud-VM host is not loaded yet; polling again next tick", "server", w.def.ID)
		return nil
	}
	state := stateOf(err)
	now := m.now()
	m.mu.Lock()
	if m.workers[w.def.ID] != w {
		m.mu.Unlock()
		return nil
	}
	o := m.obs[w.def.ID]
	if state != StateRateLimited {
		w.misses++
		if o.FailingSince.IsZero() {
			o.FailingSince = now
		}
	}
	if o.State != state {
		o.StateSince = now
	}
	o.State, o.Misses, o.LastPollDuration, o.LastError, o.LastErrorAt = state, w.misses, took, state, now
	switch state {
	case StateCredentialRefused, StateCredentialMissing:
		// The server answered; it is the credential that is wrong.
	default:
		if w.misses >= MissesUnreachable {
			o.Reachable = false
		}
	}
	m.obs[w.def.ID] = o
	first := w.state != state
	w.state = state
	m.reachable.WithLabelValues(w.def.ID, w.def.Game).Set(boolGauge(o.Reachable))
	kind := ""
	switch {
	case state == StateCredentialRefused:
		kind = EventCredentialRefused
	case state == StateCredentialMissing:
		kind = EventCredentialMissing
	case !o.Reachable:
		kind = EventUnreachable
	}
	report := kind != "" && kind != w.reported
	m.mu.Unlock()
	if first {
		m.logger.Warn("server poll failed", "server", w.def.ID, "state", state, "error", err.Error())
	}
	if report {
		m.report(ctx, w, kind, now, map[string]any{"state": state, "misses": o.Misses})
	}
	return fmt.Errorf("server %s: %s", w.def.ID, state)
}

// The health events the monitor writes, on a transition only (gravel#190, ADR-0028).
const (
	EventUnreachable       = "server.unreachable"
	EventRecovered         = "server.recovered"
	EventCredentialRefused = "server.credential_refused"
	EventCredentialMissing = "server.credential_missing"
)

// HealthEvent is one server health event for the events log.
type HealthEvent struct {
	ServerID, Kind, Summary string
	At                      time.Time
	Detail                  map[string]any
}

// HealthEvents is where the monitor writes its events and reads the last one it wrote for a
// server; the hub's is the events log.
type HealthEvents interface {
	AppendHealthEvent(ctx context.Context, e HealthEvent) error
	// LastHealthEvent is the kind of the newest server.* event for the server, "" for none.
	LastHealthEvent(ctx context.Context, serverID string) (string, error)
}

// SetHealthEvents sets where health events go; nil writes none.
func (m *Monitor) SetHealthEvents(e HealthEvents) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = e
}

// report writes a health event and remembers it, so the next transition is measured from it. A
// failed write is logged and left unremembered: the next poll in the same state tries again.
func (m *Monitor) report(ctx context.Context, w *worker, kind string, at time.Time, detail map[string]any) {
	m.mu.Lock()
	events := m.events
	m.mu.Unlock()
	if events == nil {
		return
	}
	summary := map[string]string{
		EventUnreachable:       " stopped answering its polls",
		EventRecovered:         " is answering again",
		EventCredentialRefused: " refuses the hub's credential; fix its credential file",
		EventCredentialMissing: " has no credential: its credential file is missing or empty",
	}[kind]
	e := HealthEvent{ServerID: w.def.ID, Kind: kind, At: at, Summary: w.def.Name + summary, Detail: detail}
	if err := events.AppendHealthEvent(ctx, e); err != nil {
		m.logger.Warn("could not record the server's health event", "server", w.def.ID, "kind", kind, "error", err.Error())
		return
	}
	m.mu.Lock()
	if m.workers[w.def.ID] == w {
		w.reported = kind
		if kind == EventRecovered {
			w.reported = ""
		}
	}
	m.mu.Unlock()
}

func stateOf(err error) string {
	switch {
	case errors.Is(err, drivers.ErrCredentialRefused):
		return StateCredentialRefused
	case errors.Is(err, drivers.ErrCredentialMissing):
		return StateCredentialMissing
	case errors.Is(err, drivers.ErrRateLimited):
		return StateRateLimited
	case errors.Is(err, context.DeadlineExceeded), isNetwork(err):
		return StateUnreachable
	}
	return StateError
}

// isNetwork is a failure to reach the server at all (refused, reset, no route, DNS, timeout).
func isNetwork(err error) bool {
	var ne interface{ Timeout() bool }
	if errors.As(err, &ne) {
		return true
	}
	s := err.Error()
	for _, frag := range []string{"connection refused", "connection reset", "no route to host", "no such host", "network is unreachable", "EOF"} {
		if strings.Contains(s, frag) {
			return true
		}
	}
	return false
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
