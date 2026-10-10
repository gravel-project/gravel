// Package hub assembles the service: store, domain, API handlers, pages, health, metrics and
// the two listeners. cmd/gravel-hub is a thin shell around it.
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"runtime"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"
	"connectrpc.com/grpcreflect"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/gravel-project/gravel/drivers"
	wardogsdriver "github.com/gravel-project/gravel/drivers/wardogs"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/api"
	"github.com/gravel-project/gravel/internal/apps"
	"github.com/gravel-project/gravel/internal/backup"
	"github.com/gravel-project/gravel/internal/config"
	"github.com/gravel-project/gravel/internal/httpx"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/identity/providers"
	"github.com/gravel-project/gravel/internal/ingest"
	"github.com/gravel-project/gravel/internal/jobs"
	"github.com/gravel-project/gravel/internal/metricnames"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/ratelimit"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/session"
	"github.com/gravel-project/gravel/internal/stats"
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
	apps       *apps.Service
	servers    *servers.Service
	monitor    *servers.Monitor
	moderation *servers.Moderation
	stats      *stats.Boards
	feeds      *ingest.Keyring
	ingest     *ingest.Handler
	jobs       *jobs.Runner
	web        *web.Handler
	perIP      *ratelimit.Limiter
	perUser    *ratelimit.Limiter
	perApp     *ratelimit.Limiter
	claimToken string

	registry     *metricnames.Registry
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

// Drivers are the drivers this hub has, by the name servers.yaml uses.
func Drivers() drivers.Registry {
	return drivers.Registry{wardogsdriver.Name: wardogsdriver.New}
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
			"how", "log in, then paste it on the account page or call ClaimOwnership on gravel.hub.v1.OrganizationService")
	} else {
		logger.Info("organization", "name", o.Name, "id", o.ID, "owned", true, "claimed_at", o.ClaimedAt, "owner_user_id", o.OwnerUserID)
	}

	regs := opts.Providers
	if regs == nil {
		regs = h.buildProviders()
	}
	h.ids = identity.New(st, o.ID, regs, cfg.Auth.AttemptTTL, logger, identity.WithOrganizationName(o.Name))
	h.sess = session.New(st, cfg.Auth.SessionTTL, cfg.Auth.Secure(), logger)
	h.apps = apps.New(st, o.ID, cfg.Apps.TokenTTL, logger)
	h.perIP = ratelimit.New(cfg.RateLimit.PerIP.RequestsPerMinute, cfg.RateLimit.PerIP.Burst)
	h.perUser = ratelimit.New(cfg.RateLimit.PerUser.RequestsPerMinute, cfg.RateLimit.PerUser.Burst)
	h.perApp = ratelimit.New(cfg.RateLimit.PerApp.RequestsPerMinute, cfg.RateLimit.PerApp.Burst)

	h.registry = metricnames.New()
	h.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gravel_build_info", Help: "Build information; always 1.",
	}, []string{"version", "go_version"})
	buildInfo.WithLabelValues(h.version, runtime.Version()).Set(1)
	h.authTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gravel_auth_completions_total", Help: "Finished login, link and Linked Roles attempts by provider, intent and result.",
	}, []string{"provider", "intent", "result"})
	h.limitedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gravel_rate_limited_total", Help: "Requests refused by a rate limit, by scope (ip, user or app).",
	}, []string{"scope"})
	providerNames := make([]string, 0, len(h.ids.Providers()))
	for _, reg := range h.ids.Providers() {
		providerNames = append(providerNames, reg.Provider.Name())
	}
	h.registry.MustRegister(buildInfo, h.authTotal, h.limitedTotal, backup.NewCollector(cfg.Backup.StatusFile, st, logger), newStatsCollector(st, providerNames, logger))
	h.metrics = httpx.NewMetrics(h.registry)

	// Background work runs as supervised jobs (ADR-0010): the prune, and the servers' monitor,
	// which reconciles a poll job per server with what `servers apply` stored.
	h.jobs = jobs.New(logger, h.registry)
	h.jobs.Start(jobs.Job{Name: "prune", Every: pruneEvery, Run: func(ctx context.Context) error { h.Prune(ctx); return nil }})
	registry := Drivers()
	h.servers = servers.New(st, o.ID, registry.Names(), logger)
	h.monitor = servers.NewMonitor(h.servers, registry, h.jobs, h.registry, "gravel-hub/"+h.version, logger)
	h.monitor.Start()
	h.moderation = servers.NewModeration(h.servers, h.monitor, st, st, o.ID, h.registry, logger)
	// The stats store (ADR-0012): every good poll becomes matches and player totals; boards read
	// them with the Organization settings' stats section.
	// The recorder and the rollup count their writes, so the boards serve from memory while
	// nobody plays.
	changes := &stats.Changes{}
	h.monitor.SetSink(stats.NewRecorder(st, o.ID, changes, h.registry, logger))
	statsSettings := func(ctx context.Context) (org.Stats, error) {
		set, _, err := h.org.Settings(ctx)
		return set.Stats, err
	}
	h.stats = stats.NewBoards(st, o.ID, stats.NewNamer(st, o.ID, []byte(cfg.Stats.PseudonymKey), logger), statsSettings, changes)
	// Retention (ADR-0012 §6): raw rows past the window roll up into monthly totals.
	h.jobs.Start(jobs.Job{Name: "stats_rollup", Every: stats.RollupEvery, Immediate: true,
		Run: stats.NewRetention(st, o.ID, statsSettings, changes, h.registry, logger).Run})
	// Inbound ingestion (ADR-0011): the feed tokens are read from their files as the monitor reads
	// the credentials, and the stored batches are pruned after their retention.
	h.feeds = ingest.NewKeyring()
	h.ingest = ingest.NewHandler(h.feeds, st, o.ID, h.registry, logger)
	h.jobs.Start(jobs.Job{Name: "ingest_feeds", Every: servers.ReconcileEvery, Immediate: true, Run: h.refreshFeeds})
	h.jobs.Start(jobs.Job{Name: "ingest_prune", Every: ingest.PruneEvery, Run: func(ctx context.Context) error {
		return ingest.Prune(ctx, st, time.Now(), logger)
	}})

	// The API, once: the public listener serves it, and the pages call it in process through
	// the session middleware (ADR-0005).
	h.api = h.buildAPI()
	h.web, err = web.New(h.ids, h.sess, h.sess.Middleware(h.api), nil, logger) // the theme comes from the Organization settings, through the API
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
		linkedRoles := "off (discord is link-only)"
		if cfg.Discord.Login {
			linkedRoles = cfg.RolesURL("discord")
		}
		h.logger.Info("login provider", "provider", "discord", "login", cfg.Discord.Login, "callback", cfg.CallbackURL("discord"), "linked_roles_verification_url", linkedRoles, "secret", cfg.Discord.SecretSource())
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

// scopes are the rate-limit buckets: per app when a bearer token is carried, per user when
// logged in, else per client address. An API call a page makes in process is not counted
// again: the page request was.
func (h *Hub) scopes() []ratelimit.Scoped {
	return []ratelimit.Scoped{
		{Scope: "app", Limiter: h.perApp, Key: func(r *http.Request) string {
			if a, ok := apps.FromContext(r.Context()); ok {
				return a.ClientID
			}
			return ""
		}},
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
	mux.Handle(hubv1connect.NewServerServiceHandler(api.NewServerServer(h.servers, h.monitor, h.ids, h.logger), interceptors))
	mux.Handle(hubv1connect.NewModerationServiceHandler(api.NewModerationServer(h.moderation, h.org, h.ids, h.logger), interceptors))
	mux.Handle(hubv1connect.NewServerConfigServiceHandler(api.NewConfigServer(h.moderation, h.org, h.logger), interceptors))
	mux.Handle(hubv1connect.NewStatsServiceHandler(api.NewStatsServer(h.stats, h.org, h.ids, h.logger), interceptors))
	return mux
}

func (h *Hub) rejected(scope string) { h.limitedTotal.WithLabelValues(scope).Inc() }

func (h *Hub) buildPublic() http.Handler {
	scopes := h.scopes()
	mux := http.NewServeMux()
	services := []string{hubv1connect.OrganizationServiceName, hubv1connect.IdentityServiceName, hubv1connect.ServerServiceName,
		hubv1connect.ModerationServiceName, hubv1connect.ServerConfigServiceName, hubv1connect.StatsServiceName}
	for _, svc := range services {
		mux.Handle("/"+svc+"/", h.api)
	}
	mux.Handle(grpchealth.NewHandler(grpchealth.NewStaticChecker(services...)))
	reflector := grpcreflect.NewStaticReflector(services...)
	mux.Handle(grpcreflect.NewHandlerV1(reflector))
	mux.Handle(grpcreflect.NewHandlerV1Alpha(reflector))
	mux.Handle("GET /healthz", h.metrics.HTTP("healthz", http.HandlerFunc(h.healthz)))
	mux.Handle("GET /readyz", h.metrics.HTTP("readyz", http.HandlerFunc(h.readyz)))
	// The token endpoint (ADR-0008): a service trades its client credentials for a bearer token.
	// Limited per client address, like anything anonymous.
	mux.Handle("/oauth/token", h.metrics.HTTP("oauth_token", ratelimit.Middleware(scopes, h.rejected)(h.apps.TokenHandler())))

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
	// The ingest route (ADR-0011) authenticates its own bearer, a server's feed token, so it sits
	// outside the session and app middlewares: the app middleware would refuse that bearer as an
	// unknown app token. It limits itself (per server, and per address for bad tokens).
	top := http.NewServeMux()
	top.Handle("POST "+ingest.Path, h.metrics.HTTP("ingest", h.ingest))
	top.Handle("/", httpx.Chain(mux, csrf.Handler, h.sess.Middleware, h.apps.Middleware))
	return httpx.Chain(top,
		httpx.RequestID,
		httpx.RealIP(h.cfg.Server.ClientIPHeader),
		httpx.Recover(h.logger),
		httpx.Logging(h.logger, "/healthz", "/readyz"),
	)
}

// refreshFeeds reads every server's feed token file into the keyring. A file that cannot be read
// leaves its server without a feed until it can, and fails the job, which logs it.
func (h *Hub) refreshFeeds(ctx context.Context) error {
	list, err := h.servers.Servers(ctx)
	if err != nil {
		return err
	}
	var feeds []ingest.Feed
	var errs []error
	for _, s := range list {
		if s.Feed == nil {
			continue
		}
		b, err := os.ReadFile(s.Feed.TokenFile)
		if err != nil {
			errs = append(errs, fmt.Errorf("server %s: feed token: %w", s.ID, err))
			continue
		}
		feeds = append(feeds, ingest.Feed{Server: s.ID, Source: s.Game, Token: string(b)})
	}
	h.feeds.Update(feeds)
	return errors.Join(errs...)
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

// Prune deletes expired sessions and attempts and drops idle rate-limit buckets. The "prune"
// job runs it; tests call it directly.
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
	tokens, err := h.apps.Prune(ctx)
	if err != nil {
		h.logger.Warn("could not prune app tokens", "error", err.Error())
	}
	ips, users, appsSwept := h.perIP.Sweep(), h.perUser.Sweep(), h.perApp.Sweep()
	h.ingest.Sweep()
	h.logger.Debug("pruned", "sessions", sessions, "attempts", attempts, "app_tokens", tokens, "ip_buckets", ips, "user_buckets", users, "app_buckets", appsSwept)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// MetricNames are the names of every metric the hub exposes on its internal listener, including
// those with no sample yet (docs/hub.md, Observability; the dashboards' drift test reads them).
func (h *Hub) MetricNames() []string { return h.registry.Names() }
