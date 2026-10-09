package wardogs_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gravel-project/gravel/games/wardogs"
	"github.com/gravel-project/gravel/games/wardogs/wardogstest"
)

// latest is the newest recorded build's directory.
func latest(t *testing.T) string {
	t.Helper()
	builds := wardogstest.Builds(t, "testdata")
	if len(builds) == 0 {
		t.Fatal("no recorded build under testdata/")
	}
	return builds[len(builds)-1]
}

func recordedCaps(t *testing.T) wardogs.Capabilities {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(latest(t), "v1", "capabilities.json"))
	if err != nil {
		t.Fatal(err)
	}
	var caps wardogs.Capabilities
	if err := json.Unmarshal(data, &caps); err != nil {
		t.Fatal(err)
	}
	return caps
}

func TestParseRoute(t *testing.T) {
	r, err := wardogs.ParseRoute("post /v1/players/{steamId}/kick")
	if err != nil {
		t.Fatal(err)
	}
	if r.String() != "POST /v1/players/{steamId}/kick" || r.Shape() != "POST /v1/players/{}/kick" {
		t.Errorf("route = %q, shape %q", r, r.Shape())
	}
	if got := r.Params(); !slices.Equal(got, []string{"steamId"}) {
		t.Errorf("params = %v", got)
	}
	p, err := r.Path("7656 1/x")
	if err != nil || p != "/v1/players/7656%201%2Fx/kick" {
		t.Errorf("path = %q, %v", p, err)
	}
	if _, err := r.Path(); err == nil {
		t.Error("a missing parameter filled the path")
	}
	if _, err := r.Path(""); err == nil {
		t.Error("an empty parameter filled the path")
	}
	for _, bad := range []string{"", "GET", "/v1/status", "GET v1/status", "GET /v1//status", "GET /v1/status?x=1", "GET /v1/a b"} {
		if _, err := wardogs.ParseRoute(bad); err == nil {
			t.Errorf("ParseRoute(%q) passed", bad)
		}
	}
}

func TestCapabilitiesOfTheRecordedBuild(t *testing.T) {
	caps := recordedCaps(t)
	if missing := caps.Missing(); len(missing) != 0 {
		t.Errorf("the recorded build lacks %v; every known capability was written against it", missing)
	}
	if extra := caps.Unrecognised(); len(extra) != 0 {
		t.Errorf("the recorded build has routes the client has no capability for: %v", extra)
	}
	if len(caps.Set()) != len(wardogs.Known()) {
		t.Errorf("set has %d of %d", len(caps.Set()), len(wardogs.Known()))
	}
}

func TestCapabilitiesMatchByShape(t *testing.T) {
	caps := recordedCaps(t)

	// A renamed parameter is the same route, and RouteOf hands back the server's spelling.
	renamed := caps
	renamed.Routes = replace(caps.Routes, "POST /v1/players/{id}/kick", "POST /v1/players/{steamId}/kick")
	r, ok := renamed.RouteOf(wardogs.CapKick)
	if !ok || r.String() != "POST /v1/players/{steamId}/kick" {
		t.Errorf("renamed kick = %v, %v", r, ok)
	}

	// A removed route is an absent capability, and nothing else changes.
	removed := caps
	removed.Routes = replace(caps.Routes, "GET /v1/server-id", "")
	if removed.Supports(wardogs.CapServerID) || !slices.Equal(removed.Missing(), []wardogs.Capability{wardogs.CapServerID}) {
		t.Errorf("missing = %v", removed.Missing())
	}

	// A different literal segment is a different route.
	moved := caps
	moved.Routes = replace(caps.Routes, "GET /v1/status", "GET /v2/status")
	if moved.Supports(wardogs.CapStatus) {
		t.Error("GET /v2/status counted as GET /v1/status")
	}
	if got := moved.Unrecognised(); !slices.Equal(got, []string{"GET /v2/status"}) {
		t.Errorf("unrecognised = %v", got)
	}

	// Writing the document also needs it writable.
	readOnly := caps
	readOnly.Config.Writable = false
	if readOnly.Supports(wardogs.CapConfigWrite) || !readOnly.Supports(wardogs.CapConfigValidate) {
		t.Error("a read-only document still offers writes, or lost validate")
	}

	if caps.Supports("no-such-capability") {
		t.Error("an unknown capability is supported")
	}
}

func TestDiffRoutes(t *testing.T) {
	older := recordedCaps(t)
	newer := older
	newer.Routes = replace(older.Routes, "DELETE /v1/bans/{steamId}", "DELETE /v1/bans/{id}")
	newer.Routes = replace(newer.Routes, "PUT /v1/world/lighting", "")
	newer.Routes = append(newer.Routes, "GET /v1/feed")
	d := wardogs.DiffRoutes(older, newer)
	if !slices.Equal(d.Added, []string{"GET /v1/feed"}) ||
		!slices.Equal(d.Removed, []string{"PUT /v1/world/lighting"}) ||
		!slices.Equal(d.Renamed, []string{"DELETE /v1/bans/{steamId} -> DELETE /v1/bans/{id}"}) {
		t.Errorf("diff = %+v", d)
	}
	if !wardogs.DiffRoutes(older, older).Empty() {
		t.Error("a build differs from itself")
	}
}

// replace swaps one route for another ("" drops it).
func replace(routes []string, from, to string) []string {
	var out []string
	for _, r := range routes {
		switch {
		case r != from:
			out = append(out, r)
		case to != "":
			out = append(out, to)
		}
	}
	return out
}
