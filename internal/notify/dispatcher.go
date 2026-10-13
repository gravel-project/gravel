package notify

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/gravel-project/gravel/internal/events"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/store"
)

// The dispatcher's pace.
const (
	// Every is how often the dispatcher job runs.
	Every = 10 * time.Second
	// BatchSize is the most events one delivery carries.
	BatchSize = 20
	// MaxBatches bounds the deliveries to one notifier in a run, so a notifier catching up after
	// an outage sends 100 events every Every, not the whole backlog at once.
	MaxBatches = 5
	// MaxBackoff caps the wait after failures, which doubles from Every.
	MaxBackoff = 10 * time.Minute
	// FailingAlarmAfter is how long a notifier may fail before the notifier.failing alarm opens.
	FailingAlarmAfter = time.Hour
)

// ErrNoNotifier is TestNotifier's answer for a name the settings don't have.
var ErrNoNotifier = errors.New("notify: no such notifier")

// Log reads the events log (store.Store).
type Log interface {
	ListEvents(ctx context.Context, orgID uuid.UUID, q store.EventQuery) ([]store.Event, int64, error)
}

// Cursors keep each notifier's place in the log (store.Store).
type Cursors interface {
	NotifierCursor(ctx context.Context, orgID uuid.UUID, name string, now time.Time) (store.NotifierCursor, error)
	NotifierCursors(ctx context.Context, orgID uuid.UUID) ([]store.NotifierCursor, error)
	AdvanceNotifier(ctx context.Context, orgID uuid.UUID, name string, afterID int64, sent *time.Time) error
	NotifierFailed(ctx context.Context, orgID uuid.UUID, name, msg string, at, next time.Time) error
}

// Dispatcher follows the events log for every notifier the settings name.
type Dispatcher struct {
	orgID     uuid.UUID
	notifiers func(ctx context.Context) ([]org.Notifier, error)
	build     func(n org.Notifier) (Notifier, error)
	log       Log
	cursors   Cursors
	now       func() time.Time
	logger    *slog.Logger

	total *prometheus.CounterVec
	lag   *prometheus.GaugeVec
	mu    sync.Mutex
	known map[string]bool // the notifiers with a lag series
}

// Options build a Dispatcher.
type Options struct {
	OrgID uuid.UUID
	// Notifiers reads the settings' notifiers.
	Notifiers func(ctx context.Context) ([]org.Notifier, error)
	// Build makes a notifier from its entry (New with the hub's Deps).
	Build    func(n org.Notifier) (Notifier, error)
	Log      Log
	Cursors  Cursors
	Now      func() time.Time
	Logger   *slog.Logger
	Registry prometheus.Registerer
}

// NewDispatcher builds a dispatcher and registers its metrics.
func NewDispatcher(o Options) *Dispatcher {
	d := &Dispatcher{orgID: o.OrgID, notifiers: o.Notifiers, build: o.Build, log: o.Log, cursors: o.Cursors, now: o.Now, logger: o.Logger,
		total: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gravel_notifications_total", Help: "Deliveries of a batch of events by notifier and result (ok or error).",
		}, []string{"notifier", "result"}),
		lag: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gravel_notifier_lag_seconds", Help: "How old the oldest event waiting for a notifier is; 0 when it has delivered everything.",
		}, []string{"notifier"}),
		known: map[string]bool{},
	}
	if d.now == nil {
		d.now = time.Now
	}
	if d.logger == nil {
		d.logger = slog.Default()
	}
	if o.Registry != nil {
		o.Registry.MustRegister(d.total, d.lag)
	}
	return d
}

// Run is one pass of the dispatcher job: every notifier at once, each on its own, so a slow or
// failing one holds up nothing. With no notifiers it reads nothing but the settings.
func (d *Dispatcher) Run(ctx context.Context) error {
	ns, err := d.notifiers(ctx)
	if err != nil {
		return err
	}
	d.forget(ns)
	var wg sync.WaitGroup
	for _, n := range ns {
		wg.Go(func() { d.one(ctx, n) })
	}
	wg.Wait()
	return nil
}

// forget drops the lag series of notifiers the settings no longer name.
func (d *Dispatcher) forget(ns []org.Notifier) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := map[string]bool{}
	for _, n := range ns {
		now[n.Name] = true
	}
	for name := range d.known {
		if !now[name] {
			d.lag.DeleteLabelValues(name)
			delete(d.known, name)
		}
	}
	maps.Copy(d.known, now)
}

func (d *Dispatcher) one(ctx context.Context, n org.Notifier) {
	log := d.logger.With("notifier", n.Name)
	now := d.now()
	c, err := d.cursors.NotifierCursor(ctx, d.orgID, n.Name, now)
	if err != nil {
		log.Error("notifier cursor", "err", err)
		return
	}
	if n.Disabled {
		// A disabled notifier lets the log pass: enabled again, it starts from then.
		_, head, err := d.log.ListEvents(ctx, d.orgID, store.EventQuery{AfterID: c.AfterID, Follow: true, Limit: 1})
		if err == nil && head > c.AfterID {
			err = d.cursors.AdvanceNotifier(ctx, d.orgID, n.Name, head, nil)
		}
		if err != nil {
			log.Error("notifier: pass over the log", "err", err)
		}
		d.lag.WithLabelValues(n.Name).Set(0)
		return
	}
	if c.NextAttemptAt != nil && now.Before(*c.NextAttemptAt) {
		return // backing off; the lag stays as the failure left it
	}
	var nf Notifier
	after := c.AfterID
	for range MaxBatches {
		q := store.EventQuery{AfterID: after, Follow: true, Limit: BatchSize, Kinds: n.Events.Kinds,
			MinSeverity: n.Events.MinSeverityOrDefault(), Servers: n.Events.Servers}
		if len(n.Events.Games) > 0 {
			q.Games = n.Events.Games
		}
		evs, head, err := d.log.ListEvents(ctx, d.orgID, q)
		if err != nil {
			log.Error("notifier: read the log", "err", err)
			return
		}
		if len(evs) == 0 {
			// Nothing the filter takes: pass over what it left out.
			if head > after {
				if err := d.cursors.AdvanceNotifier(ctx, d.orgID, n.Name, head, nil); err != nil {
					log.Error("notifier: advance", "err", err)
				}
			}
			d.lag.WithLabelValues(n.Name).Set(0)
			return
		}
		batch := make([]Event, 0, len(evs))
		for _, e := range evs {
			batch = append(batch, Event{ID: e.ID, At: e.At, Kind: e.Kind, Severity: events.Severity(e.Severity), ServerID: e.ServerID,
				GameID: e.GameID, Summary: e.Summary, Detail: e.Detail})
		}
		if nf == nil {
			if nf, err = d.build(n); err != nil {
				d.failed(ctx, log, n.Name, c, batch, err)
				return
			}
		}
		sctx, cancel := context.WithTimeout(ctx, SendTimeout)
		err = nf.Notify(sctx, batch)
		cancel()
		if err != nil {
			d.failed(ctx, log, n.Name, c, batch, err)
			return
		}
		d.total.WithLabelValues(n.Name, "ok").Inc()
		sent := d.now()
		last := evs[len(evs)-1].ID
		caughtUp := len(evs) < BatchSize
		if caughtUp {
			last = max(last, head)
		}
		if err := d.cursors.AdvanceNotifier(ctx, d.orgID, n.Name, last, &sent); err != nil {
			// The batch went out but the cursor didn't move: it goes out again next run. Rare, and
			// a duplicate beats a lost event.
			log.Error("notifier: advance after a delivery", "err", err)
			return
		}
		if c.Failures > 0 {
			log.Info("notifier delivering again", "failures", c.Failures)
			c.Failures = 0
		}
		if caughtUp {
			d.lag.WithLabelValues(n.Name).Set(0)
			return
		}
		d.lag.WithLabelValues(n.Name).Set(sent.Sub(evs[len(evs)-1].At).Seconds())
		after = last
	}
}

// failed records a failed delivery: the cursor stays, so the same batch goes out after the backoff.
func (d *Dispatcher) failed(ctx context.Context, log *slog.Logger, name string, c store.NotifierCursor, batch []Event, err error) {
	d.total.WithLabelValues(name, "error").Inc()
	now := d.now()
	d.lag.WithLabelValues(name).Set(now.Sub(batch[0].At).Seconds())
	wait := Backoff(c.Failures + 1)
	log.Warn("notifier failed; the batch goes again after the backoff", "err", err, "events", len(batch), "failures", c.Failures+1, "retry_in", wait)
	if err := d.cursors.NotifierFailed(ctx, d.orgID, name, err.Error(), now, now.Add(wait)); err != nil {
		log.Error("notifier: record the failure", "err", err)
	}
}

// Backoff is the wait after the nth failure in a row: Every, doubling, at most MaxBackoff.
func Backoff(n int) time.Duration {
	w := Every
	for i := 1; i < n && w < MaxBackoff; i++ {
		w *= 2
	}
	return min(w, MaxBackoff)
}

// Test sends the test event to the named notifier, disabled or not, and moves no cursor.
func (d *Dispatcher) Test(ctx context.Context, name, hub string) error {
	ns, err := d.notifiers(ctx)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(ns, func(n org.Notifier) bool { return n.Name == name })
	if i < 0 {
		return ErrNoNotifier
	}
	nf, err := d.build(ns[i])
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, SendTimeout)
	defer cancel()
	return nf.Notify(ctx, []Event{TestEvent(hub, d.now())})
}

// Failing is a notifier that has failed for FailingAlarmAfter or longer.
type Failing struct {
	Name      string
	Since     time.Time
	LastError string
}

// Failing are the enabled notifiers failing for FailingAlarmAfter or longer at now: the
// notifier.failing alarm.
func (d *Dispatcher) Failing(ctx context.Context, now time.Time) ([]Failing, error) {
	ns, err := d.notifiers(ctx)
	if err != nil {
		return nil, err
	}
	if len(ns) == 0 {
		return nil, nil
	}
	cs, err := d.cursors.NotifierCursors(ctx, d.orgID)
	if err != nil {
		return nil, err
	}
	var out []Failing
	for _, c := range cs {
		i := slices.IndexFunc(ns, func(n org.Notifier) bool { return n.Name == c.Name })
		if i < 0 || ns[i].Disabled || c.FailingSince == nil || now.Sub(*c.FailingSince) < FailingAlarmAfter {
			continue
		}
		out = append(out, Failing{Name: c.Name, Since: *c.FailingSince, LastError: c.LastError})
	}
	return out, nil
}
