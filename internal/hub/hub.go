// Package hub assembles the service: store, domain, API handlers, pages, health, metrics and
// the two listeners. cmd/gravel-hub is a thin shell around it.
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
	"github.com/gravel-project/gravel/internal/backup"
	"github.com/gravel-project/gravel/internal/config"
	"github.com/gravel-project/gravel/internal/httpx"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/identity/providers"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/ratelimit"
	"github.com/gravel-project/gravel/internal/session"
	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/web"
)

// Options configure New.
type Options struct {
	Config  config.Config
	Logger  *slog.Logger
	Version string
	// Providers replaces the login providers the configuration describes. Tests use it to
	// drive the flows with a fake; nil means "build them from the configuration".
	Providers []identity.Registration
}

// Hub is a started-but-not-listening service: handlers are ready, Run serves them.
type Hub struct {
	cfg     config.Config
	logger  *slog.Logger
	version string

	st         *store.Store
	org        *org.Service
	ids        *identity.Service
	sess       *session.Manager
	web        *web.Handler
	perIP      *ratelimit.Limiter
	perUser    *ratelimit.Limiter
	claimToken string

	registry     *prometheus.Registry
	metrics      *httpx.Metrics
	authTotal    *prometheus.CounterVec
	limitedTotal *prometheus.CounterVec
	api          *http.ServeMux
	public       http.Handler
	internal     http.Handler

	listeners *listeners
}

// pruneEvery is how often expired sessions and attempts are deleted and idle rate-limit
// buckets dropped.
const pruneEvery = 10 * time.Minute

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
			"how", "log in, then paste it on the account page or call ClaimOwnership on gravel.hub.v1.OrganizationService")
	} else {
		logger.Info("organization", "name", o.Name, "id", o.ID, "owned", true, "claimed_at", o.ClaimedAt, "owner_user_id", o.OwnerUserID)
	}

	regs := opts.Providers
	if regs == nil {
		regs = h.buildProviders()
	}
	h.ids = identity.New(st, o.ID, regs, cfg.Auth.AttemptTTL, logger)
	h.sess = session.New(st, cfg.Auth.SessionTTL, cfg.Auth.Secure(), logger)
	h.perIP = ratelimit.New(cfg.RateLimit.PerIP.RequestsPerMinute, cfg.RateLimit.PerIP.Burst)
	h.perUser = ratelimit.New(cfg.RateLimit.PerUser.RequestsPerMinute, cfg.RateLimit.PerUser.Burst)

	h.registry = prometheus.NewRegistry()
	h.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gravel_build_info", Help: "Build information; always 1.",
	}, []string{"version", "go_version"})
	buildInfo.WithLabelValues(h.version, runtime.Version()).Set(1)
	h.authTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gravel_auth_completions_total", Help: "Finished login and link attempts by provider, intent and result.",
	}, []string{"provider", "intent", "result"})
	h.limitedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gravel_rate_limited_total", Help: "Requests refused by a rate limit, by scope (ip or user).",
	}, []string{"scope"})
	h.registry.MustRegister(buildInfo, h.authTotal, h.limitedTotal, backup.NewCollector(cfg.Backup.StatusFile, st, logger))
	h.metrics = httpx.NewMetrics(h.registry)

	// The API, once: the public listener serves it, and the pages call it in process through
	// the session middleware (ADR-0005).
	h.api = h.buildAPI()
	h.web, err = web.New(h.ids, h.sess, h.sess.Middleware(h.api), web.StaticTheme{T: web.DefaultTheme()}, logger)
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("hub: %w", err)
	}
	h.web.Observe = func(provider string, intent identity.Intent, result string) {
		h.authTotal.WithLabelValues(provider, string(intent), result).Inc()
	}

	h.public = h.buildPublic()
	h.internal = h.buildInternal()
	return h, nil
}

// buildProviders turns the auth configuration into login providers. Secrets are logged by
// source only.
func (h *Hub) buildProviders() []identity.Registration {
	cfg := h.cfg.Auth
	client := &http.Client{Timeout: 15 * time.Second}
	var regs []identity.Registration
	if cfg.Discord.Enabled {
		regs = append(regs, identity.Registration{Provider: providers.Discord(cfg.Discord.ClientID, cfg.Discord.ClientSecret, cfg.CallbackURL("discord"), client), Login: cfg.Discord.Login})
		h.logger.Info("login provider", "provider", "discord", "login", cfg.Discord.Login, "callback", cfg.CallbackURL("discord"), "secret", cfg.Discord.SecretSource())
	}
	if cfg.Steam.Enabled {
		regs = append(regs, identity.Registration{Provider: providers.Steam(cfg.Steam.APIKey, cfg.CallbackURL("steam"), client), Login: cfg.Steam.Login})
		key := cfg.Steam.KeySource()
		if key == "" {
			key = "none (no persona name or avatar)"
		}
		h.logger.Info("login provider", "provider", "steam", "login", cfg.Steam.Login, "callback", cfg.CallbackURL("steam"), "api_key", key)
	}
	if len(regs) == 0 {
		h.logger.Warn("no login provider is configured: the pages offer no login and the API has no callers")
	}
	return regs
}

// Handler is the public listener's handler: the Connect API, gRPC health and reflection,
// /healthz and /readyz, and the pages. Serve it with unencrypted HTTP/2 enabled (Run does) so
// gRPC works behind TLS termination.
func (h *Hub) Handler() http.Handler { return h.public }

// InternalHandler serves /metrics and /debug/pprof/. Keep it off the public network.
func (h *Hub) InternalHandler() http.Handler { return h.internal }

// OwnerClaimToken is the token minted at start, or "" when the hub is owned.
func (h *Hub) OwnerClaimToken() string { return h.claimToken }

// Close releases the database pool. Run calls it; call it yourself when you only used Handler.
func (h *Hub) Close() { h.st.Close() }

// scopes are the rate-limit buckets: per user when logged in, else per client address. An API
// call a page makes in process is not counted again: the page request was.
func (h *Hub) scopes() []ratelimit.Scoped {
	return []ratelimit.Scoped{
		{Scope: "user", Limiter: h.perUser, Key: func(r *http.Request) string {
			if web.IsInProcess(r.Context()) {
				return ""
			}
			if s, ok := session.FromContext(r.Context()); ok {
				return s.UserID.String()
			}
			return ""
		}},
		{Scope: "ip", Limiter: h.perIP, Key: func(r *http.Request) string {
			if web.IsInProcess(r.Context()) {
				return ""
			}
			return ratelimit.ClientIP(r)
		}},
	}
}

// buildAPI mounts the Connect services with their interceptors.
func (h *Hub) buildAPI() *http.ServeMux {
	mux := http.NewServeMux()
	interceptors := connect.WithInterceptors(h.metrics.Interceptor(), ratelimit.Interceptor(h.scopes(), h.rejected))
	mux.Handle(hubv1connect.NewOrganizationServiceHandler(api.NewOrganizationServer(h.org, h.logger), interceptors))
	mux.Handle(hubv1connect.NewIdentityServiceHandler(api.NewIdentityServer(h.ids, h.sess, h.org, h.logger), interceptors))
	return mux
}

func (h *Hub) rejected(scope string) { h.limitedTotal.WithLabelValues(scope).Inc() }

func (h *Hub) buildPublic() http.Handler {
	scopes := h.scopes()
	mux := http.NewServeMux()
	for _, prefix := range []string{"/" + hubv1connect.OrganizationServiceName + "/", "/" + hubv1connect.IdentityServiceName + "/"} {
		mux.Handle(prefix, h.api)
	}
	services := []string{hubv1connect.OrganizationServiceName, hubv1connect.IdentityServiceName}
	mux.Handle(grpchealth.NewHandler(grpchealth.NewStaticChecker(services...)))
	reflector := grpcreflect.NewStaticReflector(services...)
	mux.Handle(grpcreflect.NewHandlerV1(reflector))
	mux.Handle(grpcreflect.NewHandlerV1Alpha(reflector))
	mux.Handle("GET /healthz", h.metrics.HTTP("healthz", http.HandlerFunc(h.healthz)))
	mux.Handle("GET /readyz", h.metrics.HTTP("readyz", http.HandlerFunc(h.readyz)))

	// The pages, rate-limited as HTTP; the procedures above are limited by the interceptor.
	pages := http.NewServeMux()
	h.web.Register(pages)
	mux.Handle("/", h.metrics.HTTP("pages", ratelimit.Middleware(scopes, h.rejected)(pages)))

	// Cross-site browser requests with unsafe methods are refused by Fetch metadata (or the
	// Origin header) before any handler; non-browser clients carry neither and pass.
	csrf := http.NewCrossOriginProtection()
	if h.cfg.Auth.BaseURL != "" {
		if err := csrf.AddTrustedOrigin(h.cfg.Auth.BaseURL); err != nil {
			h.logger.Warn("auth.base_url is not a trusted origin", "error", err.Error())
		}
	}
	return httpx.Chain(mux,
		httpx.RequestID,
		httpx.RealIP(h.cfg.Server.ClientIPHeader),
		httpx.Recover(h.logger),
		httpx.Logging(h.logger, "/healthz", "/readyz"),
		csrf.Handler,
		h.sess.Middleware,
	)
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

// Prune deletes expired sessions and attempts and drops idle rate-limit buckets. Run calls it
// on a timer; tests call it directly.
func (h *Hub) Prune(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	sessions, err := h.sess.Prune(ctx)
	if err != nil {
		h.logger.Warn("could not prune sessions", "error", err.Error())
	}
	attempts, err := h.ids.Prune(ctx)
	if err != nil {
		h.logger.Warn("could not prune auth attempts", "error", err.Error())
	}
	ips, users := h.perIP.Sweep(), h.perUser.Sweep()
	h.logger.Debug("pruned", "sessions", sessions, "attempts", attempts, "ip_buckets", ips, "user_buckets", users)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
