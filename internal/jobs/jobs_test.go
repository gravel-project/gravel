package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func newRunner(t *testing.T) (*Runner, context.CancelFunc, chan struct{}) {
	t.Helper()
	r := New(slog.New(slog.NewTextHandler(io.Discard, nil)), prometheus.NewRegistry())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return r, cancel, done
}

// eventually waits up to two seconds for cond.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestJobsRunCountAndSurviveFailures(t *testing.T) {
	r, _, _ := newRunner(t)
	var ok, bad, boom atomic.Int32
	r.Start(Job{Name: "ok", Every: 5 * time.Millisecond, Immediate: true, Run: func(context.Context) error { ok.Add(1); return nil }})
	r.Start(Job{Name: "bad", Every: 5 * time.Millisecond, Run: func(context.Context) error { bad.Add(1); return errors.New("nope") }})
	r.Start(Job{Name: "boom", Label: "panics", Every: 5 * time.Millisecond, Run: func(context.Context) error { boom.Add(1); panic("boom") }})
	// The runner counts a run after its function returns (or panics), so wait for the counts
	// themselves, not only the runs (#115). A run that is never counted times this out.
	eventually(t, "three runs of each job, each counted under its label", func() bool {
		return ok.Load() >= 3 && bad.Load() >= 3 && boom.Load() >= 3 &&
			testutil.ToFloat64(r.total.WithLabelValues("ok", "ok")) >= 3 &&
			testutil.ToFloat64(r.total.WithLabelValues("bad", "error")) >= 3 &&
			testutil.ToFloat64(r.total.WithLabelValues("panics", "error")) >= 3 &&
			testutil.ToFloat64(r.lastSuccess.WithLabelValues("ok")) > 0
	})
	if v := testutil.ToFloat64(r.total.WithLabelValues("boom", "error")); v != 0 {
		t.Errorf("a labelled job was counted under its name: %v", v)
	}
	if got := r.Names(); len(got) != 3 {
		t.Errorf("running = %v", got)
	}
}

func TestStartBeforeRunStopAndReplace(t *testing.T) {
	r := New(slog.New(slog.NewTextHandler(io.Discard, nil)), prometheus.NewRegistry())
	var first, second atomic.Int32
	r.Start(Job{Name: "j", Every: time.Hour, Immediate: true, Run: func(context.Context) error { first.Add(1); return nil }})
	r.Start(Job{Name: "j", Every: 5 * time.Millisecond, Immediate: true, Run: func(context.Context) error { second.Add(1); return nil }})
	if first.Load()+second.Load() != 0 {
		t.Fatal("a job ran before Run")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	eventually(t, "the replacing job", func() bool { return second.Load() >= 2 })
	if first.Load() != 0 {
		t.Error("the replaced job ran")
	}
	r.Stop("j")
	n := second.Load()
	time.Sleep(30 * time.Millisecond)
	if second.Load() != n || len(r.Names()) != 0 {
		t.Error("a stopped job ran again")
	}
	r.Stop("never-started") // ignored
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
}

// Run returns only after every job has stopped, so nothing uses the store after the hub closes it.
func TestRunWaitsForJobs(t *testing.T) {
	r := New(slog.New(slog.NewTextHandler(io.Discard, nil)), prometheus.NewRegistry())
	var finished atomic.Bool
	started := make(chan struct{})
	r.Start(Job{Name: "slow", Every: time.Hour, Immediate: true, Run: func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond)
		finished.Store(true)
		return ctx.Err()
	}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	<-started
	cancel()
	<-done
	if !finished.Load() {
		t.Error("Run returned while a job was still running")
	}
}

func TestBackoff(t *testing.T) {
	for _, c := range []struct {
		every, max time.Duration
		failures   int
		want       time.Duration
	}{
		{15 * time.Second, 0, 5, 15 * time.Second},
		{15 * time.Second, 2 * time.Minute, 1, 15 * time.Second},
		{15 * time.Second, 2 * time.Minute, 2, 30 * time.Second},
		{15 * time.Second, 2 * time.Minute, 4, 2 * time.Minute},
		{15 * time.Second, 2 * time.Minute, 50, 2 * time.Minute},
	} {
		if got := Backoff(c.every, c.max, c.failures); got != c.want {
			t.Errorf("Backoff(%s, %s, %d) = %s, want %s", c.every, c.max, c.failures, got, c.want)
		}
	}
}

func TestStartRejectsAnIncompleteJob(t *testing.T) {
	r := New(slog.New(slog.NewTextHandler(io.Discard, nil)), prometheus.NewRegistry())
	defer func() {
		if recover() == nil {
			t.Error("a job without an interval was accepted")
		}
	}()
	r.Start(Job{Name: "x", Run: func(context.Context) error { return nil }})
}

// A job that starts another while the runner shuts down (the reconcile job does) launches
// nothing, so Run's wait is never joined late.
func TestStartDuringShutdownIsIgnored(t *testing.T) {
	r := New(slog.New(slog.NewTextHandler(io.Discard, nil)), prometheus.NewRegistry())
	started := make(chan struct{})
	var late atomic.Int32
	r.Start(Job{Name: "starter", Every: time.Hour, Immediate: true, Run: func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		time.Sleep(10 * time.Millisecond) // Run is now waiting
		r.Start(Job{Name: "late", Every: time.Millisecond, Immediate: true, Run: func(context.Context) error { late.Add(1); return nil }})
		return nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	<-started
	cancel()
	<-done
	time.Sleep(20 * time.Millisecond)
	if late.Load() != 0 || len(r.Names()) != 0 {
		t.Errorf("a job started during shutdown ran %d times", late.Load())
	}
}
