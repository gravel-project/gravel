package hub_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/config"
	"github.com/gravel-project/gravel/internal/hub"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Organization.Name = "Hidden Token Gaming"
	cfg.Database.URL = storetest.DatabaseURL(t)
	cfg.Database.ConnectTimeout = 10 * time.Second
	cfg.Server.Listen = "127.0.0.1:0"
	cfg.Server.InternalListen = "127.0.0.1:0"
	cfg.Server.ShutdownTimeout = 5 * time.Second
	return cfg
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	_ = resp.Body.Close()
	return resp.StatusCode, out
}

func TestHubEndToEnd(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	cfg.Database.Migrate = config.MigrateManual
	if _, err := hub.New(ctx, hub.Options{Config: cfg, Logger: quiet()}); err == nil || !strings.Contains(err.Error(), "not current") {
		t.Fatalf("manual migrate on an empty database must refuse to start: %v", err)
	}

	cfg.Database.Migrate = config.MigrateAuto
	h, err := hub.New(ctx, hub.Options{Config: cfg, Logger: quiet(), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	token := h.OwnerClaimToken()
	if token == "" {
		t.Fatal("a fresh hub must mint an owner-claim token")
	}

	public := newH2CServer(h.Handler())
	defer public.Close()
	internal := httptest.NewServer(h.InternalHandler())
	defer internal.Close()

	if code, body := getJSON(t, public.URL+"/healthz"); code != 200 || body["status"] != "ok" || body["version"] != "test" {
		t.Errorf("healthz: %d %v", code, body)
	}
	if code, body := getJSON(t, public.URL+"/readyz"); code != 200 || body["status"] != "ready" {
		t.Errorf("readyz: %d %v", code, body)
	}

	grpc := hubv1connect.NewOrganizationServiceClient(h2cClient(), public.URL, connect.WithGRPC())
	if _, err := grpc.ClaimOwnership(ctx, connect.NewRequest(&hubv1.ClaimOwnershipRequest{Token: "wrong"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("wrong token over gRPC: %v", err)
	}
	claimed, err := grpc.ClaimOwnership(ctx, connect.NewRequest(&hubv1.ClaimOwnershipRequest{Token: token}))
	if err != nil || !claimed.Msg.GetOrganization().GetOwned() {
		t.Fatalf("claim over gRPC: %v", err)
	}
	if _, err := grpc.ClaimOwnership(ctx, connect.NewRequest(&hubv1.ClaimOwnershipRequest{Token: token})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("second claim: %v", err)
	}

	resp, err := http.Post(public.URL+"/gravel.hub.v1.OrganizationService/GetOrganization", "application/json", strings.NewReader("{}")) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&got)
	_ = resp.Body.Close()
	if o, _ := got["organization"].(map[string]any); o["owned"] != true || o["name"] != "Hidden Token Gaming" || resp.Header.Get("X-Request-ID") == "" {
		t.Errorf("json get after claim: %v (request id %q)", got, resp.Header.Get("X-Request-ID"))
	}

	mresp, err := http.Get(internal.URL + "/metrics") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	metrics, _ := io.ReadAll(mresp.Body)
	_ = mresp.Body.Close()
	for _, want := range []string{
		`gravel_build_info{go_version="`,
		`gravel_rpc_requests_total{code="ok",procedure="/gravel.hub.v1.OrganizationService/ClaimOwnership"} 1`,
		`gravel_rpc_requests_total{code="failed_precondition",procedure="/gravel.hub.v1.OrganizationService/ClaimOwnership"} 1`,
		`gravel_http_requests_total{code="200",handler="healthz",method="GET"} 1`,
		`go_goroutines`,
	} {
		if !strings.Contains(string(metrics), want) {
			t.Errorf("metrics should contain %s", want)
		}
	}

	// A restart of an owned hub mints nothing.
	h2, err := hub.New(ctx, hub.Options{Config: cfg, Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	if h2.OwnerClaimToken() != "" {
		t.Error("an owned hub must not mint a token")
	}
	h2.Close()

	h.Close()
	if code, body := getJSON(t, public.URL+"/readyz"); code != http.StatusServiceUnavailable || body["status"] != "not ready" {
		t.Errorf("readyz with the pool closed: %d %v", code, body)
	}
}

func TestRunServesAndStops(t *testing.T) {
	cfg := testConfig(t)
	h, err := hub.New(context.Background(), hub.Options{Config: cfg, Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	select {
	case <-h.Started():
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not start listening")
	}
	if code, _ := getJSON(t, "http://"+h.PublicAddr().String()+"/healthz"); code != 200 {
		t.Errorf("healthz over Run: %d", code)
	}
	if code, _ := getJSON(t, "http://"+h.InternalAddr().String()+"/metrics"); code != 200 {
		t.Errorf("metrics over Run: %d", code)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop")
	}
	d := net.Dialer{Timeout: time.Second}
	if _, err := d.DialContext(context.Background(), "tcp", h.PublicAddr().String()); err == nil {
		t.Error("public listener should be closed after Run returns")
	}
}

func TestRunFailsOnBusyPort(t *testing.T) {
	cfg := testConfig(t)
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	cfg.Server.Listen = ln.Addr().String()
	h, err := hub.New(context.Background(), hub.Options{Config: cfg, Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Errorf("want a listen error, got %v", err)
	}
}
