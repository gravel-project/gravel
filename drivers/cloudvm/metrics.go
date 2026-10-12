package cloudvm

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// The states a host gauge reports, every one, so a dashboard sees a 0 as well as a 1.
var allStates = []string{StateAbsent, StateCreating, StateBooting, StateStarting, StateRunning, StateParking, StateParked, StateResizing, StateDestroying}

type metrics struct {
	state        *prometheus.GaugeVec
	since        *prometheus.GaugeVec
	failing      *prometheus.GaugeVec
	ops          *prometheus.CounterVec
	duration     *prometheus.HistogramVec
	running      *prometheus.CounterVec
	price        *prometheus.GaugeVec
	logBatches   *prometheus.CounterVec
	bansEnforced *prometheus.CounterVec
	envDrift     *prometheus.CounterVec
	tier         *prometheus.GaugeVec
	pending      *prometheus.GaugeVec
	decisions    *prometheus.CounterVec
	resizes      *prometheus.CounterVec
}

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		state: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gravel_cloudvm_host_state", Help: "1 for the state a cloud-VM server's host is in, 0 for the others.",
		}, []string{"server", "state"}),
		since: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gravel_cloudvm_state_since_timestamp_seconds", Help: "When a cloud-VM server's host entered its state.",
		}, []string{"server"}),
		failing: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gravel_cloudvm_host_failing", Help: "1 while a step of a cloud-VM server's host keeps failing, by its error code.",
		}, []string{"server", "code"}),
		ops: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gravel_cloudvm_operations_total", Help: "Host operations (create, unpark, park, resize, destroy, recreate, restart) by result; an error is one failed attempt.",
		}, []string{"server", "op", "result"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gravel_cloudvm_operation_duration_seconds", Help: "How long a host operation took, from leaving one resting state to reaching the next.",
			Buckets: []float64{5, 15, 30, 60, 120, 300, 600, 1200, 1800, 3600},
		}, []string{"op"}),
		running: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gravel_cloudvm_running_seconds_total", Help: "Time a cloud-VM server's host has run (what the cloud bills).",
		}, []string{"server"}),
		price: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gravel_cloudvm_hourly_price_usd", Help: "The hourly price of a running host's size; absent while it is parked.",
		}, []string{"server", "size"}),
		logBatches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gravel_cloudvm_log_batches_total", Help: "Log batches a cloud-VM server pushed that reached its game's roster.",
		}, []string{"server"}),
		bansEnforced: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gravel_cloudvm_bans_enforced_total", Help: "Banned players kicked as they connected to a cloud-VM server.",
		}, []string{"server"}),
		envDrift: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gravel_cloudvm_env_drift_total", Help: "Times the game's environment file on a host was not the one the hub pushed, and was pushed again.",
		}, []string{"server"}),
	}
	m.tier = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gravel_cloudvm_tier_players", Help: "The players the rung of its scaling ladder a running host is on is sized for; absent without a policy.",
	}, []string{"server"})
	m.pending = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gravel_cloudvm_scale_pending", Help: "1 while a scaling decision waits for a boundary, by its direction.",
	}, []string{"server", "direction"})
	m.decisions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gravel_cloudvm_scale_decisions_total", Help: "Scaling decisions by direction and what became of them: decided, executed at a boundary, or cancelled.",
	}, []string{"server", "direction", "outcome"})
	m.resizes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gravel_cloudvm_resizes_total", Help: "Resizes of a host (the policy's and the owner's) by direction and result: ok, refused by the provider, or failed and started at the old size.",
	}, []string{"server", "direction", "result"})
	reg.MustRegister(m.state, m.since, m.failing, m.ops, m.duration, m.running, m.price, m.logBatches, m.bansEnforced, m.envDrift,
		m.tier, m.pending, m.decisions, m.resizes)
	return m
}

func (m *metrics) setState(h Host) {
	for _, s := range allStates {
		v := 0.0
		if s == h.State {
			v = 1
		}
		m.state.WithLabelValues(h.ServerID, s).Set(v)
	}
	if !h.StateSince.IsZero() {
		m.since.WithLabelValues(h.ServerID).Set(float64(h.StateSince.UnixNano()) / 1e9)
	}
}

func (m *metrics) setFailing(rt *hostRT, server, code string) {
	rt.mu.Lock()
	old := rt.failing
	rt.failing = code
	rt.mu.Unlock()
	if old != "" && old != code {
		m.failing.DeleteLabelValues(server, old)
	}
	m.failing.WithLabelValues(server, code).Set(1)
}

func (m *metrics) clearFailing(rt *hostRT, server string) {
	rt.mu.Lock()
	old := rt.failing
	rt.failing = ""
	rt.mu.Unlock()
	if old != "" {
		m.failing.DeleteLabelValues(server, old)
	}
}

// resting states end an operation.
func resting(state string) bool {
	return state == StateRunning || state == StateParked || state == StateAbsent
}

// opOf is the operation a transition out of a state starts (or, for a state mid-way, the one it
// belongs to, for counting a failed attempt).
func opOf(state string) string {
	switch state {
	case StateCreating, StateBooting, StateStarting:
		return "create"
	case StateParking:
		return "park"
	case StateResizing:
		return "resize"
	case StateDestroying:
		return "destroy"
	}
	return state
}

func opName(from, to string) string {
	switch {
	case from == StateAbsent && to == StateCreating:
		return "create"
	case from == StateParked && to == StateCreating:
		return "unpark"
	case from == StateRunning && to == StateCreating:
		return "recreate"
	case from == StateRunning && (to == StateStarting || to == StateBooting):
		return "restart"
	case from == StateRunning && to == StateParking:
		return "park"
	case from == StateRunning && to == StateResizing:
		return "resize"
	case to == StateDestroying:
		return "destroy"
	}
	return ""
}

// transition times operations: leaving a resting state starts one, reaching one ends it.
func (m *metrics) transition(rt *hostRT, server, from, to string, now time.Time) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if resting(from) || rt.op == "" {
		if op := opName(from, to); op != "" {
			rt.op, rt.opAt = op, now
		}
	}
	if resting(to) && rt.op != "" {
		m.ops.WithLabelValues(server, rt.op, "ok").Inc()
		m.duration.WithLabelValues(rt.op).Observe(now.Sub(rt.opAt).Seconds())
		rt.op = ""
	}
}

// setPrice shows a running host's price, nothing for one that is not.
func (m *metrics) setPrice(server, size string, price float64, running bool) {
	m.price.DeletePartialMatch(prometheus.Labels{"server": server})
	if running && size != "" {
		m.price.WithLabelValues(server, size).Set(price)
	}
}

// setTier shows the players a host's rung is sized for; 0 removes the series.
func (m *metrics) setTier(server string, players int) {
	if players <= 0 {
		m.tier.DeleteLabelValues(server)
		return
	}
	m.tier.WithLabelValues(server).Set(float64(players))
}

// setPending shows a decision waiting in one direction; "" shows none.
func (m *metrics) setPending(server, dir string) {
	for _, d := range []string{DirectionUp, DirectionDown} {
		v := 0.0
		if d == dir {
			v = 1
		}
		m.pending.WithLabelValues(server, d).Set(v)
	}
}
