package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravel-project/gravel/discord/bot"
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

func TestModules(t *testing.T) {
	names := func(cfg bot.Config) []string {
		t.Helper()
		mods, err := modules(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range mods {
			out = append(out, m.Name())
		}
		return out
	}
	cfg := bot.Default()
	cfg.Hub.URL, cfg.Hub.ClientID, cfg.Hub.ClientSecret = "http://hub:8080", "gravel_abc", "sec"
	if got := strings.Join(names(cfg), ","); got != "core,rolesync,linkedroles" {
		t.Errorf("stock modules: %s", got)
	}
	cfg.RoleSync.Enabled, cfg.LinkedRoles.Enabled = false, false
	if got := strings.Join(names(cfg), ","); got != "core" {
		t.Errorf("with both off: %s", got)
	}
	cfg.Hub.URL = "not a url"
	if _, err := modules(cfg); err == nil {
		t.Error("a bad hub url fails")
	}
}

func TestConfigCheck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bot.yaml")
	if err := os.WriteFile(path, []byte(botYAML+"role_sync:\n  dry_run: true\nlinked_roles:\n  enabled: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"config", "check", "--config", path}, &out, &errOut); code != 0 {
		t.Fatalf("config check: %d %s", code, errOut.String())
	}
	for _, want := range []string{"config ok", "role sync every 10m0s, poll 30s, dry run", "linked roles false", "token from discord.token"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("config check should say %q: %s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "tok,") || strings.Contains(out.String(), "sec,") {
		t.Errorf("config check prints no secret: %s", out.String())
	}
	if err := os.WriteFile(path, []byte(botYAML+"role_sync:\n  interval: 10s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), []string{"config", "check", "--config", path}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "role_sync.interval") {
		t.Errorf("a bad interval: %d %s", code, errOut.String())
	}
	if code := run(context.Background(), []string{"frobnicate"}, &out, &errOut); code != 2 {
		t.Errorf("unknown command: %d", code)
	}
	out.Reset()
	if code := run(context.Background(), []string{"version"}, &out, &errOut); code != 0 || !strings.HasPrefix(out.String(), "gravel-bot ") {
		t.Errorf("version: %d %q", code, out.String())
	}
}
