// Command gravel-bot runs gravel's Discord bot with the stock modules (docs/bot.md).
//
//	gravel-bot serve        [--config bot.yaml]            run the bot (default)
//	gravel-bot config check [--config bot.yaml]            load and validate the configuration
//	gravel-bot healthcheck  [--url http://127.0.0.1:8081/healthz]
//	gravel-bot version
//
// The config path defaults to $GRAVEL_BOT_CONFIG, then /etc/gravel/bot.yaml.
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

	"github.com/gravel-project/gravel/discord/bot"
	"github.com/gravel-project/gravel/discord/hubclient"
	"github.com/gravel-project/gravel/discord/modules/core"
)

// version is set by the linker (-X main.version=...); ko and goreleaser both do.
var version = "dev"

const defaultConfigPath = "/etc/gravel/bot.yaml"

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
	case "config":
		if len(args) == 0 || args[0] != "check" {
			fmt.Fprintln(stderr, "usage: gravel-bot config check [--config bot.yaml]")
			return 2
		}
		return configCheck(args[1:], stdout, stderr)
	case "healthcheck":
		return healthcheck(ctx, args, stderr)
	case "version":
		fmt.Fprintln(stdout, "gravel-bot", buildVersion())
		return 0
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	}
	fmt.Fprintf(stderr, "gravel-bot: unknown command %q\n", cmd)
	usage(stderr)
	return 2
}

func isFlag(s string) bool { return len(s) > 0 && s[0] == '-' }

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: gravel-bot <command> [flags]

  serve         run the bot (default); --config bot.yaml
  config check  load and validate the configuration
  healthcheck   GET /healthz and exit 0 when it answers; --url
  version       print the version

The config path comes from --config, then $GRAVEL_BOT_CONFIG, then `+defaultConfigPath+`.
`)
}

func configFlag(fs *flag.FlagSet) *string {
	def := os.Getenv("GRAVEL_BOT_CONFIG")
	if def == "" {
		def = defaultConfigPath
	}
	return fs.String("config", def, "path to bot.yaml")
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("gravel-bot "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func load(path string, stderr io.Writer) (bot.Config, bool) {
	cfg, err := bot.Load(path)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return bot.Config{}, false
	}
	return cfg, true
}

// modules are the stock modules, built against the configuration.
func modules(cfg bot.Config) ([]bot.Module, error) {
	hub, err := hubclient.New(hubclient.Config{URL: cfg.Hub.URL, ClientID: cfg.Hub.ClientID, ClientSecret: cfg.Hub.ClientSecret})
	if err != nil {
		return nil, err
	}
	return []bot.Module{core.New(hub, cfg.Hub.PublicURL)}, nil
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
	mods, err := modules(cfg)
	if err != nil {
		logger.Error("start failed", "error", err.Error())
		return 1
	}
	rt, err := bot.New(bot.Options{Config: cfg, Logger: logger, Version: buildVersion()}, mods...)
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
	guilds := "global"
	if n := len(cfg.Discord.GuildIDs); n > 0 {
		guilds = fmt.Sprintf("%d guild(s)", n)
	}
	fmt.Fprintf(stdout, "config ok: application %s, commands %s, gateway %t, hub %s (public %s, client %s), token from %s, hub secret from %s, listen %s, internal %s\n",
		cfg.Discord.ApplicationID, guilds, cfg.Discord.Gateway, cfg.Hub.URL, cfg.Hub.PublicURL, cfg.Hub.ClientID,
		cfg.Discord.TokenSource(), cfg.Hub.SecretSource(), cfg.Server.Listen, cfg.Server.InternalListen)
	return 0
}

func healthcheck(ctx context.Context, args []string, stderr io.Writer) int {
	fs := newFlagSet("healthcheck", stderr)
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

func buildVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				return "dev-" + s.Value[:7]
			}
		}
	}
	return strings.TrimSpace(version)
}
