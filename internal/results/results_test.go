package results_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/gravel-project/gravel/internal/objectstore/objectstoretest"
	"github.com/gravel-project/gravel/internal/results"
	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

type leases struct {
	mu    sync.Mutex
	id    uuid.UUID
	ended []string
}

func (l *leases) LiveLease(string) (uuid.UUID, bool) { return l.id, l.id != uuid.Nil }
func (l *leases) MatchEnded(_ context.Context, server string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ended = append(l.ended, server)
}

func event(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "games", "cs2", "matchzy", "testdata", "events", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type rig struct {
	st      *store.Store
	org     uuid.UUID
	svc     *results.Service
	leases  *leases
	objects *objectstoretest.Memory
	reg     *prometheus.Registry
}

func newRig(t *testing.T, withDemos bool) *rig {
	t.Helper()
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o, err := st.CreateBuiltinOrganization(ctx, uuid.New(), "HTG")
	if err != nil {
		t.Fatal(err)
	}
	srv := store.ManagedServer{ID: "cs2-1", GameID: "cs2", Name: "CS2", Driver: "cloudvm", Location: "do-sfo3", CredentialFile: "/run/secrets/rcon",
		PollInterval: 15 * time.Second, Trust: "official", Feed: json.RawMessage(`{"url": "https://ingest.example.com", "token_file": "/run/secrets/feed"}`),
		VM: json.RawMessage(`{"size": "c-2", "volume_gib": 100}`), Settings: json.RawMessage(`{}`)}
	if err := st.ApplyServers(ctx, o.ID, []store.Game{{ID: "cs2"}}, nil, []store.ManagedServer{srv}, time.Now()); err != nil {
		t.Fatal(err)
	}
	r := &rig{st: st, org: o.ID, leases: &leases{}, reg: prometheus.NewRegistry()}
	var objects *objectstoretest.Memory
	if withDemos {
		objects = objectstoretest.NewMemory()
		r.objects = objects
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if objects != nil {
		r.svc = results.New(st, o.ID, r.leases, func(string) string { return "de_nuke" }, objects, 64<<20, r.reg, logger)
	} else {
		r.svc = results.New(st, o.ID, r.leases, func(string) string { return "de_nuke" }, nil, 64<<20, r.reg, logger)
	}
	return r
}

func (r *rig) count(t *testing.T, metric, event, result string) float64 {
	t.Helper()
	mfs, _ := r.reg.Gather()
	for _, mf := range mfs {
		if mf.GetName() != metric {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if (event == "" || labels["event"] == event) && labels["result"] == result {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// A series from MatchZy's events: started, a map live, its running totals, its end, the series'
// end, which ends the lease's match. Repeats change nothing.
func TestASeriesFromItsEvents(t *testing.T) {
	r := newRig(t, false)
	ctx := context.Background()
	r.leases.id = uuid.New()
	if err := r.st.CreateServerLease(ctx, store.ServerLease{ID: r.leases.id, OrganizationID: r.org, ServerID: "cs2-1", HolderKind: store.LeaseHolderApp,
		Holder: "app", HolderRef: "s", State: store.LeaseReady, CreatedAt: time.Now(), MaxUntil: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"series_start", "going_live", "round_end", "round_end", "map_result", "series_end", "player_death"} {
		r.svc.Events(ctx, "cs2-1", event(t, name))
	}
	rs, err := r.svc.List(ctx, store.MatchResultFilter{})
	if err != nil || len(rs) != 1 {
		t.Fatalf("results %v %v", rs, err)
	}
	got, err := r.svc.Get(ctx, rs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExternalID != 27 || got.Team1Name == "" || got.EndedAt == nil || got.Winner == "" || got.LeaseID == nil || *got.LeaseID != r.leases.id {
		t.Errorf("result %+v", got)
	}
	if len(got.Maps) != 1 || got.Maps[0].MapName != "de_nuke" || got.Maps[0].EndedAt == nil || got.Maps[0].StartedAt == nil {
		t.Errorf("maps %+v", got.Maps)
	}
	if len(got.Players) == 0 || got.Players[0].Provider != results.Provider || !strings.HasPrefix(got.Players[0].Subject, "7656119") {
		t.Errorf("players %+v", got.Players)
	}
	if len(r.leases.ended) != 1 || r.leases.ended[0] != "cs2-1" {
		t.Errorf("the lease's match ended %v", r.leases.ended)
	}
	if r.count(t, "gravel_match_events_total", "player_death", "ignored") != 1 || r.count(t, "gravel_match_events_total", "round_end", "recorded") != 2 {
		t.Error("the event counts")
	}
	if l, _ := r.svc.List(ctx, store.MatchResultFilter{LeaseID: &r.leases.id, EndedOnly: true}); len(l) != 1 {
		t.Errorf("by lease: %v", l)
	}
}

// An event whose series_start never came makes the series; bad JSON is counted, nothing more.
func TestMissedEvents(t *testing.T) {
	r := newRig(t, false)
	ctx := context.Background()
	r.svc.Events(ctx, "cs2-1", event(t, "map_result"))
	r.svc.Events(ctx, "cs2-1", []byte(`{"event": `))
	r.svc.Events(ctx, "cs2-1", []byte(`{"event":"series_start","matchid":"27"}`))
	if rs, _ := r.svc.List(ctx, store.MatchResultFilter{}); len(rs) != 1 || rs[0].ExternalID != 27 {
		t.Errorf("results %v", rs)
	}
	if r.count(t, "gravel_match_events_total", "unknown", "error") != 2 { // neither envelope reads
		t.Error("errors not counted")
	}
}

// A demo goes to the object store under its result and the map records it; a link to it works for
// a while; without a store the hub keeps none.
func TestDemos(t *testing.T) {
	r := newRig(t, true)
	ctx := context.Background()
	r.svc.Events(ctx, "cs2-1", event(t, "series_start"))
	h := http.Header{}
	h.Set("MatchZy-MatchId", "27")
	h.Set("MatchZy-MapNumber", "0")
	h.Set("MatchZy-FileName", "../2026-10-14_27_de nuke.dem")
	demo := bytes.Repeat([]byte("demo"), 1000)
	if err := r.svc.Demo(ctx, "cs2-1", h, bytes.NewReader(demo), int64(len(demo))); err != nil {
		t.Fatal(err)
	}
	rs, _ := r.svc.List(ctx, store.MatchResultFilter{})
	got, _ := r.svc.Get(ctx, rs[0].ID)
	if len(got.Maps) != 1 || got.Maps[0].DemoBytes != int64(len(demo)) || !strings.HasPrefix(got.Maps[0].DemoKey, "matches/"+got.ID.String()+"/0-") ||
		strings.Contains(got.Maps[0].DemoKey, "..") || strings.Contains(got.Maps[0].DemoKey, " ") {
		t.Fatalf("map %+v", got.Maps)
	}
	if b, ok := r.objects.Bytes(got.Maps[0].DemoKey); !ok || !bytes.Equal(b, demo) {
		t.Error("the demo isn't in the store")
	}
	if url, err := r.svc.DemoURL(ctx, got.ID, 0); err != nil || url == "" {
		t.Errorf("link: %q %v", url, err)
	}
	if _, err := r.svc.DemoURL(ctx, got.ID, 1); !errors.Is(err, results.ErrNoDemo) {
		t.Errorf("a map with no demo: %v", err)
	}
	h.Set("MatchZy-MapNumber", "x")
	if err := r.svc.Demo(ctx, "cs2-1", h, bytes.NewReader(demo), -1); err == nil {
		t.Error("a bad map number")
	}
	off := newRig(t, false)
	if off.svc.MaxDemoBytes() != 0 {
		t.Error("no store, yet demos are taken")
	}
}
