package hub_test

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/gravel-project/gravel/games/wardogs/wardogstest"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/hub"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

// The hub's polls of a server with players on become a match and its rows (ADR-0012); the board
// is the owner's until the settings make stats public, and then shows players under pseudonyms.
func TestStatsFromPolls(t *testing.T) {
	cfg := testConfig(t)
	cfg.Stats.PseudonymKey = "a test pseudonym key, at least 32 bytes long"
	st := storetest.Open(t)
	storetest.Reset(t, st)
	t.Cleanup(func() { storetest.Reset(t, st) })
	var kills atomic.Int32
	fake := wardogstest.New(t, "../../games/wardogs/testdata/CL-509546", wardogstest.Options{Handle: map[string]http.HandlerFunc{
		"GET /v1/players": func(w http.ResponseWriter, _ *http.Request) {
			k := kills.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"players":[{"name":"Ana","steamId":"76561190000000001","faction":"Lonestar","kills":`+itoa(k)+`,"deaths":1,"cash":0,"pingMs":40},`+
				`{"name":"Ben","steamId":"76561190000000002","faction":"Valkyra","kills":0,"deaths":`+itoa(k)+`,"cash":0,"pingMs":60}],"count":2}`)
		},
	}})
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
	m := servers.Manifest{Games: []servers.GameRef{{ID: "wardogs"}}, Servers: []servers.Server{{ID: "wd-1", Name: "War Dogs #1", Game: "wardogs",
		Driver: "wardogs", Location: "slc", Endpoint: fake.URL, CredentialFile: cred, PollInterval: servers.Duration(5 * time.Second), Trust: servers.TrustOfficial}}}
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
	client := hubv1connect.NewStatsServiceClient(http.DefaultClient, base)

	// Two polls: the match and its rows exist.
	deadline := time.Now().Add(30 * time.Second)
	for {
		if ms, err := st.ListMatches(ctx, o.ID, "wd-1", 0, 10); err == nil && len(ms) == 1 && ms[0].Kills >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no match recorded from the polls")
		}
		time.Sleep(100 * time.Millisecond)
	}

	if _, err := client.GetBoard(ctx, connect.NewRequest(&hubv1.GetBoardRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("a private board, anonymously: %v", err)
	}
	if _, err := org.New(st, cfg.Claim.TokenTTL, quiet()).UpdateSettings(ctx, org.Settings{Stats: org.Stats{Public: true}}); err != nil {
		t.Fatal(err)
	}
	b, err := client.GetBoard(ctx, connect.NewRequest(&hubv1.GetBoardRequest{ServerId: "wd-1"}))
	if err != nil {
		t.Fatal(err)
	}
	es := b.Msg.GetEntries()
	if len(es) != 2 || es[0].GetKills() < 2 || !es[0].GetPseudonymous() || es[0].GetUserId() != "" || strings.Contains(es[0].GetName(), "Ana") || len(strings.Fields(es[0].GetName())) != 2 {
		t.Errorf("public board: %v", es)
	}
	ms, err := client.ListMatches(ctx, connect.NewRequest(&hubv1.ListMatchesRequest{ServerId: "wd-1"}))
	if err != nil || len(ms.Msg.GetMatches()) != 1 || ms.Msg.GetMatches()[0].GetPlayers() != 2 || ms.Msg.GetMatches()[0].GetMap() != "Ozeti" {
		t.Errorf("matches: %v %v", ms, err)
	}
	if _, err := client.GetBoard(ctx, connect.NewRequest(&hubv1.GetBoardRequest{Metric: "headshots"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a bad metric: %v", err)
	}
	if _, err := client.SetBoardName(ctx, connect.NewRequest(&hubv1.SetBoardNameRequest{ShowName: true})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("setting a board name needs a member: %v", err)
	}

	resp, err := http.Get("http://" + h.InternalAddr().String() + "/metrics") //nolint:noctx // a test
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	for _, want := range []string{`gravel_stats_matches_total{game="wardogs",server="wd-1"} 1`, `gravel_stats_rows_written_total{server="wd-1"}`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics lack %s", want)
		}
	}
}

func itoa(n int32) string { return strconv.Itoa(int(n)) }
