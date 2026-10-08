// Package hub assembles the service: store, domain, API handlers, health, metrics and the two
// listeners. cmd/gravel-hub is a thin shell around it.
package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"runtime"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"
	"connectrpc.com/grpcreflect"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/api"
	"github.com/gravel-project/gravel/internal/config"
	"github.com/gravel-project/gravel/internal/httpx"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/store"
)

// Options configure New.
type Options struct {
	Config  config.Config
	Logger  *slog.Logger
	Version string
}

// Hub is a started-but-not-listening service: handlers are ready, Run serves them.
type Hub struct {
	cfg     config.Config
	logger  *slog.Logger
	version string

	st         *store.Store
	org        *org.Service
	claimToken string

	registry *prometheus.Registry
	metrics  *httpx.Metrics
	public   http.Handler
	internal http.Handler

	listeners *listeners
}

// New connects to the database, applies the migration policy, ensures the built-in organization
// and builds the handlers. It logs the owner-claim token when the hub is unowned.
func New(ctx context.Context, opts Options) (*Hub, error) {
	cfg, logger := opts.Config, opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	h := &Hub{cfg: cfg, logger: logger, version: opts.Version, listeners: newListeners()}

	logger.Info("connecting to the database", "url", cfg.Database.RedactedURL(), "source", cfg.Database.Source())
	st, err := store.Open(ctx, cfg.Database.URL, cfg.Database.MaxConns, cfg.Database.ConnectTimeout, logger)
	if err != nil {
		return nil, fmt.Errorf("hub: %w", err)
	}
	h.st = st
	switch cfg.Database.Migrate {
	case config.MigrateAuto:
		if err := st.Migrate(ctx); err != nil {
			st.Close()
			return nil, fmt.Errorf("hub: %w", err)
		}
	default:
		if err := st.Ready(ctx); err != nil {
			st.Close()
			return nil, fmt.Errorf("hub: database.migrate is %q and the schema is not current: %w", cfg.Database.Migrate, err)
		}
	}

	h.org = org.New(st, cfg.Claim.TokenTTL, logger)
	o, token, err := h.org.EnsureBuiltin(ctx, cfg.Organization.Name)
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("hub: %w", err)
	}
	h.claimToken = token
	if token != "" {
		logger.Warn("the hub is unowned: claim it once with this token (it expires; a restart mints a new one)",
			"token", token, "expires_at", o.ClaimTokenExpiresAt.UTC().Format(time.RFC3339),
			"how", "ClaimOwnership on gravel.hub.v1.OrganizationService")
	} else {
		logger.Info("organization", "name", o.Name, "id", o.ID, "owned", true, "claimed_at", o.ClaimedAt)
	}

	h.registry = prometheus.NewRegistry()
	h.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gravel_build_info", Help: "Build information; always 1.",
	}, []string{"version", "go_version"})
	buildInfo.WithLabelValues(h.version, runtime.Version()).Set(1)
	h.registry.MustRegister(buildInfo)
	h.metrics = httpx.NewMetrics(h.registry)

	h.public = h.buildPublic()
	h.internal = h.buildInternal()
	return h, nil
}

// Handler is the public listener's handler: the Connect API, gRPC health and reflection,
// /healthz and /readyz. Serve it with unencrypted HTTP/2 enabled (Run does) so gRPC works
// behind TLS termination.
func (h *Hub) Handler() http.Handler { return h.public }

// InternalHandler serves /metrics and /debug/pprof/. Keep it off the public network.
func (h *Hub) InternalHandler() http.Handler { return h.internal }

// OwnerClaimToken is the token minted at start, or "" when the hub is owned.
func (h *Hub) OwnerClaimToken() string { return h.claimToken }

// Close releases the database pool. Run calls it; call it yourself when you only used Handler.
func (h *Hub) Close() { h.st.Close() }

func (h *Hub) buildPublic() http.Handler {
	mux := http.NewServeMux()
	interceptors := connect.WithInterceptors(h.metrics.Interceptor())
	mux.Handle(hubv1connect.NewOrganizationServiceHandler(api.NewOrganizationServer(h.org, h.logger), interceptors))
	mux.Handle(grpchealth.NewHandler(grpchealth.NewStaticChecker(hubv1connect.OrganizationServiceName)))
	reflector := grpcreflect.NewStaticReflector(hubv1connect.OrganizationServiceName)
	mux.Handle(grpcreflect.NewHandlerV1(reflector))
	mux.Handle(grpcreflect.NewHandlerV1Alpha(reflector))
	mux.Handle("GET /healthz", h.metrics.HTTP("healthz", http.HandlerFunc(h.healthz)))
	mux.Handle("GET /readyz", h.metrics.HTTP("readyz", http.HandlerFunc(h.readyz)))
	return httpx.Chain(mux, httpx.RequestID, httpx.Recover(h.logger), httpx.Logging(h.logger, "/healthz", "/readyz"))
}

func (h *Hub) buildInternal() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(h.registry, promhttp.HandlerOpts{Registry: h.registry}))
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	return httpx.Chain(mux, httpx.RequestID, httpx.Recover(h.logger))
}

func (h *Hub) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": h.version})
}

func (h *Hub) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := h.st.Ready(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready", "reason": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
