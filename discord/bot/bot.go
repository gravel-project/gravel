// Package bot is gravel's Discord bot runtime (ADR-0008): one Discord application, modules that
// register slash commands, gateway listeners and background jobs, interactions delivered over
// HTTP (signature-verified, acknowledged within Discord's three seconds) or over the gateway
// when the application has no endpoint URL, and a single-replica gateway that resumes. The
// stock binary (cmd/gravel-bot) runs gravel's modules; a host's binary imports this package and
// registers its own beside them.
package bot

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/disgoorg/disgo"
	disgobot "github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/handler"
	"github.com/disgoorg/disgo/httpserver"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Module is a unit of bot behaviour. Register adds its commands, listeners and jobs to the
// registry; the runtime registers the commands with Discord and runs the rest.
type Module interface {
	Name() string
	Register(r *Registry) error
}

// Job is background work that runs for the life of the bot: a reconciler's loop, a poller. It
// returns when ctx is done; an error stops the bot (a supervisor restarts it).
type Job func(ctx context.Context) error

// Registry collects what modules register. Commands are routed by name; a handler replies
// through disgo's CommandEvent (CreateMessage, DeferCreateMessage then UpdateInteractionResponse
// for anything slow: Discord waits three seconds for the first answer).
type Registry struct {
	module   string
	router   handler.Router
	commands []discord.ApplicationCommandCreate
	names    map[string]string
	jobs     []namedJob
	listen   []disgobot.EventListener
}

type namedJob struct {
	name string
	run  Job
}

// Snowflake is a Discord id, for configuration and tests without importing disgo's package.
type Snowflake = snowflake.ID

// SlashCommand registers a slash command and its handler (disgo's CommandEvent: CreateMessage, or
// DeferCreateMessage then UpdateInteractionResponse for anything slower than three seconds). A name
// registered by another module is an error at startup, not a silent override.
func (r *Registry) SlashCommand(create discord.SlashCommandCreate, h handler.CommandHandler) error {
	if create.Name == "" {
		return fmt.Errorf("bot: module %s registers a slash command without a name", r.module)
	}
	if owner, dup := r.names[create.Name]; dup {
		return fmt.Errorf("bot: slash command /%s is registered by both %s and %s", create.Name, owner, r.module)
	}
	r.names[create.Name] = r.module
	r.commands = append(r.commands, create)
	r.router.Command("/"+create.Name, h)
	return nil
}

// Component registers a handler for message components whose custom id matches the pattern
// (disgo's router syntax: "/verify/{user}").
func (r *Registry) Component(pattern string, h handler.ComponentHandler) {
	r.router.Component(pattern, h)
}

// Listen adds a gateway event listener (disgo's bot.NewListenerFunc wraps a func).
func (r *Registry) Listen(l disgobot.EventListener) { r.listen = append(r.listen, l) }

// Job adds background work.
func (r *Registry) Job(name string, run Job) {
	r.jobs = append(r.jobs, namedJob{name: r.module + "/" + name, run: run})
}

// Options tune New beyond the configuration.
type Options struct {
	Config  Config
	Logger  *slog.Logger
	Version string
	// Intents replaces the default gateway intents (guilds and guild members).
	Intents *gateway.Intents
	// RESTURL replaces Discord's API base, for tests against a fake.
	RESTURL string
	// HTTPClient is the client disgo's REST layer uses; nil means a default with a timeout.
	HTTPClient *http.Client
}

// Runtime is a built bot: handlers are ready, Run serves them.
type Runtime struct {
	cfg     Config
	logger  *slog.Logger
	version string
	client  *disgobot.Client
	router  *handler.Mux
	modules []Module

	commands []discord.ApplicationCommandCreate
	jobs     []namedJob

	public   http.Handler
	internal http.Handler
	ready    atomic.Bool
	gateway  atomic.Bool

	registry         *prometheus.Registry
	interactions     *prometheus.CounterVec
	gatewayConnected prometheus.Gauge
	jobResults       *prometheus.CounterVec

	listeners listeners
}

type listeners struct {
	once             sync.Once
	public, internal net.Addr
	started          chan struct{}
}

// Default gateway intents: guild structure and member events, which role sync follows. Message
// content is never requested.
const defaultIntents = gateway.IntentGuilds | gateway.IntentGuildMembers

// New builds the runtime: the disgo client, the modules' registrations, the HTTP handlers.
// Nothing talks to Discord until Run.
func New(opts Options, modules ...Module) (*Runtime, error) {
	cfg, logger := opts.Config, opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	key, err := hex.DecodeString(cfg.Discord.PublicKey)
	if err != nil || len(key) != 32 {
		return nil, errors.New("bot: discord.public_key is not a 32-byte hex key")
	}
	if id, ok := applicationIDFromToken(cfg.Discord.Token); !ok {
		return nil, errors.New("bot: discord.token is not a bot token")
	} else if id != cfg.Discord.AppID() {
		return nil, fmt.Errorf("bot: discord.token belongs to application %s, not discord.application_id %s", id, cfg.Discord.ApplicationID)
	}
	rt := &Runtime{cfg: cfg, logger: logger, version: opts.Version, modules: modules, router: handler.New(), listeners: listeners{started: make(chan struct{})}}
	rt.router.Use(rt.observe)
	rt.router.NotFound(func(e *handler.InteractionEvent) error {
		return e.CreateMessage(discord.MessageCreate{Content: "That command is not available here.", Flags: discord.MessageFlagEphemeral})
	})
	rt.router.Error(func(e *handler.InteractionEvent, err error) {
		logger.Error("interaction failed", "error", err.Error(), "interaction", e.ID())
	})

	names := map[string]string{}
	var listen []disgobot.EventListener
	for _, m := range modules {
		reg := &Registry{module: m.Name(), router: rt.router, names: names}
		if err := m.Register(reg); err != nil {
			return nil, fmt.Errorf("bot: module %s: %w", m.Name(), err)
		}
		rt.commands = append(rt.commands, reg.commands...)
		rt.jobs = append(rt.jobs, reg.jobs...)
		listen = append(listen, reg.listen...)
	}

	intents := defaultIntents
	if opts.Intents != nil {
		intents = *opts.Intents
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	restOpts := []rest.ClientConfigOpt{rest.WithHTTPClient(httpClient), rest.WithLogger(logger)}
	if opts.RESTURL != "" {
		restOpts = append(restOpts, rest.WithURL(opts.RESTURL))
	}
	clientOpts := []disgobot.ConfigOpt{
		disgobot.WithLogger(logger),
		disgobot.WithRestClientConfigOpts(restOpts...),
		disgobot.WithEventListeners(append([]disgobot.EventListener{rt.router, rt.gatewayStatus()}, listen...)...),
	}
	if cfg.Discord.Gateway {
		clientOpts = append(clientOpts, disgobot.WithGatewayConfigOpts(gateway.WithIntents(intents), gateway.WithAutoReconnect(true)))
	}
	client, err := disgo.New(cfg.Discord.Token, clientOpts...)
	if err != nil {
		return nil, fmt.Errorf("bot: %w", err)
	}
	rt.client = client

	rt.registry = prometheus.NewRegistry()
	rt.registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "gravel_bot_build_info", Help: "Build information; always 1."}, []string{"version", "go_version"})
	buildInfo.WithLabelValues(rt.version, runtime.Version()).Set(1)
	rt.interactions = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gravel_bot_interactions_total", Help: "Interactions handled, by route and result."}, []string{"route", "result"})
	rt.gatewayConnected = prometheus.NewGauge(prometheus.GaugeOpts{Name: "gravel_bot_gateway_connected", Help: "1 while the gateway session is up."})
	rt.jobResults = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gravel_bot_jobs_total", Help: "Background jobs ended, by job and result."}, []string{"job", "result"})
	rt.registry.MustRegister(buildInfo, rt.interactions, rt.gatewayConnected, rt.jobResults)

	rt.public = rt.buildPublic(key)
	rt.internal = rt.buildInternal()
	return rt, nil
}

// observe counts every routed interaction.
func (rt *Runtime) observe(next handler.Handler) handler.Handler {
	return func(e *handler.InteractionEvent) error {
		route := "other"
		if d, ok := e.Interaction.(discord.ApplicationCommandInteraction); ok {
			route = "/" + d.Data.CommandName()
		}
		err := next(e)
		result := "ok"
		if err != nil {
			result = "error"
		}
		rt.interactions.WithLabelValues(route, result).Inc()
		return err
	}
}

// gatewayStatus keeps the connected gauge and the readiness flag in step with the session.
func (rt *Runtime) gatewayStatus() disgobot.EventListener {
	return disgobot.NewListenerFunc(func(e disgobot.Event) {
		switch e.(type) {
		case *events.Ready:
			rt.gateway.Store(true)
			rt.gatewayConnected.Set(1)
			rt.logger.Info("gateway ready")
		case *events.Resumed:
			rt.gateway.Store(true)
			rt.gatewayConnected.Set(1)
			rt.logger.Info("gateway resumed")
		}
	})
}

func (rt *Runtime) buildPublic(key []byte) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /interactions", httpserver.HandleInteraction(httpserver.DefaultVerifier{}, key, rt.logger, rt.client.EventManager.HandleHTTPEvent))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": rt.version})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		switch {
		case !rt.ready.Load():
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "starting", "reason": "commands not registered yet"})
		case rt.cfg.Discord.Gateway && !rt.gateway.Load():
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "connecting", "reason": "gateway not ready"})
		default:
			writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
		}
	})
	return mux
}

func (rt *Runtime) buildInternal() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(rt.registry, promhttp.HandlerOpts{Registry: rt.registry}))
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// Handler serves /interactions, /healthz and /readyz.
func (rt *Runtime) Handler() http.Handler { return rt.public }

// InternalHandler serves /metrics.
func (rt *Runtime) InternalHandler() http.Handler { return rt.internal }

// Client is the disgo client, for a module that needs the REST API outside an interaction.
func (rt *Runtime) Client() *disgobot.Client { return rt.client }

// Commands are the slash commands the modules registered, in registration order.
func (rt *Runtime) Commands() []discord.ApplicationCommandCreate { return rt.commands }

// Start registers the commands with Discord and opens the gateway when it is on; Run calls it.
// Separated so a test can drive the handlers without a gateway.
func (rt *Runtime) Start(ctx context.Context) error {
	if err := handler.SyncCommands(rt.client, rt.commands, rt.cfg.Discord.Guilds(), rest.WithCtx(ctx)); err != nil {
		return fmt.Errorf("bot: register commands: %w", err)
	}
	scope := "global"
	if len(rt.cfg.Discord.GuildIDs) > 0 {
		scope = fmt.Sprintf("%d guild(s)", len(rt.cfg.Discord.GuildIDs))
	}
	rt.logger.Info("commands registered", "count", len(rt.commands), "scope", scope, "application_id", rt.cfg.Discord.ApplicationID)
	if rt.cfg.Discord.Gateway {
		if err := rt.client.OpenGateway(ctx); err != nil {
			return fmt.Errorf("bot: open gateway: %w", err)
		}
	}
	rt.ready.Store(true)
	return nil
}

// Close releases the Discord client.
func (rt *Runtime) Close(ctx context.Context) { rt.client.Close(ctx) }

// Run starts the bot, serves the two listeners and runs the jobs until ctx is cancelled or
// something fails.
func (rt *Runtime) Run(ctx context.Context) error {
	var lc net.ListenConfig
	publicLn, err := lc.Listen(ctx, "tcp", rt.cfg.Server.Listen)
	if err != nil {
		return fmt.Errorf("bot: listen %s: %w", rt.cfg.Server.Listen, err)
	}
	internalLn, err := lc.Listen(ctx, "tcp", rt.cfg.Server.InternalListen)
	if err != nil {
		_ = publicLn.Close()
		return fmt.Errorf("bot: listen %s: %w", rt.cfg.Server.InternalListen, err)
	}
	rt.listeners.once.Do(func() {
		rt.listeners.public, rt.listeners.internal = publicLn.Addr(), internalLn.Addr()
		close(rt.listeners.started)
	})
	rt.logger.Info("listening", "public", publicLn.Addr().String(), "internal", internalLn.Addr().String(), "version", rt.version,
		"gateway", rt.cfg.Discord.Gateway, "modules", len(rt.modules), "token", rt.cfg.Discord.TokenSource(), "hub_secret", rt.cfg.Hub.SecretSource())

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer rt.Close(context.Background())

	publicSrv := &http.Server{Handler: rt.public, ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 1 << 20}
	internalSrv := &http.Server{Handler: rt.internal, ReadHeaderTimeout: 10 * time.Second}
	errs := make(chan error, 2+len(rt.jobs))
	go func() { errs <- serve(ctx, publicSrv, publicLn, rt.cfg.Server.ShutdownTimeout) }()
	go func() { errs <- serve(ctx, internalSrv, internalLn, rt.cfg.Server.ShutdownTimeout) }()

	if err := rt.Start(ctx); err != nil {
		cancel()
		<-errs
		<-errs
		return err
	}
	for _, j := range rt.jobs {
		go func() { errs <- rt.runJob(ctx, j) }()
	}

	var first error
	for range 2 + len(rt.jobs) {
		if err := <-errs; err != nil && first == nil {
			first = err
			cancel()
		}
	}
	rt.logger.Info("stopped")
	return first
}

func (rt *Runtime) runJob(ctx context.Context, j namedJob) error {
	rt.logger.Info("job started", "job", j.name)
	err := j.run(ctx)
	switch {
	case err == nil || errors.Is(err, context.Canceled):
		rt.jobResults.WithLabelValues(j.name, "ok").Inc()
		rt.logger.Info("job stopped", "job", j.name)
		return nil
	default:
		rt.jobResults.WithLabelValues(j.name, "error").Inc()
		return fmt.Errorf("bot: job %s: %w", j.name, err)
	}
}

// Addrs returns the listeners' addresses once Run has bound them; tests pick free ports with
// ":0" and read them here.
func (rt *Runtime) Addrs(ctx context.Context) (public, internal net.Addr, err error) {
	select {
	case <-rt.listeners.started:
		return rt.listeners.public, rt.listeners.internal, nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

// serve runs srv on ln until ctx is cancelled, then shuts it down within timeout.
func serve(ctx context.Context, srv *http.Server, ln net.Listener, timeout time.Duration) error {
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	select {
	case err := <-served:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("bot: serve %s: %w", ln.Addr(), err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close()
		return fmt.Errorf("bot: shutdown %s: %w", ln.Addr(), err)
	}
	<-served
	return nil
}

// applicationIDFromToken reads the application id a bot token starts with (base64 of the id,
// then a dot), as disgo does; false when the token has no such shape.
func applicationIDFromToken(token string) (snowflake.ID, bool) {
	first, _, ok := strings.Cut(token, ".")
	if !ok {
		return 0, false
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(first, "="))
	if err != nil {
		return 0, false
	}
	id, err := snowflake.Parse(string(raw))
	return id, err == nil
}
