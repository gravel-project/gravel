// Package hub assembles the service: store, domain, API handlers, pages, health, metrics and
// the two listeners. cmd/gravel-hub is a thin shell around it.
package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"regexp"
	"runtime"
	"sync"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"
	"connectrpc.com/grpcreflect"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/gravel-project/gravel/cloud"
	"github.com/gravel-project/gravel/cloud/digitalocean"
	"github.com/gravel-project/gravel/drivers"
	"github.com/gravel-project/gravel/drivers/cloudvm"
	cs2game "github.com/gravel-project/gravel/drivers/cloudvm/cs2"
	wardogsdriver "github.com/gravel-project/gravel/drivers/wardogs"
	"github.com/gravel-project/gravel/games/cs2"
	"github.com/gravel-project/gravel/games/cs2/matchzy"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/api"
	"github.com/gravel-project/gravel/internal/apps"
	"github.com/gravel-project/gravel/internal/archive"
	"github.com/gravel-project/gravel/internal/backup"
	"github.com/gravel-project/gravel/internal/config"
	"github.com/gravel-project/gravel/internal/dashboards"
	"github.com/gravel-project/gravel/internal/httpx"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/identity/proof"
	"github.com/gravel-project/gravel/internal/identity/providers"
	"github.com/gravel-project/gravel/internal/ingest"
	"github.com/gravel-project/gravel/internal/jobs"
	"github.com/gravel-project/gravel/internal/metricnames"
	"github.com/gravel-project/gravel/internal/notify"
	"github.com/gravel-project/gravel/internal/objectstore"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/promquery"
	"github.com/gravel-project/gravel/internal/publicaddr"
	"github.com/gravel-project/gravel/internal/ratelimit"
	"github.com/gravel-project/gravel/internal/results"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/session"
	"github.com/gravel-project/gravel/internal/spend"
	"github.com/gravel-project/gravel/internal/stats"
	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/web"
	"github.com/gravel-project/gravel/internal/web/admin"
)

// Options configure New.
type Options struct {
	Config  config.Config
	Logger  *slog.Logger
	Version string
	// Image is the image this hub runs as its deployment names it (GRAVEL_IMAGE), reported on
	// /healthz with its digest when pinned by one (gravel#214); empty when the deployment says
	// nothing.
	Image string
	// Providers replaces the login providers the configuration describes. Tests use it to
	// drive the flows with a fake; nil means "build them from the configuration".
	Providers []identity.Registration
	// CloudProviders, Remote and Egress replace the cloud-VM driver's clouds, its SSH and the
	// hub's egress lookup (tests use a fake cloud); nil means the real ones.
	CloudProviders func(ctx context.Context, l cloudvm.Location) (cloud.Provider, error)
	Remote         cloudvm.Remote
	Egress         func(ctx context.Context) ([]string, error)
	// Games replaces the cloud-VM driver's game controls, by Game spec id.
	Games map[string]cloudvm.GameFactory
}

// Hub is a started-but-not-listening service: handlers are ready, Run serves them.
type Hub struct {
	cfg     config.Config
	logger  *slog.Logger
	version string
	image   string

	st     *store.Store
	org    *org.Service
	fleet  *servers.Fleet
	alarms *alarmSource
	// notify carries the events log to the settings' notifiers (ADR-0028 §5).
	notify     *notify.Dispatcher
	ids        *identity.Service
	sess       *session.Manager
	apps       *apps.Service
	servers    *servers.Service
	hosts      *servers.Hosts
	leases     *servers.Leases
	results    *results.Service
	cloudvm    *cloudvm.Manager
	monitor    *servers.Monitor
	moderation *servers.Moderation
	stats      *stats.Boards
	spend      *spend.Service // cloud spend (ADR-0034)
	// participation records first-party apps' participation events (ADR-0014).
	participation *stats.Participation
	feeds         *ingest.Keyring
	ingest        *ingest.Handler
	// Proof of control (gravel#5) and the limit on its failure answers in chat.
	proofs       *proof.Service
	proofReplies *ratelimit.Limiter
	jobs         *jobs.Runner
	web          *web.Handler
	perIP        *ratelimit.Limiter
	perUser      *ratelimit.Limiter
	perApp       *ratelimit.Limiter
	claimToken   string

	registry     *metricnames.Registry
	metrics      *httpx.Metrics
	authTotal    *prometheus.CounterVec
	limitedTotal *prometheus.CounterVec
	// oauthAuthorizations counts how the authorization endpoint ended (ADR-0015).
	oauthAuthorizations *prometheus.CounterVec
	// localLogins counts local account logins and registrations by result (ADR-0026).
	localLogins *prometheus.CounterVec
	// passwordResets counts password links by result (gravel#187).
	passwordResets *prometheus.CounterVec
	// metricsQueries counts the admin dashboard's metrics queries by result (gravel#204).
	metricsQueries *prometheus.CounterVec
	// eventsTotal counts events appended to the events log by kind (ADR-0028).
	eventsTotal *prometheus.CounterVec
	api         *http.ServeMux
	public      http.Handler
	internal    http.Handler

	listeners *listeners
}

// pruneEvery is how often expired sessions and attempts are deleted and idle rate-limit
// buckets dropped.
const pruneEvery = 10 * time.Minute

// CloudProviders are the clouds this hub makes VMs in, by the name a servers.yaml location uses.
func CloudProviders() []string { return []string{"digitalocean"} }

// DriverDeps are what a driver needs from the running hub.
type DriverDeps struct {
	// CloudVM is the cloud-VM driver's manager; without it the driver cannot run (servers check,
	// in CI, only needs the names).
	CloudVM *cloudvm.Manager
}

// Drivers are the drivers this hub has, by the name servers.yaml uses.
func Drivers(d DriverDeps) drivers.Registry {
	r := drivers.Registry{wardogsdriver.Name: wardogsdriver.Factory}
	if d.CloudVM != nil {
		r[cloudvm.Name] = d.CloudVM.Factory()
	} else {
		r[cloudvm.Name] = func(drivers.Target) (drivers.Driver, error) {
			return nil, errors.New("hub: the cloud-VM driver runs only inside the hub")
		}
	}
	return r
}

// DriverNames are the drivers this hub has.
func DriverNames() []string { return Drivers(DriverDeps{}).Names() }

// games are the cloud-VM driver's game controls, by Game spec id.
var games = map[string]cloudvm.GameFactory{"cs2": cs2game.New}

// defaultImages are the OS images the cloud-VM driver boots, by provider.
var defaultImages = map[string]string{"digitalocean": "fedora-44-x64"}

func (h *Hub) cloudProvider(ctx context.Context, l cloudvm.Location) (cloud.Provider, error) {
	return NewCloudProvider(l.Provider, l.Token, l.Project, h.version)
}

// NewCloudProvider is a cloud this hub makes VMs in (CloudProviders), for a token and project.
// `gravel-hub servers apply` uses it to walk scaling ladders against the cloud's sizes.
func NewCloudProvider(provider, token, project, version string) (cloud.Provider, error) {
	switch provider {
	case "digitalocean":
		return digitalocean.New(digitalocean.Config{Token: token, Project: project, UserAgent: "gravel-hub/" + version})
	}
	return nil, fmt.Errorf("hub: no cloud %q", provider)
}

// export writes the hub's archive (gravel#213): the tables from one snapshot, the settings and
// servers manifests beside them.
func (h *Hub) export(ctx context.Context, w io.Writer) error {
	files, name, err := archive.Readable(ctx, h.org, h.servers)
	if err != nil {
		return err
	}
	_, err = archive.Write(ctx, h.st, w, h.version, name, files, time.Now())
	return err
}

// egress is the hub's own address for the game hosts' firewalls: hub.yaml's, else asked.
func (h *Hub) egress(ctx context.Context) ([]string, error) {
	if len(h.cfg.Cloud.EgressCIDRs) > 0 {
		return h.cfg.Cloud.EgressCIDRs, nil
	}
	a, err := publicaddr.Detect(ctx, nil, publicaddr.TraceURL)
	if err != nil {
		return nil, err
	}
	return []string{a.String()}, nil
}

// New connects to the database, applies the migration policy, ensures the built-in organization
// and builds the handlers. It logs the owner-claim token when the hub is unowned.
func New(ctx context.Context, opts Options) (*Hub, error) {
	cfg, logger := opts.Config, opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	h := &Hub{cfg: cfg, logger: logger, version: opts.Version, image: opts.Image, listeners: newListeners()}
	go binarySHA256() // hashed once, off the first health check's path

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

	// A demo database is served only by a hub that says so on every page (gravel#112): made-up
	// members and matches never pass for real ones.
	seeded, err := st.HasDemoMembers(ctx)
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("hub: %w", err)
	}
	if seeded && !cfg.Demo {
		st.Close()
		return nil, errors.New("hub: the database holds demo members (gravel-hub demo seed); a hub serving it must set demo: true in hub.yaml, so every page says the data is made up")
	}
	if cfg.Demo {
		logger.Warn("this is a demo hub: every page says its members, matches and stats are made up", "seeded", seeded)
	}

	h.org = org.New(st, cfg.Claim.TokenTTL, logger).WithGames(servers.GameIDs()).WithDemo(cfg.Demo)
	o, token, err := h.org.EnsureBuiltin(ctx, cfg.Organization.Name)
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("hub: %w", err)
	}
	h.claimToken = token
	if token != "" {
		logger.Warn("the hub is unowned: claim it once with this token (it expires; a restart mints a new one)",
			"token", token, "expires_at", o.ClaimTokenExpiresAt.UTC().Format(time.RFC3339),
			"how", "log in, then run gravel claim <token>, paste it on the account page, or call ClaimOwnership on gravel.hub.v1.OrganizationService")
	} else {
		logger.Info("organization", "name", o.Name, "id", o.ID, "owned", true, "claimed_at", o.ClaimedAt, "owner_user_id", o.OwnerUserID)
	}

	regs := opts.Providers
	if regs == nil {
		regs = h.buildProviders()
	}
	idOpts := []identity.Option{identity.WithOrganizationName(o.Name)}
	if cfg.Auth.Local.Enabled { // ADR-0026: who may register is read from the settings at each call
		idOpts = append(idOpts, identity.WithLocalAccounts(st, func(ctx context.Context) (identity.LocalPolicy, error) {
			set, _, err := h.org.Settings(ctx)
			if err != nil {
				return identity.LocalPolicy{}, err
			}
			return identity.LocalPolicy{RegistrationOpen: set.LocalAccounts.Open(), MinPasswordLength: set.LocalAccounts.MinPasswordLength}, nil
		}), identity.WithPasswordLinks(st, func(result string) { h.passwordResets.WithLabelValues(result).Inc() }))
	}
	h.passwordResets = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gravel_password_resets_total", Help: "Password links (set and reset) by result: issued, used, invalid (an unknown, used or expired link), refused (a password the rules turned down).",
	}, []string{"result"})
	h.ids = identity.New(st, o.ID, regs, cfg.Auth.AttemptTTL, logger, idOpts...)
	h.sess = session.New(st, cfg.Auth.SessionTTL, cfg.Auth.Secure(), logger)
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
	if h.ids.LocalEnabled() {
		providerNames = append(providerNames, identity.ProviderLocal)
	}
	h.localLogins = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gravel_local_logins_total", Help: "Local account logins and registrations by result: ok, refused (an unknown username or a wrong password), locked, invalid (a malformed or closed registration), rate_limited, error.",
	}, []string{"result"})
	h.oauthAuthorizations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gravel_oauth_authorizations_total", Help: "Requests at the authorization endpoint by result (issued, denied, rejected, error).",
	}, []string{"result"})
	h.metricsQueries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gravel_metrics_queries_total", Help: "The admin dashboard's metrics queries the hub ran against its metrics store, by result: ok, unavailable, refused, too_large, no_store.",
	}, []string{"result"})
	h.registry.MustRegister(buildInfo, h.authTotal, h.limitedTotal, h.oauthAuthorizations, h.localLogins, h.passwordResets, h.metricsQueries, backup.NewCollector(cfg.Backup.StatusFile, st, logger), newStatsCollector(st, providerNames, logger))
	// First-party apps (ADR-0008, ADR-0015): app tokens live apps.token_ttl, members' tokens an hour.
	h.apps = apps.New(st, o.ID, cfg.Apps.TokenTTL, logger, apps.WithMetrics(h.registry))
	h.metrics = httpx.NewMetrics(h.registry)

	// Background work runs as supervised jobs (ADR-0010): the prune, and the servers' monitor,
	// which reconciles a poll job per server with what `servers apply` stored.
	h.jobs = jobs.New(logger, h.registry)
	h.jobs.Start(jobs.Job{Name: "prune", Every: pruneEvery, Run: func(ctx context.Context) error { h.Prune(ctx); return nil }})
	// The events log (ADR-0028): every source appends in its own transaction; the hub counts them,
	// and turns the backup status file into events.
	h.eventsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gravel_events_total", Help: "Events appended to the events log, by kind.",
	}, []string{"kind"})
	h.registry.MustRegister(h.eventsTotal)
	st.OnEvent(func(kind string) { h.eventsTotal.WithLabelValues(kind).Inc() })
	if cfg.Backup.StatusFile != "" {
		w := backup.NewWatcher(cfg.Backup.StatusFile, func(ctx context.Context) (store.Event, error) {
			o, err := h.org.Get(ctx)
			return store.Event{OrganizationID: o.ID}, err
		}, st.AppendEvent)
		h.jobs.Start(jobs.Job{Name: "backup_watch", Every: 5 * time.Minute, Immediate: true, Run: func(ctx context.Context) error { return w.Check(ctx, time.Now().UTC()) }})
	}
	h.servers = servers.New(st, o.ID, DriverNames(), logger).WithProviders(CloudProviders()...)
	// The cloud-VM driver (ADR-0003): the hub makes the VMs its servers run on. One manager owns
	// every host; the monitor's drivers read it.
	h.hosts = servers.NewHosts(h.servers, st, st, o.ID, logger)
	cvm := cloudvm.Options{Providers: opts.CloudProviders, Remote: opts.Remote, Egress: opts.Egress, DefaultImages: defaultImages,
		Runner: h.jobs, Logger: logger.With("driver", cloudvm.Name), Registerer: h.registry}
	if cvm.Providers == nil {
		cvm.Providers = h.cloudProvider
	}
	if cvm.Remote == nil {
		cvm.Remote = cloudvm.SSH{}
	}
	if cvm.Egress == nil {
		cvm.Egress = h.egress
	}
	gameControls := opts.Games
	if gameControls == nil {
		gameControls = games
	}
	h.cloudvm = cloudvm.NewManager(h.hosts, h.hosts, gameControls, cvm)
	h.hosts.Attach(h.cloudvm)
	// Server leases (#20, ADR-0023): a lease's password and plugins reach its server through the
	// spec the Manager reads, so the leases are loaded before it starts.
	h.leases = servers.NewLeases(h.servers, h.hosts, st, st, o.ID, []byte(cfg.Leases.Key), h.registry, logger)
	if err := h.leases.Load(ctx); err != nil {
		return nil, fmt.Errorf("hub: leases: %w", err)
	}
	// A CS2 lease's match config is MatchZy's JSON (games/cs2/matchzy): checked before it is taken.
	h.leases.SetMatchCheck("matchzy", func(b []byte) error {
		var c matchzy.Config
		if err := json.Unmarshal(b, &c); err != nil {
			return err
		}
		return c.Validate()
	})
	// Proof of control (gravel#5): a code typed in a cloud-VM server's chat proves the account the
	// log says typed it. Counter-Strike 2, the one game whose chat the hub reads, knows players by
	// their Steam account.
	h.proofs = proof.New(st, logger, []proof.Channel{proof.GameChat{ProviderName: cs2game.Provider}}, proof.WithMetrics(h.registry))
	h.proofReplies = newProofReplies()
	h.cloudvm.SetChatSink(h.chatProofs)
	registry := Drivers(DriverDeps{CloudVM: h.cloudvm})
	h.monitor = servers.NewMonitor(h.servers, registry, h.jobs, h.registry, "gravel-hub/"+h.version, logger)
	// Fleet health (gravel#190): the monitor writes a server's health transitions into the events
	// log; the fleet judges every server, and the alarms are computed from it and the hub's state.
	h.monitor.SetHealthEvents(healthEvents{st: st, orgID: o.ID})
	h.fleet = servers.NewFleet(h.servers, h.monitor, h.cloudvm, h.leases)
	// Notifiers (gravel#196): a dispatcher follows the log for each notifier the settings name,
	// with its secret from hub.yaml; one failing for an hour is an alarm.
	h.notify = notify.NewDispatcher(notify.Options{OrgID: o.ID, Log: st, Cursors: st, Logger: logger.With("job", "notify"), Registry: h.registry,
		Notifiers: func(ctx context.Context) ([]org.Notifier, error) {
			set, _, err := h.org.Settings(ctx)
			return set.Notifiers, err
		},
		Build: h.notifier(o.Name)})
	h.jobs.Start(jobs.Job{Name: "notify", Every: notify.Every, Immediate: true, Timeout: notify.MaxBatches*notify.SendTimeout + 10*time.Second, Run: h.notify.Run})
	h.alarms = &alarmSource{statusFile: cfg.Backup.StatusFile, archiver: st, now: time.Now, notifiers: h.notify, owned: func(ctx context.Context) (bool, time.Time, error) {
		o, err := h.org.Get(ctx)
		return o.Owned(), o.CreatedAt, err
	}}
	gauges := newFleetGauges(h.registry)
	h.jobs.Start(jobs.Job{Name: "fleet_health", Every: FleetEvery, Immediate: true, Run: func(ctx context.Context) error {
		snap, err := h.fleet.Snapshot(ctx)
		if err != nil {
			return err
		}
		alarms, err := h.alarms.Alarms(ctx, snap)
		if err != nil {
			return err
		}
		gauges.set(snap, alarms)
		return nil
	}})
	h.moderation = servers.NewModeration(h.servers, h.monitor, st, st, o.ID, h.registry, logger)
	h.monitor.SetPrimer(h.moderation.Prime)
	h.monitor.Start()
	h.cloudvm.Start()
	h.jobs.Start(jobs.Job{Name: "leases", Every: servers.LeaseEvery, Run: h.leases.Run})
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
	// Participation from first-party apps (ADR-0014) shares the change counter, so its boards are
	// cached like the combat boards.
	h.participation = stats.NewParticipation(st, o.ID, h.servers, servers.GameIDs(), statsSettings, changes, h.registry)
	// Cloud spend (ADR-0034): each cloud location's bill, where its token may read it, a snapshot
	// a day, beside the hub's own estimate.
	h.spend = spend.New(st, o.ID, func(ctx context.Context) ([]spend.Location, error) {
		locs, errs, err := h.hosts.CloudLocations(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]spend.Location, 0, len(locs))
		for i, l := range locs {
			sl := spend.Location{ID: l.ID, Provider: l.Provider, Err: errs[i]}
			if sl.Err == nil {
				sl.Cloud, sl.Err = h.cloudProvider(ctx, l)
			}
			out = append(out, sl)
		}
		return out, nil
	}, func(ctx context.Context) (*time.Location, error) {
		set, err := statsSettings(ctx)
		return set.Location(), err
	}, logger)
	h.jobs.Start(jobs.Job{Name: "cloud_spend", Every: spend.SnapshotEvery, Immediate: true, Run: h.spend.Snapshot})
	// Retention (ADR-0012 §6): raw rows past the window roll up into monthly totals.
	h.jobs.Start(jobs.Job{Name: "stats_rollup", Every: stats.RollupEvery, Immediate: true,
		Run: stats.NewRetention(st, o.ID, statsSettings, changes, h.registry, logger).Run})
	// Inbound ingestion (ADR-0011): the feed tokens are read from their files as the monitor reads
	// the credentials, and the stored batches are pruned after their retention.
	h.feeds = ingest.NewKeyring()
	h.ingest = ingest.NewHandler(h.feeds, st, o.ID, h.registry, logger)
	// A cloud-VM server's log push (CS2's logaddress_add_http) comes from its own host, is kept
	// redacted, and feeds the driver's roster, which is how its players are known.
	h.ingest.SetLogSource("cs2", ingest.LogSource{Headers: cs2.LogHeaders, Redact: cs2.RedactLog})
	h.ingest.SetLogTap(h.cloudvm)
	h.ingest.SetAddressPin(h.cloudvm)
	// Match results and demos from a server's match plugin (ADR-0024): from the server's own host,
	// demos to the object store when the hub has a bucket.
	var objects objectstore.Store
	if d := cfg.Demos; d.Enabled() {
		s3, err := objectstore.NewS3(objectstore.S3Config{Endpoint: d.Endpoint, Region: d.Region, Bucket: d.Bucket, AccessKey: d.AccessKey,
			SecretKey: d.SecretKey, PathStyle: d.PathStyle})
		if err != nil {
			return nil, fmt.Errorf("hub: demos: %w", err)
		}
		objects = s3
	}
	mapName := func(server string) string {
		if o, ok := h.monitor.Observation(server); ok {
			return o.Status.Map
		}
		return ""
	}
	// A result keeps the stat extensions its server's game declares (ADR-0033).
	extensions := func(ctx context.Context, server string) []string {
		srv, err := h.servers.Server(ctx, server)
		if err != nil {
			return nil
		}
		sp, _ := h.servers.Spec(srv.Game)
		return sp.Stats.Names()
	}
	h.results = results.New(st, o.ID, h.leases, mapName, extensions, objects, int64(cfg.Demos.MaxMiB)<<20, h.registry, logger)
	h.ingest.SetPluginSinks(h.results, h.results)
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
	h.web.ObserveAuthorization = func(result string) { h.oauthAuthorizations.WithLabelValues(result).Inc() }
	h.web.ObserveLocal = func(result string) { h.localLogins.WithLabelValues(result).Inc() }
	// Local logins hash a password each (ADR-0026): 10 a minute per address, 5 at once, on top of
	// the pages' own per-address limit.
	h.web.LocalLimiter = ratelimit.New(10, 5)

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
// notifier builds a notifier from its settings entry with its secret and the mail server from
// hub.yaml.
func (h *Hub) notifier(orgName string) func(n org.Notifier) (notify.Notifier, error) {
	smtp := h.cfg.Notify.SMTP
	d := notify.Deps{Hub: orgName, Client: &http.Client{Timeout: notify.SendTimeout},
		SMTP: notify.SMTP{Host: smtp.Host, Port: smtp.PortOrDefault(), Username: smtp.Username, Password: smtp.Password, From: smtp.From, TLS: smtp.TLSMode()}}
	if h.cfg.Auth.BaseURL != "" {
		d.Link = h.cfg.Auth.BaseURL + "/admin"
	}
	return func(n org.Notifier) (notify.Notifier, error) {
		d := d
		d.Secret = h.cfg.Notify.Secret(n.Name)
		return notify.New(n, d)
	}
}

func (h *Hub) Handler() http.Handler { return h.public }

// InternalHandler serves /metrics and /debug/pprof/. Keep it off the public network.
func (h *Hub) InternalHandler() http.Handler { return h.internal }

// OwnerClaimToken is the token minted at start, or "" when the hub is owned.
func (h *Hub) OwnerClaimToken() string { return h.claimToken }

// Close releases the database pool. Run calls it; call it yourself when you only used Handler.
func (h *Hub) Close() { h.st.Close() }

// scopes are the rate-limit buckets: per app when an app's own bearer token is carried, per user
// when logged in or calling with a member's token (an app acting for many members is limited per
// member), else per client address. An API call a page makes in process is not counted again: the
// page request was.
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
			if m, ok := apps.MemberFromContext(r.Context()); ok {
				return m.UserID.String()
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
	// A member's token (ADR-0015) is held to its scopes before any procedure runs.
	interceptors := connect.WithInterceptors(h.metrics.Interceptor(), ratelimit.Interceptor(h.scopes(), h.rejected), api.MemberTokens())
	access := api.NewAccess(h.org, h.ids, h.logger).WithServers(h.servers) // who is calling, for every capability check (ADR-0027)
	mux.Handle(hubv1connect.NewOrganizationServiceHandler(api.NewOrganizationServer(h.org, access, h.logger).WithEvents(h.st).WithNotifiers(h.notify).WithExport(h.export), interceptors))
	identityAPI := api.NewIdentityServer(h.ids, h.sess, h.org, access, h.logger).WithBaseURL(h.cfg.Auth.BaseURL)
	identityAPI.SetProofs(h.proofs, h.servers, h.monitor)
	mux.Handle(hubv1connect.NewIdentityServiceHandler(identityAPI, interceptors))
	// The servers manifest over the API (#195) is applied as `gravel-hub servers apply` does on the
	// host: the credential files checked, a scaling ladder against the cloud's sizes.
	applyOpts := servers.ApplyOptions{CheckCredentials: true, Clouds: func(_ context.Context, l servers.Location, token string) (cloud.Provider, error) {
		return NewCloudProvider(l.Provider, token, l.Project, h.version)
	}}
	mux.Handle(hubv1connect.NewServerServiceHandler(api.NewServerServer(h.servers, h.monitor, h.ids, access, h.logger).WithSharedPasswords(h.hosts).
		WithFleet(h.fleet, h.alarms).WithManifests(h.servers, applyOpts), interceptors))
	moderation := api.NewModerationServer(h.moderation, access, h.ids, h.logger)
	mux.Handle(hubv1connect.NewModerationServiceHandler(moderation, interceptors))
	mux.Handle(hubv1connect.NewServerHostServiceHandler(api.NewHostServer(h.hosts, access, h.logger).WithSpend(h.spend), interceptors))
	mux.Handle(hubv1connect.NewMetricsServiceHandler(api.NewMetricsServer(dashboards.All(), h.cfg.Metrics, h.metricsStore(),
		access, func(r string) { h.metricsQueries.WithLabelValues(r).Inc() }, h.logger), interceptors))
	mux.Handle(hubv1connect.NewServerLeaseServiceHandler(api.NewLeaseServer(h.leases, access, h.logger), interceptors))
	mux.Handle(hubv1connect.NewServerConfigServiceHandler(api.NewConfigServer(h.moderation, access, h.logger), interceptors))
	statsAPI := api.NewStatsServer(h.stats, h.participation, access, h.ids, h.logger)
	statsAPI.SetResults(h.results)
	statsAPI.WithEraser(func(ctx context.Context, key store.PlayerKey, by store.Actor) (store.Erasure, error) {
		o, err := h.org.Get(ctx)
		if err != nil {
			return store.Erasure{}, err
		}
		return stats.Erase(ctx, h.st, o.ID, key, by, time.Now().UTC())
	})
	mux.Handle(hubv1connect.NewStatsServiceHandler(statsAPI, interceptors))
	mux.Handle(hubv1connect.NewAppServiceHandler(api.NewAppServer(h.apps, h.logger).WithAccess(access), interceptors))
	return mux
}

func (h *Hub) rejected(scope string) { h.limitedTotal.WithLabelValues(scope).Inc() }

func (h *Hub) buildPublic() http.Handler {
	scopes := h.scopes()
	mux := http.NewServeMux()
	services := []string{hubv1connect.OrganizationServiceName, hubv1connect.IdentityServiceName, hubv1connect.ServerServiceName,
		hubv1connect.ModerationServiceName, hubv1connect.ServerHostServiceName, hubv1connect.ServerLeaseServiceName, hubv1connect.ServerConfigServiceName,
		hubv1connect.StatsServiceName, hubv1connect.AppServiceName, hubv1connect.MetricsServiceName}
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
	// The token endpoint (ADR-0008, ADR-0015): client credentials, or an authorization code with
	// its PKCE verifier, in; a Bearer token out. Introspection (RFC 7662) answers an app whether a
	// token it was handed is live. Both authenticate the app's client credentials and are limited
	// per client address, like anything anonymous; /oauth/authorize is a page.
	mux.Handle("/oauth/token", h.metrics.HTTP("oauth_token", ratelimit.Middleware(scopes, h.rejected)(h.apps.TokenHandler())))
	mux.Handle("/oauth/introspect", h.metrics.HTTP("oauth_introspect", ratelimit.Middleware(scopes, h.rejected)(h.apps.IntrospectHandler())))

	// The pages, rate-limited as HTTP; the procedures above are limited by the interceptor.
	pages := http.NewServeMux()
	// The admin dashboard (ADR-0029) first: a static client of the procedures above, at /admin.
	dashboard := admin.New(admin.Bundle())
	if !dashboard.Built() {
		h.logger.Warn("this binary was built without the admin dashboard: /admin answers 503 (make build includes it)")
	}
	dashboard.Register(pages)
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
	top.Handle("POST "+ingest.LogPath, h.metrics.HTTP("ingest_log", http.HandlerFunc(h.ingest.ServeLog)))
	top.Handle("POST "+ingest.EventsPath, h.metrics.HTTP("ingest_events", http.HandlerFunc(h.ingest.ServeEvents)))
	top.Handle("POST "+ingest.DemoPath, h.metrics.HTTP("ingest_demo", http.HandlerFunc(h.ingest.ServeDemo)))
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

// healthz says the process runs, and what it is (gravel#214): the version, the image its
// deployment names with that image's digest, and the SHA-256 of the executable itself, which is
// true however the hub was deployed.
// metricsStore is the store the operator dashboards read (gravel#204), nil when hub.yaml names none.
func (h *Hub) metricsStore() api.Querier {
	m := h.cfg.Metrics
	if !m.Configured() {
		return nil
	}
	c, err := promquery.New(m.QueryURL, m.BearerToken, &http.Client{Timeout: m.QueryTimeout()})
	if err != nil { // config.Validate checked the address
		h.logger.Error("the metrics store's address is unusable", "error", err.Error())
		return nil
	}
	return c
}

func (h *Hub) healthz(w http.ResponseWriter, _ *http.Request) {
	body := map[string]string{"status": "ok", "version": h.version, "binary_sha256": binarySHA256()}
	if h.image != "" {
		body["image"] = h.image
		if d := imageDigest(h.image); d != "" {
			body["digest"] = d
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// digestRE is an image reference's pinned digest.
var digestRE = regexp.MustCompile(`@(sha256:[0-9a-f]{64})$`)

// imageDigest is the digest an image reference is pinned to, or "".
func imageDigest(ref string) string {
	if m := digestRE.FindStringSubmatch(ref); m != nil {
		return m[1]
	}
	return ""
}

// binarySHA256 is the hex SHA-256 of the running executable, read once; "" when it can't be read.
var binarySHA256 = sync.OnceValue(func() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return ""
	}
	return hex.EncodeToString(sum.Sum(nil))
})

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
	var pruned int64
	if o, err := h.org.Get(ctx); err == nil {
		set, _, err := h.org.Settings(ctx)
		if err == nil {
			days := set.Events.RetentionDaysOrDefault()
			if pruned, err = h.st.PruneEvents(ctx, o.ID, time.Now().UTC().AddDate(0, 0, -days)); err != nil {
				h.logger.Warn("could not prune events", "error", err.Error())
			}
		}
	}
	tokens, err := h.apps.Prune(ctx)
	if err != nil {
		h.logger.Warn("could not prune app tokens", "error", err.Error())
	}
	var proofs int64
	if h.proofs != nil {
		if proofs, err = h.proofs.Prune(ctx); err != nil {
			h.logger.Warn("could not prune identity proofs", "error", err.Error())
		}
		h.proofReplies.Sweep()
	}
	ips, users, appsSwept := h.perIP.Sweep(), h.perUser.Sweep(), h.perApp.Sweep()
	h.ingest.Sweep()
	if h.web != nil && h.web.LocalLimiter != nil {
		h.web.LocalLimiter.Sweep()
	}
	h.logger.Debug("pruned", "events", pruned, "sessions", sessions, "attempts", attempts, "app_tokens", tokens, "proofs", proofs, "ip_buckets", ips, "user_buckets", users, "app_buckets", appsSwept)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// MetricNames are the names of every metric the hub exposes on its internal listener, including
// those with no sample yet (docs/hub.md, Observability; the dashboards' drift test reads them).
func (h *Hub) MetricNames() []string { return h.registry.Names() }
