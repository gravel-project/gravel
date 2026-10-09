// Package cli is the command line of a bot built on gravel's runtime: serve, config check,
// healthcheck and version, the config path from a flag, an environment variable or a default,
// and the stock modules plus the host's own. gravel-bot is a Program with no extra modules; a
// host's main is a Program literal (docs/bot.md, "A host's bot").
package cli

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

	"github.com/gravel-project/gravel/discord/bot"
	"github.com/gravel-project/gravel/discord/bot/stock"
	"github.com/gravel-project/gravel/discord/hubclient"
)

// Program describes one bot binary.
type Program struct {
	// Name is the binary's name, used in usage, errors and `version`.
	Name string
	// Version is the build's version; "dev" or "" falls back to the VCS revision.
	Version string
	// DefaultConfig is the config path when neither --config nor ConfigEnv names one.
	DefaultConfig string
	// ConfigEnv is the environment variable that names the config path.
	ConfigEnv string
	// About is an optional paragraph for the usage text.
	About string
	// Modules returns the host's own modules, registered after the stock set; nil for none.
	Modules func(cfg bot.Config, hub *hubclient.Client) []bot.Module
}

// Main runs the program with the process's arguments and returns the exit code.
func (p Program) Main() int {
	return p.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr)
}

// Run runs one command: 0 on success, 1 on failure, 2 on a usage error.
func (p Program) Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cmd := "serve"
	if len(args) > 0 && !isFlag(args[0]) {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		return p.serve(ctx, args, stdout, stderr)
	case "config":
		if len(args) == 0 || args[0] != "check" {
			fmt.Fprintf(stderr, "usage: %s config check [--config bot.yaml]\n", p.Name)
			return 2
		}
		return p.configCheck(args[1:], stdout, stderr)
	case "healthcheck":
		return p.healthcheck(ctx, args, stderr)
	case "version":
		fmt.Fprintln(stdout, p.Name, p.BuildVersion())
		return 0
	case "help", "-h", "--help":
		p.usage(stdout)
		return 0
	}
	fmt.Fprintf(stderr, "%s: unknown command %q\n", p.Name, cmd)
	p.usage(stderr)
	return 2
}

// BuildVersion is Version, or "dev-<revision>" from the build info when Version is "dev" or empty.
func (p Program) BuildVersion() string {
	v := strings.TrimSpace(p.Version)
	if v != "" && v != "dev" {
		return v
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				return "dev-" + s.Value[:7]
			}
		}
	}
	return "dev"
}

// BuildModules builds the hub client and returns the stock modules the configuration enables,
// then the host's.
func (p Program) BuildModules(cfg bot.Config) ([]bot.Module, error) {
	hub, err := hubclient.New(hubclient.Config{URL: cfg.Hub.URL, ClientID: cfg.Hub.ClientID, ClientSecret: cfg.Hub.ClientSecret})
	if err != nil {
		return nil, err
	}
	mods := stock.Modules(cfg, hub)
	if p.Modules != nil {
		mods = append(mods, p.Modules(cfg, hub)...)
	}
	return mods, nil
}

func isFlag(s string) bool { return len(s) > 0 && s[0] == '-' }

func (p Program) usage(w io.Writer) {
	about := ""
	if p.About != "" {
		about = "\n" + p.About + "\n"
	}
	fmt.Fprintf(w, `usage: %s <command> [flags]

  serve         run the bot (default); --config bot.yaml
  config check  load and validate the configuration, and list the modules
  healthcheck   GET /healthz and exit 0 when it answers; --url
  version       print the version
%s
The config path comes from --config, then $%s, then %s.
`, p.Name, about, p.ConfigEnv, p.DefaultConfig)
}

func (p Program) newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(p.Name+" "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func (p Program) configFlag(fs *flag.FlagSet) *string {
	def := ""
	if p.ConfigEnv != "" {
		def = os.Getenv(p.ConfigEnv)
	}
	if def == "" {
		def = p.DefaultConfig
	}
	return fs.String("config", def, "path to bot.yaml")
}

func load(path string, stderr io.Writer) (bot.Config, bool) {
	cfg, err := bot.Load(path)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return bot.Config{}, false
	}
	return cfg, true
}

func (p Program) serve(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := p.newFlagSet("serve", stderr)
	path := p.configFlag(fs)
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
	mods, err := p.BuildModules(cfg)
	if err != nil {
		logger.Error("start failed", "error", err.Error())
		return 1
	}
	rt, err := bot.New(bot.Options{Config: cfg, Logger: logger, Version: p.BuildVersion()}, mods...)
	if err != nil {
		logger.Error("start failed", "error", err.Error())
		return 1
	}
	if err := rt.Run(ctx); err != nil {
		logger.Error("stopped with error", "error", err.Error())
		return 1
	}
	return 0
}

func (p Program) configCheck(args []string, stdout, stderr io.Writer) int {
	fs := p.newFlagSet("config check", stderr)
	path := p.configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, ok := load(*path, stderr)
	if !ok {
		return 1
	}
	mods, err := p.BuildModules(cfg)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	names := make([]string, 0, len(mods))
	for _, m := range mods {
		names = append(names, m.Name())
	}
	guilds := "global"
	if n := len(cfg.Discord.GuildIDs); n > 0 {
		guilds = fmt.Sprintf("%d guild(s)", n)
	}
	roleSync := "off"
	if cfg.RoleSync.Enabled {
		roleSync = fmt.Sprintf("every %s, poll %s", cfg.RoleSync.Interval, cfg.RoleSync.PollInterval)
		if cfg.RoleSync.DryRun {
			roleSync += ", dry run"
		}
	}
	fmt.Fprintf(stdout, "config ok: application %s, commands %s, gateway %t, hub %s (public %s, client %s), token from %s, hub secret from %s, role sync %s, linked roles %t, listen %s, internal %s, modules %s\n",
		cfg.Discord.ApplicationID, guilds, cfg.Discord.Gateway, cfg.Hub.URL, cfg.Hub.PublicURL, cfg.Hub.ClientID,
		cfg.Discord.TokenSource(), cfg.Hub.SecretSource(), roleSync, cfg.LinkedRoles.Enabled, cfg.Server.Listen, cfg.Server.InternalListen,
		strings.Join(names, ", "))
	return 0
}

func (p Program) healthcheck(ctx context.Context, args []string, stderr io.Writer) int {
	fs := p.newFlagSet("healthcheck", stderr)
	url := fs.String("url", "http://127.0.0.1:8081/healthz", "health endpoint to probe")
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
