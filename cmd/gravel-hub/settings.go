package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/gravel-project/gravel/internal/config"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/store"
)

// settings is `gravel-hub settings export|apply|check`: the Organization settings as a YAML
// manifest, read from and written to the hub's database by the hub itself (the same domain rules
// the API enforces; ADR-0007). `apply` is idempotent and, with --dry-run, exits 3 when it would
// change something, so a converge can check before it writes. `check` validates a manifest with
// no configuration and no database, so a deployment's CI can gate on it.
func settings(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: gravel-hub settings export|apply|check <manifest.yaml> [--dry-run] [--config hub.yaml]")
		return 2
	}
	switch args[0] {
	case "export":
		return settingsExport(ctx, args[1:], stdout, stderr)
	case "apply":
		return settingsApply(ctx, args[1:], stdout, stderr)
	case "check":
		return settingsCheck(args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "gravel-hub settings: unknown command %q\n", args[0])
	return 2
}

func openOrg(ctx context.Context, cfg config.Config, stderr io.Writer) (*org.Service, func(), bool) {
	logger := cfg.Log.NewLogger(io.Discard)
	st, err := store.Open(ctx, cfg.Database.URL, 1, cfg.Database.ConnectTimeout, logger)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return nil, nil, false
	}
	return org.New(st, cfg.Claim.TokenTTL, logger), st.Close, true
}

func settingsExport(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("settings export", stderr)
	path := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, ok := load(*path, stderr)
	if !ok {
		return 1
	}
	svc, closeStore, ok := openOrg(ctx, cfg, stderr)
	if !ok {
		return 1
	}
	defer closeStore()
	set, err := storedSettings(ctx, svc, cfg.Organization.Name, false)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	out, err := set.Manifest()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	_, _ = stdout.Write(out)
	return 0
}

func settingsApply(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("settings apply", stderr)
	path := configFlag(fs)
	dryRun := fs.Bool("dry-run", false, "validate and say whether the manifest differs from the stored settings; change nothing (exit 3 when it would)")
	manifest, rest := manifestArg(args)
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if manifest == "" {
		fmt.Fprintln(stderr, "usage: gravel-hub settings apply <manifest.yaml> [--dry-run] [--config hub.yaml]")
		return 2
	}
	want, err := readManifest(manifest)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	cfg, ok := load(*path, stderr)
	if !ok {
		return 1
	}
	svc, closeStore, ok := openOrg(ctx, cfg, stderr)
	if !ok {
		return 1
	}
	defer closeStore()
	current, err := storedSettings(ctx, svc, cfg.Organization.Name, *dryRun)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	wantOut, _ := want.Manifest()
	currentOut, _ := current.Manifest()
	if bytes.Equal(wantOut, currentOut) {
		fmt.Fprintln(stdout, "settings unchanged")
		return 0
	}
	if *dryRun {
		fmt.Fprintf(stdout, "settings would change (%s)\n", summarize(want))
		return 3
	}
	if _, err := svc.UpdateSettings(ctx, want); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "settings applied (%s)\n", summarize(want))
	return 0
}

// storedSettings is the built-in organization's settings, what export prints and apply compares
// the manifest with. A migrated database that was never served has no organization yet: it is
// created with the configured name (never touching the owner claim, so the token serve printed
// stays valid), except on a dry run, which changes nothing and compares with the defaults.
func storedSettings(ctx context.Context, svc *org.Service, name string, dryRun bool) (org.Settings, error) {
	if !dryRun {
		if _, err := svc.CreateBuiltinIfAbsent(ctx, name); err != nil {
			return org.Settings{}, err
		}
	}
	set, _, err := svc.Settings(ctx)
	if dryRun && errors.Is(err, store.ErrNotFound) {
		return org.DefaultSettings(), nil
	}
	return set, err
}

// settingsCheck is `gravel-hub settings check <manifest.yaml>`: the parse and validation apply
// runs before it opens the store, and nothing else. Exit 0 valid, 1 invalid (every problem
// named), 2 usage.
func settingsCheck(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("settings check", stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: gravel-hub settings check <manifest.yaml>")
		return 2
	}
	set, err := readManifest(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "settings valid (%s)\n", summarize(set))
	return 0
}

// readManifest reads, strictly parses and validates a manifest: what apply and check share.
func readManifest(path string) (org.Settings, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return org.Settings{}, err
	}
	set, err := org.ParseManifest(b)
	if err != nil {
		return org.Settings{}, err
	}
	if err := set.Validate(); err != nil {
		return org.Settings{}, err
	}
	return set, nil
}

func summarize(s org.Settings) string {
	return fmt.Sprintf("%d nav links, theme %s, discord %s", len(s.Nav), describeTheme(s.Theme), describeDiscord(s.Discord))
}

func describeTheme(t org.Theme) string {
	if t == (org.Theme{}) {
		return "default"
	}
	parts := 0
	for _, v := range []string{t.Light.Accent, t.Light.Background, t.Light.Foreground, t.Light.Muted, t.Light.Line, t.Light.OK, t.Light.Err,
		t.Dark.Accent, t.Dark.Background, t.Dark.Foreground, t.Dark.Muted, t.Dark.Line, t.Dark.OK, t.Dark.Err} {
		if v != "" {
			parts++
		}
	}
	extra := ""
	if t.LogoURL != "" {
		extra += ", logo"
	}
	if t.Font != "" {
		extra += ", font"
	}
	return fmt.Sprintf("%d tokens%s", parts, extra)
}

func describeDiscord(d org.Discord) string {
	if d.IsZero() {
		return "unmapped"
	}
	out := fmt.Sprintf("guild %s, %d roles", d.GuildID, len(d.RoleIDs()))
	if n := len(d.ServerCards); n > 0 {
		out += fmt.Sprintf(", %d server cards", n)
	}
	return out
}
