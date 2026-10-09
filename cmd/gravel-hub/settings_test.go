package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
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

// ensureOrganization does what the hub does at start: creates the built-in organization and, while
// it is unowned, mints the owner-claim token it returns.
func ensureOrganization(t *testing.T, ctx context.Context, cfgPath string) string {
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
	_, token, err := org.New(st, 15*time.Minute, logger).EnsureBuiltin(ctx, cfg.Organization.Name)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// freshHub gives a test a migrated database that has never been served (no organization row) and
// a hub.yaml pointing at it; it returns the store, the config path, a temporary directory and a
// runner for the command line.
func freshHub(t *testing.T) (*store.Store, string, string, func(args ...string) (int, string, string)) {
	t.Helper()
	url := storetest.DatabaseURL(t)
	st := storetest.Open(t)
	storetest.Reset(t, st)
	t.Cleanup(func() { storetest.Reset(t, st) })
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "hub.yaml")
	if err := os.WriteFile(cfgPath, []byte("version: 1\norganization:\n  name: Test\ndatabase:\n  url: "+url+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := run(context.Background(), args, &out, &errOut)
		return code, out.String(), errOut.String()
	}
	if code, _, errOut := run("migrate", "--config", cfgPath); code != 0 {
		t.Fatalf("migrate: %d %s", code, errOut)
	}
	if _, err := st.GetBuiltinOrganization(context.Background()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a never-served database has no organization: %v", err)
	}
	return st, cfgPath, dir, run
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsCheck(t *testing.T) {
	// No configuration and no database: a config path that does not exist proves neither is read.
	t.Setenv("GRAVEL_CONFIG", filepath.Join(t.TempDir(), "absent.yaml"))
	dir := t.TempDir()
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := run(context.Background(), args, &out, &errOut)
		return code, out.String(), errOut.String()
	}
	manifest := filepath.Join(dir, "organization.yaml")

	writeFile(t, manifest, "theme:\n  dark:\n    accent: \"#3ee07a\"\n  font: Archivo, system-ui, sans-serif\nnav:\n  - label: Discord\n    url: https://discord.gg/x\n")
	if code, out, errOut := run("settings", "check", manifest); code != 0 || out != "settings valid (1 nav links, theme 1 tokens, font, discord unmapped)\n" || errOut != "" {
		t.Errorf("valid manifest: %d %q %q", code, out, errOut)
	}

	writeFile(t, manifest, "theme:\n  dark:\n    acent: \"#3ee07a\"\n")
	if code, out, errOut := run("settings", "check", manifest); code != 1 || out != "" || !strings.Contains(errOut, "field acent not found") {
		t.Errorf("a typo'd key is named: %d %q %q", code, out, errOut)
	}

	writeFile(t, manifest, "theme:\n  light:\n    background: \"url(https://x)\"\n")
	if code, out, errOut := run("settings", "check", manifest); code != 1 || out != "" || !strings.Contains(errOut, "theme.light.background") {
		t.Errorf("a bad colour is named: %d %q %q", code, out, errOut)
	}

	if code, _, errOut := run("settings", "check", filepath.Join(dir, "missing.yaml")); code != 1 || !strings.Contains(errOut, "missing.yaml") {
		t.Errorf("an unreadable manifest: %d %q", code, errOut)
	}
	for _, args := range [][]string{
		{"settings", "check"},
		{"settings", "check", manifest, manifest},
		{"settings", "check", "--config", "hub.yaml", manifest},
	} {
		if code, _, errOut := run(args...); code != 2 || errOut == "" {
			t.Errorf("%v: usage error: %d %q", args, code, errOut)
		}
	}
}

func TestSettingsApplyOnANeverServedDatabase(t *testing.T) {
	st, cfgPath, dir, run := freshHub(t)
	ctx := context.Background()
	manifest := filepath.Join(dir, "organization.yaml")
	writeFile(t, manifest, "nav:\n  - label: Discord\n    url: https://discord.gg/x\n")

	// A dry run changes nothing, not even the organization row.
	if code, out, errOut := run("settings", "apply", manifest, "--dry-run", "--config", cfgPath); code != 3 || !strings.Contains(out, "would change (1 nav links") {
		t.Errorf("dry run: %d %q %q", code, out, errOut)
	}
	if _, err := st.GetBuiltinOrganization(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a dry run must not create the organization: %v", err)
	}

	if code, out, errOut := run("settings", "apply", manifest, "--config", cfgPath); code != 0 || !strings.Contains(out, "applied (1 nav links") {
		t.Fatalf("apply: %d %q %q", code, out, errOut)
	}
	o, err := st.GetBuiltinOrganization(ctx)
	if err != nil || o.Name != "Test" || o.Owned() || o.ClaimTokenHash != nil || o.ClaimTokenExpiresAt != nil {
		t.Errorf("apply creates the organization with the configured name and no claim token: %v %+v", err, o)
	}
	if code, out, _ := run("settings", "export", "--config", cfgPath); code != 0 || !strings.Contains(out, "label: Discord") {
		t.Errorf("export after apply: %d\n%s", code, out)
	}
	// serve, started later, still mints the token that claims the hub.
	if token := ensureOrganization(t, ctx, cfgPath); token == "" {
		t.Error("serve after apply must mint a claim token")
	}
}

func TestSettingsExportOnANeverServedDatabase(t *testing.T) {
	st, cfgPath, _, run := freshHub(t)
	if code, out, errOut := run("settings", "export", "--config", cfgPath); code != 0 || !strings.Contains(out, "version: 1") {
		t.Fatalf("export defaults: %d %q %q", code, out, errOut)
	}
	if o, err := st.GetBuiltinOrganization(context.Background()); err != nil || o.Name != "Test" || o.ClaimTokenHash != nil {
		t.Errorf("export creates the organization and nothing else: %v %+v", err, o)
	}
}

func TestSettingsCommandsKeepTheClaimToken(t *testing.T) {
	st, cfgPath, dir, run := freshHub(t)
	ctx := context.Background()
	token := ensureOrganization(t, ctx, cfgPath) // the token serve printed
	before, err := st.GetBuiltinOrganization(ctx)
	if err != nil || token == "" || before.ClaimTokenHash == nil || before.Owned() {
		t.Fatalf("an unowned hub with a token: %v %q %+v", err, token, before)
	}
	manifest := filepath.Join(dir, "organization.yaml")
	writeFile(t, manifest, "theme:\n  dark:\n    accent: \"#3ee07a\"\n")
	if code, out, errOut := run("settings", "apply", manifest, "--config", cfgPath); code != 0 || !strings.Contains(out, "applied") {
		t.Fatalf("apply: %d %q %q", code, out, errOut)
	}
	if code, _, errOut := run("settings", "export", "--config", cfgPath); code != 0 {
		t.Fatalf("export: %d %q", code, errOut)
	}
	after, err := st.GetBuiltinOrganization(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after.ClaimTokenHash, before.ClaimTokenHash) || !after.ClaimTokenExpiresAt.Equal(*before.ClaimTokenExpiresAt) {
		t.Errorf("settings apply/export rotated the claim token: before %x %v, after %x %v",
			before.ClaimTokenHash, before.ClaimTokenExpiresAt, after.ClaimTokenHash, after.ClaimTokenExpiresAt)
	}
	if !bytes.Equal(after.ClaimTokenHash, sha256Sum(token)) {
		t.Error("the stored hash is no longer the hash of the token serve printed")
	}
}

func sha256Sum(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

func TestSettingsExportAndApply(t *testing.T) {
	_, cfgPath, dir, run := freshHub(t)
	ensureOrganization(t, context.Background(), cfgPath)

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
