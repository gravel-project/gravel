package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"

	"github.com/gravel-project/gravel/internal/config"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/store"
)

// settings is `gravel-hub settings export|apply`: the Organization settings as a YAML manifest,
// read from and written to the hub's database by the hub itself (the same domain rules the API
// enforces; ADR-0007). `apply` is idempotent and, with --dry-run, exits 3 when it would change
// something, so a converge can check before it writes.
func settings(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: gravel-hub settings export|apply <manifest.yaml> [--dry-run] [--config hub.yaml]")
		return 2
	}
	switch args[0] {
	case "export":
		return settingsExport(ctx, args[1:], stdout, stderr)
	case "apply":
		return settingsApply(ctx, args[1:], stdout, stderr)
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
	set, _, err := svc.Settings(ctx)
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
	// The manifest may come before or after the flags; --config's value is not the manifest.
	var rest []string
	manifest := ""
	configValueNext := false
	for _, a := range args {
		switch {
		case configValueNext:
			rest = append(rest, a)
			configValueNext = false
		case a == "--config" || a == "-config":
			rest = append(rest, a)
			configValueNext = true
		case manifest == "" && !isFlag(a):
			manifest = a
		default:
			rest = append(rest, a)
		}
	}
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if manifest == "" {
		fmt.Fprintln(stderr, "usage: gravel-hub settings apply <manifest.yaml> [--dry-run] [--config hub.yaml]")
		return 2
	}
	b, err := os.ReadFile(manifest)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	want, err := org.ParseManifest(b)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := want.Validate(); err != nil {
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
	current, _, err := svc.Settings(ctx)
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
		fmt.Fprintf(stdout, "settings would change (%d nav links, theme %s, discord %s)\n", len(want.Nav), describeTheme(want.Theme), describeDiscord(want.Discord))
		return 3
	}
	if _, err := svc.UpdateSettings(ctx, want); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "settings applied (%d nav links, theme %s, discord %s)\n", len(want.Nav), describeTheme(want.Theme), describeDiscord(want.Discord))
	return 0
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
	return fmt.Sprintf("guild %s, %d roles", d.GuildID, len(d.RoleIDs()))
}
