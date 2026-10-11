package hub_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/gravel-project/gravel/games/wardogs/wardogsim"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/demo"
	"github.com/gravel-project/gravel/internal/hub"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/store/storetest"
	"github.com/gravel-project/gravel/internal/web/templates"
)

// The demo hub (gravel#112), done when: a hub with demo: true, a seeded database and a simulator
// in servers.yaml shows full boards and match lists, live status that changes, and new matches
// recorded from the simulator's polls. A hub without demo: true refuses that database.
func TestDemoHub(t *testing.T) {
	cfg := testConfig(t)
	cfg.Demo = true
	cfg.Stats.PseudonymKey = "a test pseudonym key, at least 32 bytes long"
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	// Leave the database as TestHubEndToEnd expects to find it, unmigrated, whatever runs first.
	t.Cleanup(func() {
		if err := st.MigrateDownTo(ctx, 0); err != nil {
			t.Error(err)
		}
	})

	// A simulator that is always busy, with matches of a few seconds, so the test sees them end.
	profile := wardogsim.DefaultProfile()
	profile.Timezone = "UTC"
	profile.Population = slices.Repeat([]float64{30}, 24)
	profile.MatchMinutes = wardogsim.Range{Min: 0.12, Max: 0.15}
	profile.SessionMinutes = wardogsim.Range{Min: 60, Max: 120}
	profile.KillsPerMinute = 3
	profile.RestartHour = nil
	sim, err := wardogsim.New(wardogsim.Options{Token: "sim-token", Profile: profile, Seed: 1, Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	simSrv := httptest.NewServer(sim.Handler())
	t.Cleanup(simSrv.Close)
	simCtx, stopSim := context.WithCancel(ctx)
	t.Cleanup(stopSim)
	go func() { _ = sim.Run(simCtx) }()
	cred := filepath.Join(t.TempDir(), "rcon")
	if err := os.WriteFile(cred, []byte("sim-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The deployment's order: the organization and servers.yaml, the seed, the settings, serve.
	o, err := org.New(st, cfg.Claim.TokenTTL, quiet()).CreateBuiltinIfAbsent(ctx, cfg.Organization.Name)
	if err != nil {
		t.Fatal(err)
	}
	m := servers.Manifest{Games: []servers.GameRef{{ID: "wardogs"}}, Servers: []servers.Server{{ID: "demo-1", Name: "Demo #1", Game: "wardogs",
		Driver: "wardogs", Location: "sim", Endpoint: simSrv.URL, CredentialFile: cred, PollInterval: servers.Duration(5 * time.Second), Trust: servers.TrustOfficial}}}
	if _, err := servers.New(st, o.ID, hub.Drivers().Names(), quiet()).Apply(ctx, m, servers.ApplyOptions{CheckCredentials: true}); err != nil {
		t.Fatal(err)
	}
	seededAt := time.Now()
	sum, _, err := demo.Seed(ctx, st, o.ID, cfg.Demo, demo.Options{Days: 7, Members: 12, Seed: 1, Now: seededAt})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := org.New(st, cfg.Claim.TokenTTL, quiet()).UpdateSettings(ctx, org.Settings{Stats: org.Stats{Public: true}}); err != nil {
		t.Fatal(err)
	}

	// Without demo: true the hub refuses the seeded database.
	plain := cfg
	plain.Demo = false
	if _, err := hub.New(ctx, hub.Options{Config: plain, Logger: quiet(), Version: "test", Providers: []identity.Registration{}}); err == nil || !strings.Contains(err.Error(), "demo: true") {
		t.Fatalf("a hub without demo: true on a seeded database: %v", err)
	}

	h, err := hub.New(ctx, hub.Options{Config: cfg, Logger: quiet(), Version: "test", Providers: []identity.Registration{}})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- h.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	<-h.Started()
	base := "http://" + h.PublicAddr().String()

	got, err := hubv1connect.NewOrganizationServiceClient(http.DefaultClient, base).GetOrganization(ctx, connect.NewRequest(&hubv1.GetOrganizationRequest{}))
	if err != nil || !got.Msg.GetOrganization().GetDemo() {
		t.Errorf("the API does not say the hub is a demo: %v %v", got, err)
	}
	for _, page := range []string{"/servers", "/boards", "/servers/demo-1", "/login", "/servers/no-such-server"} {
		resp, err := http.Get(base + page) //nolint:noctx // a test
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if !strings.Contains(string(body), templates.DemoNotice) {
			t.Errorf("%s (%d) has no demo banner", page, resp.StatusCode)
		}
	}

	// Full boards: members by name where they chose so, everyone else under a pseudonym.
	stats := hubv1connect.NewStatsServiceClient(http.DefaultClient, base)
	b, err := stats.GetBoard(ctx, connect.NewRequest(&hubv1.GetBoardRequest{ServerId: "demo-1", PageSize: 100}))
	if err != nil {
		t.Fatal(err)
	}
	named, pseudonymous := 0, 0
	for _, e := range b.Msg.GetEntries() {
		switch {
		case e.GetPseudonymous():
			pseudonymous++
		case e.GetUserId() != "":
			named++
		}
	}
	if len(b.Msg.GetEntries()) < 50 || named == 0 || pseudonymous < named {
		t.Errorf("board: %d entries, %d named members, %d pseudonymous", len(b.Msg.GetEntries()), named, pseudonymous)
	}
	ms, err := stats.ListMatches(ctx, connect.NewRequest(&hubv1.ListMatchesRequest{ServerId: "demo-1"}))
	if err != nil || len(ms.Msg.GetMatches()) == 0 || sum.Matches["demo-1"] < 50 {
		t.Fatalf("matches: %v %v (seeded %d)", ms, err, sum.Matches["demo-1"])
	}

	// Live: the status moves and the simulator's matches are recorded beside the seeded ones.
	serverAPI := hubv1connect.NewServerServiceClient(http.DefaultClient, base)
	status := func() *hubv1.ServerStatus {
		got, err := serverAPI.GetServerStatus(ctx, connect.NewRequest(&hubv1.GetServerStatusRequest{ServerId: "demo-1"}))
		if err != nil {
			t.Fatal(err)
		}
		return got.Msg.GetServer().GetStatus()
	}
	deadline := time.Now().Add(45 * time.Second)
	var first *hubv1.ServerStatus
	live, changed := 0, false
	for live < 2 || !changed {
		if time.Now().After(deadline) {
			t.Fatalf("after 45 s: %d live matches recorded, status changed %v", live, changed)
		}
		time.Sleep(500 * time.Millisecond)
		s := status()
		if s.GetState() != servers.StateOK && s.GetState() != "" {
			continue
		}
		if s.GetPlayers() == 0 {
			continue
		}
		if first == nil {
			first = s
		} else if s.GetRotationIndex() != first.GetRotationIndex() || teamScore(s) != teamScore(first) {
			changed = true
		}
		ms, err := stats.ListMatches(ctx, connect.NewRequest(&hubv1.ListMatchesRequest{ServerId: "demo-1"}))
		if err != nil {
			t.Fatal(err)
		}
		live = 0
		for _, m := range ms.Msg.GetMatches() {
			if m.GetStartedAt().AsTime().After(seededAt) {
				live++
			}
		}
	}
}

func teamScore(s *hubv1.ServerStatus) int64 {
	var n int64
	for _, t := range s.GetTeams() {
		n += t.GetScore()
	}
	return n
}
