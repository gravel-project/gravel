package hub_test

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/gravel-project/gravel/games/wardogs/wardogstest"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/hub"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

// A server applied to the database is polled by the running hub through its driver, and the API
// and the metrics answer from the observation; nothing reaches the game server but the poll.
func TestHubPollsAppliedServers(t *testing.T) {
	cfg := testConfig(t)
	st := storetest.Open(t)
	storetest.Reset(t, st)
	t.Cleanup(func() { storetest.Reset(t, st) })
	fake := wardogstest.New(t, "../../games/wardogs/testdata/CL-509546", wardogstest.Options{})
	cred := filepath.Join(t.TempDir(), "rcon")
	if err := os.WriteFile(cred, []byte(wardogstest.Token), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	h, err := hub.New(ctx, hub.Options{Config: cfg, Logger: quiet(), Version: "test", Providers: []identity.Registration{}})
	if err != nil {
		t.Fatal(err)
	}
	o, err := st.GetBuiltinOrganization(ctx)
	if err != nil {
		t.Fatal(err)
	}
	m := servers.Manifest{
		Games: []servers.GameRef{{ID: "wardogs"}},
		Servers: []servers.Server{{ID: "wd-1", Name: "War Dogs #1", Game: "wardogs", Driver: "wardogs", Location: "slc",
			Endpoint: fake.URL, CredentialFile: cred, Trust: servers.TrustOfficial}},
	}
	if _, err := servers.New(st, o.ID, hub.Drivers().Names(), quiet()).Apply(ctx, m, servers.ApplyOptions{CheckCredentials: true}); err != nil {
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
	client := hubv1connect.NewServerServiceClient(http.DefaultClient, base)

	var srv *hubv1.Server
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := client.GetServerStatus(ctx, connect.NewRequest(&hubv1.GetServerStatusRequest{ServerId: "wd-1"}))
		if err != nil {
			t.Fatal(err)
		}
		if srv = got.Msg.GetServer(); srv.GetStatus().GetState() == servers.StateOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the hub never polled the server: %v", srv)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st := srv.GetStatus(); !st.GetReachable() || st.GetMap() != "Ozeti" || st.GetBuild() != "++Wardogs+Live-CL-509546" || st.GetMaxPlayers() != 100 {
		t.Errorf("status = %v", st)
	}
	if fake.Strikes() != 0 {
		t.Errorf("strikes = %d", fake.Strikes())
	}

	resp, err := http.Get("http://" + h.InternalAddr().String() + "/metrics") //nolint:noctx // a test
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	for _, want := range []string{
		`gravel_server_reachable{game="wardogs",server="wd-1"} 1`,
		`gravel_server_max_players{game="wardogs",server="wd-1"} 100`,
		`gravel_hub_jobs_total{job="server_poll",result="ok"}`,
		`gravel_hub_jobs_total{job="servers_reconcile",result="ok"}`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics lack %s", want)
		}
	}
	for _, name := range []string{"gravel_hub_jobs_total", "gravel_hub_job_last_success_timestamp_seconds", "gravel_server_players", "gravel_server_last_observed_timestamp_seconds"} {
		found := false
		for _, n := range h.MetricNames() {
			found = found || n == name
		}
		if !found {
			t.Errorf("MetricNames lacks %s", name)
		}
	}
}
