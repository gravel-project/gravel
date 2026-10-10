package wardogs_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gravel-project/gravel/drivers"
	"github.com/gravel-project/gravel/drivers/wardogs"
	"github.com/gravel-project/gravel/games/wardogs/wardogstest"
)

const fixture = "../../games/wardogs/testdata/CL-509546"

func newDriver(t *testing.T, srv *wardogstest.Server, cred string) drivers.ExternalReachable {
	t.Helper()
	d, err := wardogs.New(drivers.Target{Endpoint: srv.URL, Credential: cred, UserAgent: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDriverOnTheRecordedBuild(t *testing.T) {
	srv := wardogstest.New(t, fixture, wardogstest.Options{Handle: map[string]http.HandlerFunc{
		"GET /v1/players": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"players":[{"name":"P","steamId":76561190000000001,"faction":"Manticore","kills":1,"deaths":2,"cash":3,"pingMs":4}],"count":1}`)
		},
	}})
	d := newDriver(t, srv, wardogstest.Token)
	ctx := context.Background()
	build, err := d.Build(ctx)
	if err != nil || build != "++Wardogs+Live-CL-509546" {
		t.Fatalf("build = %q, %v", build, err)
	}
	caps, err := d.Capabilities(ctx)
	want := []string{drivers.CapBan, drivers.CapBans, drivers.CapBroadcast, drivers.CapConfigRead, drivers.CapConfigWrite, drivers.CapKick,
		drivers.CapKill, drivers.CapMessage, drivers.CapMovePlayer, drivers.CapPlayers, drivers.CapRotation, drivers.CapStatus, drivers.CapUnban}
	slices.Sort(want)
	if err != nil || !slices.Equal(caps, want) {
		t.Errorf("capabilities = %v, %v", caps, err)
	}
	st, err := d.Status(ctx)
	if err != nil || st.Name != "HTG WARDOGS | NA WEST | #1" || st.Map != "Ozeti" || st.MaxPlayers != 100 || st.ScoreTick != 24 ||
		len(st.Teams) != 3 || st.Teams[0] != (drivers.TeamScore{Name: "Lonestar", Color: "#4CB1EF"}) || st.NextRotationIndex != 1 {
		t.Errorf("status = %+v, %v", st, err)
	}
	p, err := d.Players(ctx)
	if err != nil || len(p) != 1 || p[0] != (drivers.Player{Name: "P", Identity: drivers.Identity{Provider: "steam", Subject: "76561190000000001"}, Team: "Manticore", Kills: 1, Deaths: 2, PingMs: 4}) {
		t.Errorf("players = %+v, %v", p, err)
	}
}

// A build without the config routes and with the document read-only loses config_write, and a
// call on a missing route is ErrNotSupported.
func TestDriverCapabilitiesFollowTheServer(t *testing.T) {
	srv := wardogstest.New(t, fixture, wardogstest.Options{
		Remove:   []string{"GET /v1/players", "POST /v1/config/validate"},
		ReadOnly: true,
	})
	d := newDriver(t, srv, wardogstest.Token)
	caps, err := d.Capabilities(context.Background())
	if err != nil || slices.Contains(caps, drivers.CapPlayers) || slices.Contains(caps, drivers.CapConfigWrite) || !slices.Contains(caps, drivers.CapConfigRead) {
		t.Errorf("capabilities = %v, %v", caps, err)
	}
	if _, err := d.Players(context.Background()); !errors.Is(err, drivers.ErrNotSupported) {
		t.Errorf("players on a build without them = %v", err)
	}
}

func TestDriverErrors(t *testing.T) {
	srv := wardogstest.New(t, fixture, wardogstest.Options{})
	ctx := context.Background()
	if _, err := newDriver(t, srv, "").Status(ctx); !errors.Is(err, drivers.ErrCredentialMissing) {
		t.Errorf("no credential = %v", err)
	}
	d := newDriver(t, srv, "wrong")
	if _, err := d.Status(ctx); !errors.Is(err, drivers.ErrCredentialRefused) {
		t.Errorf("wrong credential = %v", err)
	}
	if _, err := d.Status(ctx); !errors.Is(err, drivers.ErrCredentialRefused) || srv.Strikes() != 1 {
		t.Errorf("again = %v, %d strikes", err, srv.Strikes())
	}
	d.SetCredential(wardogstest.Token)
	if _, err := d.Status(ctx); err != nil {
		t.Errorf("after SetCredential = %v", err)
	}
	limited := wardogstest.New(t, fixture, wardogstest.Options{Handle: map[string]http.HandlerFunc{
		"GET /v1/status": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "5")
			w.WriteHeader(http.StatusTooManyRequests)
		},
	}})
	if _, err := newDriver(t, limited, wardogstest.Token).Status(ctx); !errors.Is(err, drivers.ErrRateLimited) {
		t.Errorf("429 = %v", err)
	}
	if _, err := wardogs.New(drivers.Target{Endpoint: "not a url"}); err == nil {
		t.Error("a bad endpoint built a driver")
	}
}

// rotationReads counts the GET /v1/rotation requests the server saw.
func rotationReads(srv *wardogstest.Server) int {
	n := 0
	for _, r := range srv.Requests() {
		if r.Method == http.MethodGet && r.Path == "/v1/rotation" {
			n++
		}
	}
	return n
}

// The status names the rotation's maps (the theatres, where Map is the level), read at most once
// a minute: the status gives only the positions (#97).
func TestStatusNamesTheRotationsMaps(t *testing.T) {
	var failing atomic.Bool
	srv := wardogstest.New(t, fixture, wardogstest.Options{Handle: map[string]http.HandlerFunc{
		"GET /v1/rotation": func(w http.ResponseWriter, _ *http.Request) {
			if failing.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = io.WriteString(w, `{"enabled":true,"mode":"ordered","count":3,"entries":[{"index":0,"map":"Europe","status":"now"},{"index":1,"map":"NorthAmerica","status":"next"},{"index":2,"map":"Kavkazi"}]}`)
		},
	}})
	d := newDriver(t, srv, wardogstest.Token)
	now := time.Date(2026, 10, 10, 20, 0, 0, 0, time.UTC)
	wardogs.SetNow(d, func() time.Time { return now })
	ctx := context.Background()

	st, err := d.Status(ctx)
	if err != nil || st.Map != "Ozeti" || st.RotationMap != "Europe" || st.NextRotationMap != "NorthAmerica" {
		t.Fatalf("status = %+v, %v", st, err)
	}
	now = now.Add(wardogs.RotationTTL - time.Second)
	if _, err := d.Status(ctx); err != nil || rotationReads(srv) != 1 {
		t.Errorf("within the minute the rotation is not read again: %d reads, %v", rotationReads(srv), err)
	}
	now = now.Add(time.Second)
	if _, err := d.Status(ctx); err != nil || rotationReads(srv) != 2 {
		t.Errorf("after the minute it is: %d reads, %v", rotationReads(srv), err)
	}

	// A failed read never fails the status; the last good names stay until a read works.
	failing.Store(true)
	now = now.Add(wardogs.RotationTTL)
	st, err = d.Status(ctx)
	if err != nil || st.RotationMap != "Europe" || st.NextRotationMap != "NorthAmerica" || rotationReads(srv) != 3 {
		t.Errorf("a failed read: %+v, %v, %d reads", st, err, rotationReads(srv))
	}
	// …and it waits the minute before trying again.
	if _, err := d.Status(ctx); err != nil || rotationReads(srv) != 3 {
		t.Errorf("a failure is not retried at once: %d reads", rotationReads(srv))
	}

	// A first read that fails: the names are empty, the status is whole.
	fresh := newDriver(t, srv, wardogstest.Token)
	if st, err := fresh.Status(ctx); err != nil || st.RotationMap != "" || st.Name == "" {
		t.Errorf("no rotation yet: %+v, %v", st, err)
	}
}

// A build that doesn't serve the rotation: no request, no names, the status as before.
func TestStatusWithoutTheRotationRoute(t *testing.T) {
	srv := wardogstest.New(t, fixture, wardogstest.Options{Remove: []string{"GET /v1/rotation"}})
	d := newDriver(t, srv, wardogstest.Token)
	ctx := context.Background()
	if _, err := d.Capabilities(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := d.Status(ctx)
	if err != nil || st.Map != "Ozeti" || st.RotationMap != "" || st.NextRotationMap != "" || rotationReads(srv) != 0 {
		t.Errorf("status = %+v, %v, %d rotation reads", st, err, rotationReads(srv))
	}
}
