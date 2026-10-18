package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/gravel-project/gravel/games/wardogs/wardogsim"
	"github.com/gravel-project/gravel/internal/config"
	"github.com/gravel-project/gravel/internal/demo"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/stats"
	"github.com/gravel-project/gravel/internal/store"
)

const (
	demoUsage       = "usage: gravel-hub demo wardogs-server|seed [flags]"
	simUsage        = "usage: gravel-hub demo wardogs-server --listen ADDR --token-file FILE [--profile FILE] [--seed N] [--name NAME] [--build CL-…] [--feed-url URL --feed-token-file FILE]"
	seedUsage       = "usage: gravel-hub demo seed [--config hub.yaml] [--days 90] [--members 40] [--seed N] [--profile FILE]"
	simShutdownWait = 5 * time.Second
)

// demoCmd is `gravel-hub demo`: a demo hub's two halves (gravel#112). `wardogs-server` serves a
// simulated War Dogs server for the hub to poll; `seed` writes made-up history into a demo hub's
// database, and only a demo hub's (demo: true in hub.yaml), and only an empty one.
func demoCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, demoUsage)
		return 2
	}
	switch args[0] {
	case "wardogs-server":
		return demoWardogsServer(ctx, args[1:], stdout, stderr)
	case "seed":
		return demoSeed(ctx, args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "gravel-hub demo: unknown command %q\n%s\n", args[0], demoUsage)
	return 2
}

// readProfile reads a calibration file, or gives the default profile for "".
func readProfile(path string) (wardogsim.Profile, error) {
	if path == "" {
		return wardogsim.DefaultProfile(), nil
	}
	f, err := os.Open(path)
	if err != nil {
		return wardogsim.Profile{}, err
	}
	defer func() { _ = f.Close() }()
	p, err := wardogsim.ParseProfile(f)
	if err != nil {
		return wardogsim.Profile{}, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// readSecret reads a token file's first line; an empty file is an error.
func readSecret(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(string(data), "\n")
	if line = strings.TrimSpace(line); line == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return line, nil
}

func demoWardogsServer(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("demo wardogs-server", stderr)
	listen := fs.String("listen", "", "the address to serve the RCON API on, host:port")
	tokenFile := fs.String("token-file", "", "a file holding the RCON password the simulated server accepts")
	profilePath := fs.String("profile", "", "a calibration file (population by hour, match length, kills, rejoins); the built-in profile without one")
	seed := fs.Uint64("seed", 1, "the simulation's seed: the same seed and clock give the same world")
	build := fs.String("build", "", "the recorded build to answer as (games/wardogs/testdata); the newest without one")
	feedURL := fs.String("feed-url", "", "start with the feed's Url set, as the host's own file would: Season 02 posts to it as given (https://hub.example.com/api/ingest/events); CL-509546 appends /api/ingest/events")
	feedTokenFile := fs.String("feed-token-file", "", "a file holding the feed's token, with --feed-url")
	name := fs.String("name", "", "the name the server reports (ServerName); a demo name without one")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *listen == "" || *tokenFile == "" || fs.NArg() > 0 || (*feedTokenFile != "" && *feedURL == "") {
		fmt.Fprintln(stderr, simUsage)
		return 2
	}
	token, err := readSecret(*tokenFile)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	feedToken := ""
	if *feedTokenFile != "" {
		if feedToken, err = readSecret(*feedTokenFile); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	profile, err := readProfile(*profilePath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	logger := slog.New(slog.NewJSONHandler(stdout, nil))
	sim, err := wardogsim.New(wardogsim.Options{Token: token, Profile: profile, Seed: *seed, Build: *build,
		ServerName: *name, FeedURL: *feedURL, FeedToken: feedToken, Logger: logger})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", *listen)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Handler: sim.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	go func() { _ = sim.Run(ctx) }()
	feed := sim.FeedTarget()
	if feed == "" {
		feed = "off"
	}
	logger.Info("simulating a War Dogs server: its players, matches and stats are made up", "build", sim.Build(), "listen", ln.Addr().String(),
		"profile", orDefault(*profilePath, "built-in"), "seed", *seed, "feed", feed)
	select {
	case err := <-served:
		fmt.Fprintln(stderr, err)
		return 1
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), simShutdownWait)
	defer cancel()
	_ = srv.Shutdown(sctx)
	logger.Info("stopped")
	return 0
}

func orDefault(s, otherwise string) string {
	if s == "" {
		return otherwise
	}
	return s
}

func demoSeed(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("demo seed", stderr)
	path := configFlag(fs)
	days := fs.Int("days", demo.DefaultDays, "how many days of history, ending now")
	members := fs.Int("members", demo.DefaultMembers, "how many made-up members")
	seed := fs.Uint64("seed", 1, "the history's seed: the same seed gives the same members and matches")
	profilePath := fs.String("profile", "", "the calibration the history takes its shapes from; the built-in profile without one")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, seedUsage)
		return 2
	}
	cfg, ok := load(*path, stderr)
	if !ok {
		return 1
	}
	// The first refusal, before the database is opened: only a hub that says it is a demo.
	if !cfg.Demo {
		fmt.Fprintln(stderr, demo.ErrNotDemo)
		return 1
	}
	profile, err := readProfile(*profilePath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	logger := cfg.Log.NewLogger(io.Discard)
	st, err := openDemoStore(ctx, cfg, logger)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer st.Close()
	organization, err := org.New(st, cfg.Claim.TokenTTL, logger).CreateBuiltinIfAbsent(ctx, cfg.Organization.Name)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	sum, keys, err := demo.Seed(ctx, st, organization.ID, cfg.Demo, demo.Options{Days: *days, Members: *members, Seed: *seed, Profile: profile, Now: time.Now()})
	if err != nil {
		if errors.Is(err, store.ErrNotADemoDatabase) {
			fmt.Fprintln(stderr, "refused: nothing was written.")
		}
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "seeded %d days of made-up history (seed %d): %d members (%d moderators, %d showing their names), %d audit entries, %d bans (%d lifted)\n",
		*days, *seed, sum.Members, sum.Moderators, sum.ShowingNames, sum.AuditEntries, sum.Bans, sum.Lifted)
	for _, id := range slices.Sorted(maps.Keys(sum.Matches)) {
		fmt.Fprintf(stdout, "  %s: %d matches, %d player rows\n", id, sum.Matches[id], sum.Rows[id])
	}
	for _, id := range sum.Skipped {
		fmt.Fprintf(stdout, "  %s: not War Dogs, no matches\n", id)
	}
	if cfg.Stats.PseudonymKey == "" {
		fmt.Fprintln(stdout, "no pseudonym key (stats.pseudonym_key_file): unlinked players show as unnamed; set one before serving")
		return 0
	}
	if _, err := stats.NewNamer(st, organization.ID, []byte(cfg.Stats.PseudonymKey), logger).Names(ctx, keys); err != nil {
		fmt.Fprintln(stderr, "pseudonyms:", err)
		return 1
	}
	fmt.Fprintf(stdout, "pseudonyms for %d players\n", len(keys))
	return 0
}

// openDemoStore opens the database and applies the hub's migration policy, as serve does.
func openDemoStore(ctx context.Context, cfg config.Config, logger *slog.Logger) (*store.Store, error) {
	st, err := store.Open(ctx, cfg.Database.URL, 2, cfg.Database.ConnectTimeout, logger)
	if err != nil {
		return nil, err
	}
	if cfg.Database.Migrate == config.MigrateAuto {
		err = st.Migrate(ctx)
	} else {
		err = st.Ready(ctx)
	}
	if err != nil {
		st.Close()
		return nil, err
	}
	return st, nil
}
