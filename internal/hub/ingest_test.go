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

	"github.com/gravel-project/gravel/games/wardogs/wardogstest"
	"github.com/gravel-project/gravel/internal/hub"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/ingest"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

// A server's feed posts to the public listener with its own bearer, which the app middleware
// would refuse as an unknown app token: the ingest route takes it first, stores the batch and
// answers 200; an unknown token is 401, and only POST reaches the route.
func TestIngestRoute(t *testing.T) {
	cfg := testConfig(t)
	st := storetest.Open(t)
	storetest.Reset(t, st)
	t.Cleanup(func() { storetest.Reset(t, st) })
	ctx := context.Background()
	dir := t.TempDir()
	cred, feedToken := filepath.Join(dir, "rcon"), filepath.Join(dir, "feed")
	for f, v := range map[string]string{cred: wardogstest.Token, feedToken: "feed-token-for-wd-1\n"} {
		if err := os.WriteFile(f, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Applied before the hub starts, so its first feed refresh reads the token.
	o, err := org.New(st, cfg.Claim.TokenTTL, quiet()).CreateBuiltinIfAbsent(ctx, cfg.Organization.Name)
	if err != nil {
		t.Fatal(err)
	}
	fake := wardogstest.New(t, "../../games/wardogs/testdata/CL-509546", wardogstest.Options{})
	m := servers.Manifest{
		Games: []servers.GameRef{{ID: "wardogs"}},
		Servers: []servers.Server{{ID: "wd-1", Name: "War Dogs #1", Game: "wardogs", Driver: "wardogs", Location: "slc",
			Endpoint: fake.URL, CredentialFile: cred, Trust: servers.TrustOfficial,
			Feed: &servers.Feed{URL: "https://ingest.example.com", TokenFile: feedToken}}},
	}
	if _, err := servers.New(st, o.ID, hub.Drivers().Names(), quiet()).Apply(ctx, m, servers.ApplyOptions{CheckCredentials: true}); err != nil {
		t.Fatal(err)
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
	url := "http://" + h.PublicAddr().String() + ingest.Path
	post := func(method, token, body string) int {
		req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	batch := `[{"eventId":"e-1","killerSteamId":"76561190000000001","victimSteamId":"76561190000000002"}]`
	deadline := time.Now().Add(10 * time.Second)
	for code := post(http.MethodPost, "feed-token-for-wd-1", batch); code != http.StatusOK; code = post(http.MethodPost, "feed-token-for-wd-1", batch) {
		if code != http.StatusUnauthorized || time.Now().After(deadline) {
			t.Fatalf("the feed's batch: %d", code)
		}
		time.Sleep(20 * time.Millisecond) // the first feed refresh is still running
	}
	if code := post(http.MethodPost, "not-a-feed-token", batch); code != http.StatusUnauthorized {
		t.Errorf("an unknown token: %d", code)
	}
	if code := post(http.MethodGet, "feed-token-for-wd-1", ""); code == http.StatusOK {
		t.Error("GET reached the ingest route")
	}
	got, err := st.ListIngestBatches(ctx, o.ID, "wd-1", 0, 10)
	if err != nil || len(got) != 1 || string(got[0].Body) != batch || got[0].Source != "wardogs" {
		t.Fatalf("stored = %+v, %v", got, err)
	}

	resp, err := http.Get("http://" + h.InternalAddr().String() + "/metrics") //nolint:noctx // a test
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	for _, want := range []string{
		`gravel_ingest_batches_total{result="stored",server="wd-1",source="wardogs"} 1`,
		`gravel_ingest_batches_total{result="unauthorized",server="unknown",source="unknown"}`,
		`gravel_ingest_last_batch_timestamp_seconds{server="wd-1"}`,
		`gravel_hub_jobs_total{job="ingest_feeds",result="ok"}`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics lack %s", want)
		}
	}
}
