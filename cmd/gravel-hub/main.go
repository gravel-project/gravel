// Command gravel-hub runs the gravel hub.
//
//	gravel-hub serve        [--config hub.yaml]           serve the API (default)
//	gravel-hub migrate      [--config hub.yaml] [--status] apply pending migrations, or show them
//	gravel-hub config check [--config hub.yaml]           load and validate the configuration
//	gravel-hub healthcheck  [--url http://127.0.0.1:8080/healthz]
//	gravel-hub settings export [--config hub.yaml]              print the Organization settings as a manifest
//	gravel-hub settings apply <manifest.yaml> [--dry-run]       apply a manifest (exit 3 on --dry-run with changes)
//	gravel-hub settings check <manifest.yaml>                   validate a manifest; no config, no database
//	gravel-hub apps create --name NAME --scopes SCOPES          register a first-party app; prints its secret once
//	gravel-hub apps list                                        the registered apps
//	gravel-hub apps revoke --client-id ID                       revoke an app and its tokens
//	gravel-hub servers export [--config hub.yaml]               print the games and servers as a servers.yaml manifest
//	gravel-hub servers apply <servers.yaml> [--dry-run]         apply a servers manifest (exit 3 on --dry-run with changes)
//	gravel-hub servers check <servers.yaml>                     validate a servers manifest; no config, no database
//	gravel-hub wardogs record --base-url URL [--token-file F]   record a War Dogs server's reads as fixtures
//	gravel-hub version
//
// The config path defaults to $GRAVEL_CONFIG, then /etc/gravel/hub.yaml.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/gravel-project/gravel/internal/config"
	"github.com/gravel-project/gravel/internal/hub"
	"github.com/gravel-project/gravel/internal/store"
)

// version is set by the linker (-X main.version=...); ko and goreleaser both do.
var version = "dev"

const defaultConfigPath = "/etc/gravel/hub.yaml"

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cmd := "serve"
	if len(args) > 0 && !isFlag(args[0]) {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		return serve(ctx, args, stdout, stderr)
	case "migrate":
		return migrate(ctx, args, stdout, stderr)
	case "config":
		if len(args) == 0 || args[0] != "check" {
			fmt.Fprintln(stderr, "usage: gravel-hub config check [--config hub.yaml]")
			return 2
		}
		return configCheck(args[1:], stdout, stderr)
	case "healthcheck":
		return healthcheck(ctx, args, stderr)
	case "settings":
		return settings(ctx, args, stdout, stderr)
	case "apps":
		return appsCmd(ctx, args, stdout, stderr)
	case "servers":
		return serversCmd(ctx, args, stdout, stderr)
	case "wardogs":
		return wardogsCmd(ctx, args, stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, "gravel-hub", buildVersion())
		return 0
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	}
	fmt.Fprintf(stderr, "gravel-hub: unknown command %q\n", cmd)
	usage(stderr)
	return 2
}

func isFlag(s string) bool { return len(s) > 0 && s[0] == '-' }

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: gravel-hub <command> [flags]

  serve         serve the API (default); --config hub.yaml
  migrate       apply pending migrations; --status shows them instead
  config check  load and validate the configuration
  healthcheck   GET /healthz and exit 0 when it answers; --url
  settings      export the Organization settings as a manifest, apply one (--dry-run exits 3 on a change),
                or check one without a config or database
  apps          register, list or revoke first-party apps (create prints the client secret once)
  servers       export the games and servers as a servers.yaml manifest, apply one (--dry-run exits 3 on a
                change), or check one without a config or database
  wardogs       record a War Dogs server's read-only answers as per-build test fixtures
  version       print the version

The config path comes from --config, then $GRAVEL_CONFIG, then `+defaultConfigPath+`.
`)
}

func configFlag(fs *flag.FlagSet) *string {
	def := os.Getenv("GRAVEL_CONFIG")
	if def == "" {
		def = defaultConfigPath
	}
	return fs.String("config", def, "path to hub.yaml")
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("gravel-hub "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func load(path string, stderr io.Writer) (config.Config, bool) {
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return config.Config{}, false
	}
	return cfg, true
}

func serve(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("serve", stderr)
	path := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, ok := load(*path, stderr)
	if !ok {
		return 1
	}
	logger := cfg.Log.NewLogger(stdout)
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	h, err := hub.New(ctx, hub.Options{Config: cfg, Logger: logger, Version: buildVersion()})
	if err != nil {
		logger.Error("start failed", "error", err.Error())
		return 1
	}
	if err := h.Run(ctx); err != nil {
		logger.Error("stopped with error", "error", err.Error())
		return 1
	}
	return 0
}

func migrate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("migrate", stderr)
	path := configFlag(fs)
	status := fs.Bool("status", false, "show the schema version and pending migrations; change nothing")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, ok := load(*path, stderr)
	if !ok {
		return 1
	}
	logger := cfg.Log.NewLogger(stdout)
	st, err := store.Open(ctx, cfg.Database.URL, 1, cfg.Database.ConnectTimeout, logger)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer st.Close()
	if !*status {
		if err := st.Migrate(ctx); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	s, err := st.Status(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "schema version %d, latest %d, pending %d\n", s.Current, s.Latest, s.Pending)
	if *status && s.Pending > 0 {
		return 3
	}
	return 0
}

func configCheck(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("config check", stderr)
	path := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, ok := load(*path, stderr)
	if !ok {
		return 1
	}
	fmt.Fprintf(stdout, "config OK: organization %q, listen %s, internal %s, database %s (from %s), migrate %s, login %s\n",
		cfg.Organization.Name, cfg.Server.Listen, cfg.Server.InternalListen, cfg.Database.RedactedURL(), cfg.Database.Source(), cfg.Database.Migrate, authSummary(cfg))
	return 0
}

// authSummary describes the login providers without any secret: which are enabled, whether
// they log in or only link, and where each secret came from.
func authSummary(cfg config.Config) string {
	var parts []string
	role := func(login bool) string {
		if login {
			return "login"
		}
		return "link-only"
	}
	if cfg.Auth.Discord.Enabled {
		parts = append(parts, fmt.Sprintf("discord (%s, secret from %s)", role(cfg.Auth.Discord.Login), cfg.Auth.Discord.SecretSource()))
	}
	if cfg.Auth.Steam.Enabled {
		key := cfg.Auth.Steam.KeySource()
		if key == "" {
			key = "no api key"
		} else {
			key = "key from " + key
		}
		parts = append(parts, fmt.Sprintf("steam (%s, %s)", role(cfg.Auth.Steam.Login), key))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ") + " at " + cfg.Auth.BaseURL
}

func healthcheck(ctx context.Context, args []string, stderr io.Writer) int {
	fs := newFlagSet("healthcheck", stderr)
	url := fs.String("url", "http://127.0.0.1:8080/healthz", "health endpoint to probe")
	timeout := fs.Duration("timeout", 3*time.Second, "give up after")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, *url, nil)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(stderr, "unhealthy:", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		fmt.Fprintf(stderr, "unhealthy: %s: %s\n", resp.Status, body)
		return 1
	}
	return 0
}

func buildVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}
