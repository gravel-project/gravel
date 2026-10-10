package cli_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/handler"

	"github.com/gravel-project/gravel/discord/bot"
	"github.com/gravel-project/gravel/discord/bot/cli"
	"github.com/gravel-project/gravel/discord/hubclient"
)

const botYAML = `version: 1
discord:
  application_id: "1558123590786748516"
  public_key: "f61a8fd49fee0b5bf031dad66740c7225d06a4c4b60b412b875f2482a91ca808"
  token: tok
hub:
  url: http://hub:8080
  client_id: gravel_abc
  client_secret: sec
`

type hostModule struct{}

func (hostModule) Name() string { return "rules" }
func (hostModule) Register(r *bot.Registry) error {
	return r.SlashCommand(discord.SlashCommandCreate{Name: "rules", Description: "the rules"}, func(*handler.CommandEvent) error { return nil })
}

func host() cli.Program {
	return cli.Program{
		Name: "host-bot", Version: "1.2.3", DefaultConfig: "/etc/host/bot.yaml", ConfigEnv: "HOST_BOT_CONFIG", About: "The host's own bot.",
		Modules: func(bot.Config, *hubclient.Client) []bot.Module { return []bot.Module{hostModule{}} },
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bot.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func run(p cli.Program, args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := p.Run(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestBuildModulesAppendsTheHostsAfterTheStockSet(t *testing.T) {
	cfg := bot.Default()
	cfg.Hub.URL, cfg.Hub.ClientID, cfg.Hub.ClientSecret = "http://hub:8080", "gravel_abc", "sec"
	names := func(p cli.Program) string {
		mods, err := p.BuildModules(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range mods {
			out = append(out, m.Name())
		}
		return strings.Join(out, ",")
	}
	if got := names(host()); got != "core,rolesync,linkedroles,servercards,rules" {
		t.Errorf("host: %s", got)
	}
	if got := names(cli.Program{Name: "gravel-bot"}); got != "core,rolesync,linkedroles,servercards" {
		t.Errorf("no host modules: %s", got)
	}
	cfg.Hub.URL = "not a url"
	if _, err := host().BuildModules(cfg); err == nil {
		t.Error("a bad hub url fails")
	}
}

func TestConfigCheck(t *testing.T) {
	path := writeConfig(t, botYAML+"role_sync:\n  dry_run: true\nlinked_roles:\n  enabled: false\n")
	code, out, errOut := run(host(), "config", "check", "--config", path)
	if code != 0 {
		t.Fatalf("config check: %d %s", code, errOut)
	}
	for _, want := range []string{"config ok", "role sync every 10m0s, poll 30s, dry run", "linked roles false", "server cards every 15s", "token from discord.token", "modules core, rolesync, servercards, rules"} {
		if !strings.Contains(out, want) {
			t.Errorf("config check should say %q: %s", want, out)
		}
	}
	if strings.Contains(out, "tok,") || strings.Contains(out, "sec,") {
		t.Errorf("config check prints no secret: %s", out)
	}

	path = writeConfig(t, botYAML+"role_sync:\n  interval: 10s\n")
	if code, _, errOut := run(host(), "config", "check", "--config", path); code != 1 || !strings.Contains(errOut, "role_sync.interval") {
		t.Errorf("a bad interval: %d %s", code, errOut)
	}
	if code, _, errOut := run(host(), "config", "nope"); code != 2 || !strings.Contains(errOut, "usage: host-bot config check") {
		t.Errorf("config without check: %d %s", code, errOut)
	}
}

// The config path comes from --config, then the program's variable, then its default.
func TestConfigPath(t *testing.T) {
	good := writeConfig(t, botYAML)
	t.Setenv("HOST_BOT_CONFIG", good)
	if code, out, errOut := run(host(), "config", "check"); code != 0 || !strings.Contains(out, "config ok") {
		t.Errorf("from the variable: %d %s %s", code, out, errOut)
	}
	if code, _, errOut := run(host(), "config", "check", "--config", filepath.Join(t.TempDir(), "missing.yaml")); code != 1 || !strings.Contains(errOut, "missing.yaml") {
		t.Errorf("the flag wins: %d %s", code, errOut)
	}
	t.Setenv("HOST_BOT_CONFIG", "")
	if code, _, errOut := run(host(), "config", "check"); code != 1 || !strings.Contains(errOut, "/etc/host/bot.yaml") {
		t.Errorf("the default: %d %s", code, errOut)
	}
}

func TestCommands(t *testing.T) {
	if code, out, _ := run(host(), "version"); code != 0 || out != "host-bot 1.2.3\n" {
		t.Errorf("version: %d %q", code, out)
	}
	if v := (cli.Program{Version: "dev"}).BuildVersion(); v != "dev" && !strings.HasPrefix(v, "dev-") {
		t.Errorf("dev version: %q", v)
	}
	code, out, _ := run(host(), "help")
	if code != 0 || !strings.Contains(out, "usage: host-bot") || !strings.Contains(out, "The host's own bot.") || !strings.Contains(out, "$HOST_BOT_CONFIG, then /etc/host/bot.yaml") {
		t.Errorf("help: %d %s", code, out)
	}
	if code, _, errOut := run(host(), "frobnicate"); code != 2 || !strings.Contains(errOut, `host-bot: unknown command "frobnicate"`) {
		t.Errorf("unknown command: %d %s", code, errOut)
	}
	if code, _, _ := run(host(), "serve", "--nope"); code != 2 {
		t.Errorf("a bad flag: %d", code)
	}
}

func TestHealthcheck(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"status":"ok"}`)) }))
	t.Cleanup(healthy.Close)
	sick := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", http.StatusServiceUnavailable) }))
	t.Cleanup(sick.Close)
	if code, _, errOut := run(host(), "healthcheck", "--url", healthy.URL); code != 0 {
		t.Errorf("healthy: %d %s", code, errOut)
	}
	if code, _, errOut := run(host(), "healthcheck", "--url", sick.URL); code != 1 || !strings.Contains(errOut, "503") {
		t.Errorf("sick: %d %s", code, errOut)
	}
	if code, _, _ := run(host(), "healthcheck", "--url", "http://127.0.0.1:1/healthz", "--timeout", "200ms"); code != 1 {
		t.Errorf("down: %d", code)
	}
}
