// Package jobs runs the hub's background work (ADR-0010): named jobs on an interval, each in its
// own goroutine. A failed run is logged, counted and retried after a backoff; a job never stops
// the hub, because a poll that fails is a state the API reports, not a reason to stop serving.
// Jobs can be started and stopped while the runner runs (a server added or removed by
// `gravel-hub servers apply`).
package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Job is one piece of recurring work.
type Job struct {
	// Name identifies the job; starting a job with a name that runs replaces it.
	Name string
	// Label is the job label on the metrics; empty means Name. Jobs of one kind share a label
	// (every server's poll is "server_poll"), so the label set stays small.
	Label string
	// Every is the interval between the end of one run and the start of the next.
	Every time.Duration
	// MaxBackoff caps the wait after failures, which doubles from Every; zero means Every (no
	// growth).
	MaxBackoff time.Duration
	// Immediate runs the job once at start instead of after the first interval.
	Immediate bool
	// Timeout bounds one run; zero means Every.
	Timeout time.Duration
	Run     func(context.Context) error
}

func (j Job) label() string {
	if j.Label != "" {
		return j.Label
	}
	return j.Name
}

// Runner runs jobs. Start them before or during Run; Run returns when its context ends and every
// job has stopped.
type Runner struct {
	logger      *slog.Logger
	total       *prometheus.CounterVec
	lastSuccess *prometheus.GaugeVec
	now         func() time.Time

	mu       sync.Mutex
	base     context.Context // nil until Run
	stopping bool            // Run is shutting down: Start launches nothing
	running  map[string]*running
	queued   []Job
	wg       sync.WaitGroup
}

type running struct {
	job    Job
	cancel context.CancelFunc
	done   chan struct{}
}

// New builds a runner and registers its metrics.
func New(logger *slog.Logger, reg prometheus.Registerer) *Runner {
	r := &Runner{
		logger: logger,
		total: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gravel_hub_jobs_total", Help: "Background job runs by job and result (ok or error).",
		}, []string{"job", "result"}),
		lastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gravel_hub_job_last_success_timestamp_seconds", Help: "When each job last ran without an error.",
		}, []string{"job"}),
		now:     time.Now,
		running: map[string]*running{},
	}
	reg.MustRegister(r.total, r.lastSuccess)
	return r
}

// Start runs a job, replacing a running one of the same name. Once Run is shutting down it does
// nothing.
func (r *Runner) Start(j Job) {
	if j.Every <= 0 || j.Run == nil || j.Name == "" {
		panic(fmt.Sprintf("jobs: job %q needs a name, an interval and a function", j.Name))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopping {
		return // a job starting another during shutdown (the reconcile job)
	}
	if r.base == nil {
		r.queued = slices.DeleteFunc(r.queued, func(q Job) bool { return q.Name == j.Name })
		r.queued = append(r.queued, j)
		return
	}
	r.stopLocked(j.Name)
	r.launchLocked(j)
}

// Stop stops a job and waits for its run in progress; a job that is not running is ignored.
func (r *Runner) Stop(name string) {
	r.mu.Lock()
	rn := r.running[name]
	r.stopLocked(name)
	r.queued = slices.DeleteFunc(r.queued, func(q Job) bool { return q.Name == name })
	r.mu.Unlock()
	if rn != nil {
		<-rn.done
	}
}

// Names are the running jobs, sorted.
func (r *Runner) Names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.running))
	for n := range r.running {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// Run starts the queued jobs and blocks until ctx ends, then stops them all.
func (r *Runner) Run(ctx context.Context) {
	r.mu.Lock()
	r.base = ctx
	for _, j := range r.queued {
		r.launchLocked(j)
	}
	r.queued = nil
	r.mu.Unlock()
	<-ctx.Done()
	r.mu.Lock()
	r.stopping = true
	for name := range r.running {
		r.stopLocked(name)
	}
	r.mu.Unlock()
	r.wg.Wait()
}

func (r *Runner) stopLocked(name string) {
	if rn, ok := r.running[name]; ok {
		rn.cancel()
		delete(r.running, name)
	}
}

func (r *Runner) launchLocked(j Job) {
	ctx, cancel := context.WithCancel(r.base)
	rn := &running{job: j, cancel: cancel, done: make(chan struct{})}
	r.running[j.Name] = rn
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer close(rn.done)
		r.loop(ctx, j)
	}()
}

func (r *Runner) loop(ctx context.Context, j Job) {
	delay := j.Every
	if j.Immediate {
		delay = 0
	}
	failures := 0
	for {
		if delay > 0 {
			t := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
		err := r.once(ctx, j)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			failures = 0
			delay = j.Every
			r.total.WithLabelValues(j.label(), "ok").Inc()
			r.lastSuccess.WithLabelValues(j.label()).Set(float64(r.now().UnixNano()) / 1e9)
			continue
		}
		failures++
		delay = Backoff(j.Every, j.MaxBackoff, failures)
		r.total.WithLabelValues(j.label(), "error").Inc()
		r.logger.Warn("background job failed; will retry", "job", j.Name, "error", err.Error(), "failures", failures, "retry_in", delay.String())
	}
}

// once runs the job with its timeout, turning a panic into an error.
func (r *Runner) once(ctx context.Context, j Job) (err error) {
	timeout := j.Timeout
	if timeout <= 0 {
		timeout = j.Every
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
			r.logger.Error("background job panicked", "job", j.Name, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
		}
	}()
	return j.Run(ctx)
}

// Backoff is the wait after the given number of consecutive failures: every, doubled per
// failure after the first, capped at max (which, when not above every, means no growth).
func Backoff(every, maxBackoff time.Duration, failures int) time.Duration {
	if maxBackoff <= every || failures <= 1 {
		return every
	}
	d := every
	for i := 1; i < failures && d < maxBackoff; i++ {
		d *= 2
	}
	return min(d, maxBackoff)
}
