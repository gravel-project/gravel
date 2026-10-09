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
)

// MissesUnreachable is how many failed polls in a row mark a server unreachable.
const MissesUnreachable = 2

// ReconcileEvery is how often the monitor rereads the servers (a `servers apply` from the
// command line) and their credential files (a rotation).
const ReconcileEvery = 30 * time.Second

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

	reachable  *prometheus.GaugeVec
	players    *prometheus.GaugeVec
	maxPlayers *prometheus.GaugeVec
	observed   *prometheus.GaugeVec

	mu      sync.Mutex
	workers map[string]*worker
	obs     map[string]Observation
}

type worker struct {
	def    Server
	drv    drivers.ExternalReachable
	cred   [32]byte // hash of the credential the driver holds
	misses int
	state  string // last state logged, so a lasting failure is logged once
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
		workers: map[string]*worker{},
		obs:     map[string]Observation{},
	}
	reg.MustRegister(m.reachable, m.players, m.maxPlayers, m.observed)
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

func pollJobName(id string) string { return "server_poll:" + id }

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
	drv, err := factory(drivers.Target{Endpoint: def.Endpoint, Credential: cred, UserAgent: m.userAgent, Logger: m.logger.With("server", def.ID)})
	if err != nil {
		return fmt.Errorf("servers: server %s: %w", def.ID, err)
	}
	m.mu.Lock()
	if old, ok := m.workers[def.ID]; ok && old.def.Game != def.Game {
		m.deleteMetricsLocked(old.def)
	}
	m.workers[def.ID] = &worker{def: def, drv: drv, cred: sum}
	if _, ok := m.obs[def.ID]; !ok {
		m.obs[def.ID] = Observation{ServerID: def.ID, State: StateUnknown}
	}
	m.mu.Unlock()
	every := time.Duration(def.PollInterval)
	m.runner.Start(jobs.Job{
		Name: pollJobName(def.ID), Label: "server_poll", Every: every, Immediate: true,
		MaxBackoff: min(every*8, 2*time.Minute),
		Run:        func(ctx context.Context) error { return m.poll(ctx, def.ID) },
	})
	return nil
}

func (m *Monitor) stopWorker(def Server) {
	m.runner.Stop(pollJobName(def.ID))
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

	build := ""
	if caps == nil {
		var err error
		if build, err = w.drv.Build(ctx); err == nil {
			caps, err = w.drv.Capabilities(ctx)
		}
		if err != nil {
			return m.failed(w, err)
		}
	}
	st, err := w.drv.Status(ctx)
	if err != nil {
		return m.failed(w, err)
	}
	var players []drivers.Player
	if slices.Contains(caps, drivers.CapPlayers) {
		if players, err = w.drv.Players(ctx); err != nil {
			return m.failed(w, err)
		}
	}

	now := m.now()
	m.mu.Lock()
	if m.workers[id] != w {
		m.mu.Unlock()
		return nil // replaced while polling
	}
	o := m.obs[id]
	o.State, o.Reachable, o.ObservedAt = StateOK, true, now
	o.Status, o.Players, o.Capabilities = st, players, caps
	if build != "" {
		o.Build = build
	}
	m.obs[id] = o
	w.misses = 0
	recovered := w.state != "" && w.state != StateOK
	w.state = StateOK
	def := w.def
	m.reachable.WithLabelValues(def.ID, def.Game).Set(1)
	m.players.WithLabelValues(def.ID, def.Game).Set(float64(st.Players))
	m.maxPlayers.WithLabelValues(def.ID, def.Game).Set(float64(st.MaxPlayers))
	m.observed.WithLabelValues(def.ID, def.Game).Set(float64(now.UnixNano()) / 1e9)
	m.mu.Unlock()
	if recovered {
		m.logger.Info("server answering again", "server", id)
	}
	return nil
}

// failed records a failed poll and returns the error for the runner.
func (m *Monitor) failed(w *worker, err error) error {
	state := stateOf(err)
	m.mu.Lock()
	if m.workers[w.def.ID] != w {
		m.mu.Unlock()
		return nil
	}
	o := m.obs[w.def.ID]
	if state != StateRateLimited {
		w.misses++
	}
	o.State = state
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
	m.mu.Unlock()
	if first {
		m.logger.Warn("server poll failed", "server", w.def.ID, "state", state, "error", err.Error())
	}
	return fmt.Errorf("server %s: %s", w.def.ID, state)
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
