package servers

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/gravel-project/gravel/drivers"
	wardogsdriver "github.com/gravel-project/gravel/drivers/wardogs"
	"github.com/gravel-project/gravel/games/wardogs/wardogstest"
	"github.com/gravel-project/gravel/internal/jobs"
	"github.com/gravel-project/gravel/internal/servers/serverstest"
)

const fixture = "../../games/wardogs/testdata/CL-509546"

type monitorRig struct {
	svc  *Service
	mon  *Monitor
	run  *jobs.Runner
	fake *wardogstest.Server
	cred string
	m    Manifest
	logs *strings.Builder
}

func newMonitorRig(t *testing.T, opt wardogstest.Options) *monitorRig {
	t.Helper()
	logs := &strings.Builder{}
	logger := slog.New(slog.NewTextHandler(logWriter{logs}, nil))
	r := &monitorRig{fake: wardogstest.New(t, fixture, opt), logs: logs}
	r.cred = filepath.Join(t.TempDir(), "rcon")
	r.setCredential(t, wardogstest.Token)
	reg := prometheus.NewRegistry()
	registry := drivers.Registry{wardogsdriver.Name: wardogsdriver.New}
	r.svc = New(serverstest.NewFakeStore(), uuid.New(), registry.Names(), logger)
	r.run = jobs.New(logger, reg)
	r.mon = NewMonitor(r.svc, registry, r.run, reg, "test", logger)
	r.m = Manifest{
		Games: []GameRef{{ID: "wardogs"}},
		Servers: []Server{{ID: "wd-1", Name: "War Dogs #1", Game: "wardogs", Driver: "wardogs", Location: "slc",
			Endpoint: r.fake.URL, CredentialFile: r.cred, Trust: TrustOfficial}},
	}.Normalized()
	r.apply(t)
	return r
}

type logWriter struct{ b *strings.Builder }

func (w logWriter) Write(p []byte) (int, error) { return w.b.Write(p) }

func (r *monitorRig) setCredential(t *testing.T, v string) {
	t.Helper()
	if err := os.WriteFile(r.cred, []byte(v+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (r *monitorRig) apply(t *testing.T) {
	t.Helper()
	if _, err := r.svc.Apply(context.Background(), r.m, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.mon.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (r *monitorRig) poll(t *testing.T) (Observation, error) {
	t.Helper()
	err := r.mon.poll(context.Background(), "wd-1")
	o, _ := r.mon.Observation("wd-1")
	return o, err
}

func TestMonitorPollsThroughTheDriver(t *testing.T) {
	r := newMonitorRig(t, wardogstest.Options{Handle: map[string]http.HandlerFunc{
		"GET /v1/players": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"players":[{"name":"Player 1","steamId":"76561190000000001","faction":"Valkyra","kills":3,"deaths":1,"cash":10,"pingMs":40}],"count":1}`)
		},
	}})
	if o, ok := r.mon.Observation("wd-1"); !ok || o.State != StateUnknown || o.Reachable {
		t.Errorf("before the first poll = %+v, %v", o, ok)
	}
	if got := r.run.Names(); len(got) != 0 {
		t.Errorf("jobs running before Run: %v", got)
	}
	o, err := r.poll(t)
	if err != nil {
		t.Fatal(err)
	}
	if o.State != StateOK || !o.Reachable || o.ObservedAt.IsZero() || o.Build != "++Wardogs+Live-CL-509546" {
		t.Errorf("observation = %+v", o)
	}
	if !slices.Contains(o.Capabilities, drivers.CapKick) || !slices.Contains(o.Capabilities, drivers.CapConfigWrite) {
		t.Errorf("capabilities = %v", o.Capabilities)
	}
	if o.Status.Map != "Ozeti" || o.Status.MaxPlayers != 100 || len(o.Status.Teams) != 3 || o.Status.ScoreTick != 24 {
		t.Errorf("status = %+v", o.Status)
	}
	if len(o.Players) != 1 || o.Players[0].Identity != (drivers.Identity{Provider: "steam", Subject: "76561190000000001"}) || o.Players[0].Team != "Valkyra" {
		t.Errorf("players = %+v", o.Players)
	}
	if v := testutil.ToFloat64(r.mon.reachable.WithLabelValues("wd-1", "wardogs")); v != 1 {
		t.Errorf("reachable gauge = %v", v)
	}
	if v := testutil.ToFloat64(r.mon.maxPlayers.WithLabelValues("wd-1", "wardogs")); v != 100 {
		t.Errorf("max players gauge = %v", v)
	}
}

// A refused credential costs one strike, then nothing protected is sent until the file changes;
// the hub picks up a rotation without a restart.
func TestMonitorCredentialRefusalAndRotation(t *testing.T) {
	r := newMonitorRig(t, wardogstest.Options{})
	if _, err := r.poll(t); err != nil {
		t.Fatal(err)
	}
	r.setCredential(t, "wrong")
	if err := r.mon.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		o, err := r.poll(t)
		if err == nil || o.State != StateCredentialRefused || !o.Reachable {
			t.Fatalf("refused: %+v, %v", o, err)
		}
		if strings.Contains(err.Error(), r.fake.URL) {
			t.Errorf("the error carries the endpoint: %v", err)
		}
	}
	if r.fake.Strikes() != 1 {
		t.Errorf("strikes = %d, want 1", r.fake.Strikes())
	}
	if c := strings.Count(r.logs.String(), "server poll failed"); c != 1 {
		t.Errorf("a lasting failure was logged %d times", c)
	}
	r.setCredential(t, wardogstest.Token)
	if err := r.mon.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if o, err := r.poll(t); err != nil || o.State != StateOK {
		t.Errorf("after the rotation: %+v, %v", o, err)
	}
	if !strings.Contains(r.logs.String(), "server credential changed") || strings.Contains(r.logs.String(), wardogstest.Token) {
		t.Error("the rotation was not logged, or the credential was")
	}
}

func TestMonitorMissingCredential(t *testing.T) {
	r := newMonitorRig(t, wardogstest.Options{})
	if err := os.Remove(r.cred); err != nil {
		t.Fatal(err)
	}
	r.m.Servers[0].Name = "changed" // a new definition: a new driver, built without a credential
	r.apply(t)
	o, err := r.poll(t)
	if err == nil || o.State != StateCredentialMissing || r.fake.Strikes() != 0 {
		t.Errorf("missing credential: %+v, %v, %d strikes", o, err, r.fake.Strikes())
	}
}

// Two failed polls in a row mark a server unreachable; the observation keeps the last good data.
func TestMonitorUnreachable(t *testing.T) {
	r := newMonitorRig(t, wardogstest.Options{})
	good, err := r.poll(t)
	if err != nil {
		t.Fatal(err)
	}
	r.fake.Close()
	o, err := r.poll(t)
	if err == nil || o.State != StateUnreachable || !o.Reachable {
		t.Errorf("one miss: %+v, %v", o, err)
	}
	o, _ = r.poll(t)
	if o.Reachable || o.ObservedAt != good.ObservedAt || o.Status.Map != "Ozeti" {
		t.Errorf("two misses: %+v", o)
	}
	if v := testutil.ToFloat64(r.mon.reachable.WithLabelValues("wd-1", "wardogs")); v != 0 {
		t.Errorf("reachable gauge = %v", v)
	}
}

func TestMonitorStopsARemovedServer(t *testing.T) {
	r := newMonitorRig(t, wardogstest.Options{})
	if _, err := r.poll(t); err != nil {
		t.Fatal(err)
	}
	r.m.Servers = nil
	r.apply(t)
	if _, ok := r.mon.Observation("wd-1"); ok {
		t.Error("a removed server is still observed")
	}
	if n := testutil.CollectAndCount(r.mon.reachable); n != 0 {
		t.Errorf("%d reachable series left", n)
	}
	if err := r.mon.poll(context.Background(), "wd-1"); err != nil {
		t.Errorf("a poll tick after removal = %v", err)
	}
}

func TestStateOf(t *testing.T) {
	for err, want := range map[error]string{
		drivers.ErrCredentialRefused: StateCredentialRefused,
		drivers.ErrCredentialMissing: StateCredentialMissing,
		drivers.ErrRateLimited:       StateRateLimited,
		context.DeadlineExceeded:     StateUnreachable,
		io.ErrUnexpectedEOF:          StateUnreachable,
		drivers.ErrNotSupported:      StateError,
	} {
		if got := stateOf(err); got != want {
			t.Errorf("stateOf(%v) = %s, want %s", err, got, want)
		}
	}
}
