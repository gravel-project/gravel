package notify_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/gravel-project/gravel/internal/events"
	"github.com/gravel-project/gravel/internal/notify"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/store"
)

// memLog is the events log in memory, filtering as the store does.
type memLog struct {
	mu    sync.Mutex
	evs   []store.Event
	reads int
}

func (l *memLog) add(kind, server, game string) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	sev, err := events.SeverityOf(kind)
	if err != nil {
		panic(err)
	}
	id := int64(len(l.evs) + 1)
	l.evs = append(l.evs, store.Event{ID: id, At: t0, Kind: kind, Severity: string(sev), ServerID: server, GameID: game, Summary: kind + " " + server})
	return id
}

func (l *memLog) ListEvents(_ context.Context, _ uuid.UUID, q store.EventQuery) ([]store.Event, int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reads++
	if !q.Follow {
		return nil, 0, errors.New("the dispatcher always follows")
	}
	var out []store.Event
	for _, e := range l.evs {
		switch {
		case e.ID <= q.AfterID:
		case len(q.Kinds) > 0 && !slices.Contains(q.Kinds, e.Kind) && !slices.Contains(q.Kinds, events.Group(e.Kind)):
		case q.MinSeverity != "" && events.Rank(events.Severity(e.Severity)) < events.Rank(q.MinSeverity):
		case len(q.Servers) > 0 && !slices.Contains(q.Servers, e.ServerID):
		case q.Games != nil && !slices.Contains(q.Games, e.GameID):
		default:
			out = append(out, e)
		}
		if len(out) == q.Limit {
			break
		}
	}
	return out, int64(len(l.evs)), nil
}

// memCursors are the notifier cursors in memory, with the store's rules.
type memCursors struct {
	mu   sync.Mutex
	log  *memLog
	rows map[string]*store.NotifierCursor
}

func (c *memCursors) NotifierCursor(_ context.Context, _ uuid.UUID, name string, now time.Time) (store.NotifierCursor, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rows[name] == nil {
		c.log.mu.Lock()
		head := int64(len(c.log.evs))
		c.log.mu.Unlock()
		c.rows[name] = &store.NotifierCursor{Name: name, AfterID: head, CreatedAt: now}
	}
	return *c.rows[name], nil
}

func (c *memCursors) NotifierCursors(context.Context, uuid.UUID) ([]store.NotifierCursor, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []store.NotifierCursor
	for _, r := range c.rows {
		out = append(out, *r)
	}
	return out, nil
}

func (c *memCursors) AdvanceNotifier(_ context.Context, _ uuid.UUID, name string, afterID int64, sent *time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.rows[name]
	r.AfterID = max(r.AfterID, afterID)
	if sent != nil {
		r.LastSentAt = sent
	}
	r.LastError, r.FailingSince, r.Failures, r.NextAttemptAt = "", nil, 0, nil
	return nil
}

func (c *memCursors) NotifierFailed(_ context.Context, _ uuid.UUID, name, msg string, at, next time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.rows[name]
	r.LastError, r.Failures, r.NextAttemptAt = msg, r.Failures+1, &next
	if r.FailingSince == nil {
		r.FailingSince = &at
	}
	return nil
}

func (c *memCursors) get(name string) store.NotifierCursor {
	c.mu.Lock()
	defer c.mu.Unlock()
	return *c.rows[name]
}

// recorder is a notifier that records what it delivered and fails while told to.
type recorder struct {
	name string
	mu   sync.Mutex
	got  [][]int64
	fail error
}

func (r *recorder) Name() string { return r.name }

func (r *recorder) Notify(_ context.Context, evs []notify.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	var ids []int64
	for _, e := range evs {
		ids = append(ids, e.ID)
	}
	r.got = append(r.got, ids)
	return nil
}

func (r *recorder) delivered() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Concat(r.got...)
}

func (r *recorder) setFail(err error) {
	r.mu.Lock()
	r.fail = err
	r.mu.Unlock()
}

type rig struct {
	log       *memLog
	cursors   *memCursors
	notifiers []org.Notifier
	recs      map[string]*recorder
	buildErr  error
	now       time.Time
	reg       *prometheus.Registry
	d         *notify.Dispatcher
	mu        sync.Mutex
}

func newRig(ns ...org.Notifier) *rig {
	r := &rig{log: &memLog{}, notifiers: ns, recs: map[string]*recorder{}, now: t0, reg: prometheus.NewRegistry()}
	r.cursors = &memCursors{log: r.log, rows: map[string]*store.NotifierCursor{}}
	r.d = notify.NewDispatcher(notify.Options{Log: r.log, Cursors: r.cursors, Registry: r.reg,
		Now: func() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.now },
		Notifiers: func(context.Context) ([]org.Notifier, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			return slices.Clone(r.notifiers), nil
		},
		Build: func(n org.Notifier) (notify.Notifier, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.buildErr != nil {
				return nil, r.buildErr
			}
			if r.recs[n.Name] == nil {
				r.recs[n.Name] = &recorder{name: n.Name}
			}
			return r.recs[n.Name], nil
		}})
	return r
}

func (r *rig) run(t *testing.T) {
	t.Helper()
	if err := r.d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) rec(name string) *recorder {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.recs[name] == nil {
		r.recs[name] = &recorder{name: name}
	}
	return r.recs[name]
}

func (r *rig) advance(d time.Duration) {
	r.mu.Lock()
	r.now = r.now.Add(d)
	r.mu.Unlock()
}

func phone() org.Notifier { return org.Notifier{Name: "phone", Kind: org.NotifierNtfy, Topic: "t"} }

func TestDispatcherNoNotifiersReadsNothing(t *testing.T) {
	r := newRig()
	r.log.add("server.unreachable", "s1", "wardogs")
	r.run(t)
	if r.log.reads != 0 || len(r.cursors.rows) != 0 {
		t.Errorf("no notifiers: %d reads, %d cursors", r.log.reads, len(r.cursors.rows))
	}
}

// A new notifier starts from the head: nothing from before it existed goes out.
func TestDispatcherStartsAtTheHead(t *testing.T) {
	r := newRig(phone())
	r.log.add("server.unreachable", "s1", "wardogs")
	r.run(t)
	if got := r.rec("phone").delivered(); len(got) != 0 {
		t.Fatalf("history went out: %v", got)
	}
	id := r.log.add("server.unreachable", "s2", "wardogs")
	r.run(t)
	if got := r.rec("phone").delivered(); !slices.Equal(got, []int64{id}) {
		t.Errorf("delivered %v, want [%d]", got, id)
	}
}

// The cursor moves only when a batch was delivered; a failed batch goes out again whole after
// the backoff, and nobody gets an event twice.
func TestDispatcherRetriesWithoutDuplicates(t *testing.T) {
	r := newRig(phone())
	r.run(t) // the cursor, at the head (0)
	a := r.log.add("server.unreachable", "s1", "wardogs")
	b := r.log.add("backup.failed", "", "")
	r.rec("phone").setFail(errors.New("answered 503 Service Unavailable"))
	r.run(t)
	c := r.cursors.get("phone")
	if c.AfterID != 0 || c.Failures != 1 || c.LastError != "answered 503 Service Unavailable" || c.FailingSince == nil || !c.NextAttemptAt.Equal(t0.Add(notify.Every)) {
		t.Fatalf("after a failure: %+v", c)
	}
	r.rec("phone").setFail(nil)
	r.run(t) // still backing off
	if got := r.rec("phone").delivered(); len(got) != 0 {
		t.Fatalf("sent during the backoff: %v", got)
	}
	r.advance(notify.Every)
	r.run(t)
	r.run(t)
	if got := r.rec("phone").delivered(); !slices.Equal(got, []int64{a, b}) {
		t.Errorf("delivered %v, want [%d %d] once", got, a, b)
	}
	if c := r.cursors.get("phone"); c.AfterID != b || c.Failures != 0 || c.FailingSince != nil || c.LastError != "" {
		t.Errorf("after the retry: %+v", c)
	}
	if ok, bad := deliveries(t, r.reg, "ok"), deliveries(t, r.reg, "error"); ok != 1 || bad != 1 {
		t.Errorf("gravel_notifications_total ok %v error %v", ok, bad)
	}
}

// deliveries is gravel_notifications_total for a result, every notifier summed.
func deliveries(t *testing.T, reg *prometheus.Registry, result string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	v := 0.0
	for _, mf := range mfs {
		if mf.GetName() != "gravel_notifications_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "result" && l.GetValue() == result {
					v += m.GetCounter().GetValue()
				}
			}
		}
	}
	return v
}

// The filter: kinds and groups, the severity floor (warning by default), servers and games. The
// cursor passes over what the filter leaves out.
func TestDispatcherFilters(t *testing.T) {
	r := newRig(
		phone(),
		org.Notifier{Name: "kinds", Kind: org.NotifierNtfy, Topic: "t", Events: org.NotifierEvents{Kinds: []string{"backup", "role.granted"}, MinSeverity: "info"}},
		org.Notifier{Name: "wd", Kind: org.NotifierNtfy, Topic: "t", Events: org.NotifierEvents{Games: []string{"wardogs"}, MinSeverity: "info"}},
		org.Notifier{Name: "one", Kind: org.NotifierNtfy, Topic: "t", Events: org.NotifierEvents{Servers: []string{"cs2-1"}}},
	)
	r.run(t)
	down := r.log.add("server.unreachable", "wd-1", "wardogs") // warning
	r.log.add("server.recovered", "wd-1", "wardogs")           // info
	grant := r.log.add("role.granted", "", "")                 // notice
	backup := r.log.add("backup.failed", "", "")               // critical
	cs := r.log.add("server.unreachable", "cs2-1", "cs2")      // warning
	r.run(t)
	for name, want := range map[string][]int64{
		"phone": {down, backup, cs},
		"kinds": {grant, backup},
		"wd":    {down, down + 1},
		"one":   {cs},
	} {
		if got := r.rec(name).delivered(); !slices.Equal(got, want) {
			t.Errorf("%s delivered %v, want %v", name, got, want)
		}
	}
	// Nothing new the filter takes: the cursor still reaches the head.
	r.log.add("server.recovered", "cs2-1", "cs2")
	r.run(t)
	if c := r.cursors.get("one"); c.AfterID != int64(len(r.log.evs)) {
		t.Errorf("the cursor did not pass over the rest: %d of %d", c.AfterID, len(r.log.evs))
	}
}

// A notifier removed from the settings keeps its cursor; added again, it resumes where it stopped.
func TestDispatcherResumesAfterRemoval(t *testing.T) {
	r := newRig(phone())
	r.run(t)
	a := r.log.add("server.unreachable", "s1", "wardogs")
	r.run(t)
	r.mu.Lock()
	r.notifiers = nil
	r.mu.Unlock()
	b := r.log.add("server.unreachable", "s2", "wardogs")
	r.run(t)
	r.mu.Lock()
	r.notifiers = []org.Notifier{phone()}
	r.mu.Unlock()
	r.run(t)
	if got := r.rec("phone").delivered(); !slices.Equal(got, []int64{a, b}) {
		t.Errorf("delivered %v, want [%d %d]", got, a, b)
	}
}

// A disabled notifier lets the log pass: enabled again, it starts from then.
func TestDispatcherDisabled(t *testing.T) {
	off := phone()
	off.Disabled = true
	r := newRig(off)
	r.run(t)
	r.log.add("server.unreachable", "s1", "wardogs")
	r.run(t)
	if c := r.cursors.get("phone"); c.AfterID != 1 {
		t.Errorf("a disabled notifier's cursor: %d", c.AfterID)
	}
	r.mu.Lock()
	r.notifiers = []org.Notifier{phone()}
	r.mu.Unlock()
	b := r.log.add("server.unreachable", "s2", "wardogs")
	r.run(t)
	if got := r.rec("phone").delivered(); !slices.Equal(got, []int64{b}) {
		t.Errorf("delivered %v, want [%d]", got, b)
	}
}

// A backlog goes out in batches of BatchSize, MaxBatches a run.
func TestDispatcherBatches(t *testing.T) {
	r := newRig(phone())
	r.run(t)
	n := notify.BatchSize*notify.MaxBatches + 5
	for i := range n {
		r.log.add("server.unreachable", fmt.Sprintf("s%d", i), "wardogs")
	}
	r.run(t)
	rec := r.rec("phone")
	if len(rec.got) != notify.MaxBatches || len(rec.got[0]) != notify.BatchSize || len(rec.delivered()) != notify.BatchSize*notify.MaxBatches {
		t.Fatalf("first run: %d batches, %d events", len(rec.got), len(rec.delivered()))
	}
	r.run(t)
	if len(rec.delivered()) != n || len(rec.got[len(rec.got)-1]) != 5 {
		t.Errorf("second run: %d events", len(rec.delivered()))
	}
}

// A notifier that can't be built (its secret is missing) fails like one that can't deliver; after
// FailingAlarmAfter it is in Failing, unless it is disabled.
func TestDispatcherFailing(t *testing.T) {
	r := newRig(phone())
	r.run(t)
	r.log.add("server.unreachable", "s1", "wardogs")
	r.buildErr = errors.New("no Discord webhook URL: put it in hub.yaml notify.secrets.phone.file")
	r.run(t)
	if c := r.cursors.get("phone"); c.Failures != 1 || !strings.Contains(c.LastError, "notify.secrets.phone.file") {
		t.Fatalf("a build error: %+v", c)
	}
	if f, err := r.d.Failing(context.Background(), t0.Add(59*time.Minute)); err != nil || len(f) != 0 {
		t.Errorf("before an hour: %v %v", f, err)
	}
	f, err := r.d.Failing(context.Background(), t0.Add(time.Hour))
	if err != nil || len(f) != 1 || f[0].Name != "phone" || !f[0].Since.Equal(t0) {
		t.Errorf("after an hour: %+v %v", f, err)
	}
	off := phone()
	off.Disabled = true
	r.mu.Lock()
	r.notifiers = []org.Notifier{off}
	r.mu.Unlock()
	if f, _ := r.d.Failing(context.Background(), t0.Add(2*time.Hour)); len(f) != 0 {
		t.Errorf("a disabled notifier alarms: %+v", f)
	}
}

func TestDispatcherTest(t *testing.T) {
	off := phone()
	off.Disabled = true
	r := newRig(off)
	if err := r.d.Test(context.Background(), "nobody", "HTG"); !errors.Is(err, notify.ErrNoNotifier) {
		t.Errorf("no such notifier: %v", err)
	}
	if err := r.d.Test(context.Background(), "phone", "HTG"); err != nil {
		t.Fatal(err)
	}
	if got := r.rec("phone").delivered(); !slices.Equal(got, []int64{0}) || len(r.cursors.rows) != 0 {
		t.Errorf("the test event: %v, cursors %d", got, len(r.cursors.rows))
	}
	r.rec("phone").setFail(errors.New("answered 401 Unauthorized"))
	if err := r.d.Test(context.Background(), "phone", "HTG"); err == nil || err.Error() != "answered 401 Unauthorized" {
		t.Errorf("a failed test: %v", err)
	}
}
