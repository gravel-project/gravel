package stock_test

import (
	"strings"
	"testing"

	"github.com/gravel-project/gravel/discord/bot"
	"github.com/gravel-project/gravel/discord/bot/stock"
	"github.com/gravel-project/gravel/discord/hubclient"
)

func TestModules(t *testing.T) {
	hub, err := hubclient.New(hubclient.Config{URL: "http://hub:8080", ClientID: "gravel_abc", ClientSecret: "sec"})
	if err != nil {
		t.Fatal(err)
	}
	names := func(cfg bot.Config) string {
		var out []string
		for _, m := range stock.Modules(cfg, hub) {
			out = append(out, m.Name())
		}
		return strings.Join(out, ",")
	}
	cfg := bot.Default()
	if got := names(cfg); got != "core,rolesync,linkedroles" {
		t.Errorf("defaults: %s", got)
	}
	cfg.RoleSync.Enabled = false
	if got := names(cfg); got != "core,linkedroles" {
		t.Errorf("role sync off: %s", got)
	}
	cfg.RoleSync.Enabled, cfg.LinkedRoles.Enabled = true, false
	if got := names(cfg); got != "core,rolesync" {
		t.Errorf("linked roles off: %s", got)
	}
	cfg.RoleSync.Enabled = false
	if got := names(cfg); got != "core" {
		t.Errorf("both off: %s", got)
	}
}
