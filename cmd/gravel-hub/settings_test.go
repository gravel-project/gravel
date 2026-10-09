package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gravel-project/gravel/internal/config"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

// ensureOrganization does what the hub does at start: creates the built-in organization.
func ensureOrganization(t *testing.T, ctx context.Context, cfgPath string) {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(ctx, cfg.Database.URL, 1, 10*time.Second, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, _, err := org.New(st, 15*time.Minute, logger).EnsureBuiltin(ctx, cfg.Organization.Name); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsExportAndApply(t *testing.T) {
	url := storetest.DatabaseURL(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "hub.yaml")
	if err := os.WriteFile(cfgPath, []byte("version: 1\norganization:\n  name: Test\ndatabase:\n  url: "+url+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := run(ctx, args, &out, &errOut)
		return code, out.String(), errOut.String()
	}
	if code, _, errOut := run("migrate", "--config", cfgPath); code != 0 {
		t.Fatalf("migrate: %d %s", code, errOut)
	}
	// The organization row exists once the hub has started; settings commands need it.
	if code, _, errOut := run("settings", "export", "--config", cfgPath); code != 1 || !strings.Contains(errOut, "not found") {
		t.Fatalf("export before the organization exists: %d %q", code, errOut)
	}
	ensureOrganization(t, ctx, cfgPath)

	code, out, _ := run("settings", "export", "--config", cfgPath)
	if code != 0 || !strings.Contains(out, "version: 1") {
		t.Fatalf("export defaults: %d %q", code, out)
	}
	manifest := filepath.Join(dir, "organization.yaml")
	if err := os.WriteFile(manifest, []byte("theme:\n  dark:\n    accent: \"#3ee07a\"\n  font: Archivo, system-ui, sans-serif\nnav:\n  - label: Discord\n    url: https://discord.gg/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := run("settings", "apply", manifest, "--dry-run", "--config", cfgPath); code != 3 || !strings.Contains(out, "would change") {
		t.Errorf("dry run with a change: %d %q", code, out)
	}
	if code, out, errOut := run("settings", "apply", "--config", cfgPath, manifest); code != 0 || !strings.Contains(out, "applied (1 nav links") || !strings.Contains(out, "discord unmapped") {
		t.Fatalf("apply: %d %q %q", code, out, errOut)
	}
	if code, out, _ := run("settings", "apply", manifest, "--dry-run", "--config", cfgPath); code != 0 || !strings.Contains(out, "unchanged") {
		t.Errorf("dry run after apply: %d %q", code, out)
	}
	code, out, _ = run("settings", "export", "--config", cfgPath)
	if code != 0 || !strings.Contains(out, "accent: '#3ee07a'") && !strings.Contains(out, `accent: "#3ee07a"`) || !strings.Contains(out, "label: Discord") {
		t.Errorf("export after apply: %d\n%s", code, out)
	}
	mapped := "discord:\n  guild_id: \"519496143298756611\"\n  roles:\n    linked: \"1300000000000000001\"\n  recognition:\n    - role: \"1300000000000000003\"\n      rule: first_members\n      count: 50\n"
	if err := os.WriteFile(manifest, []byte(mapped), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := run("settings", "apply", manifest, "--config", cfgPath); code != 0 || !strings.Contains(out, "discord guild 519496143298756611, 2 roles") {
		t.Errorf("apply a mapping: %d %q %q", code, out, errOut)
	}
	if code, out, _ := run("settings", "export", "--config", cfgPath); code != 0 || !strings.Contains(out, "guild_id: \"519496143298756611\"") && !strings.Contains(out, "guild_id: '519496143298756611'") {
		t.Errorf("export keeps the ids quoted strings: %d\n%s", code, out)
	}
	if err := os.WriteFile(manifest, []byte("theme:\n  font: \"x; y\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := run("settings", "apply", manifest, "--config", cfgPath); code != 1 || !strings.Contains(errOut, "theme.font") {
		t.Errorf("invalid manifest: %d %q", code, errOut)
	}
	if code, _, errOut := run("settings", "apply", "--config", cfgPath); code != 2 || !strings.Contains(errOut, "usage") {
		t.Errorf("no manifest: %d %q", code, errOut)
	}
	if code, _, _ := run("settings", "frobnicate"); code != 2 {
		t.Errorf("unknown subcommand: %d", code)
	}
}
