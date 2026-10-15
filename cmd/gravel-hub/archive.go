package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/gravel-project/gravel/internal/archive"
	"github.com/gravel-project/gravel/internal/hub"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/store"
)

// exportCmd is `gravel-hub export [-o FILE] [--config hub.yaml]` (gravel#213): everything the hub
// holds, as an archive (internal/archive), to FILE (written 0600, refused when it exists) or
// stdout. It reads from one snapshot while the hub runs. Exit 0 written, 1 failed, 2 usage.
func exportCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("export", stderr)
	path := configFlag(fs)
	out := fs.String("o", "", "the archive's file (written 0600; refused when it exists); stdout when unset")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: gravel-hub export [-o hub.tar.gz] [--config hub.yaml]")
		return 2
	}
	cfg, ok := load(*path, stderr)
	if !ok {
		return 1
	}
	logger := cfg.Log.NewLogger(io.Discard)
	st, err := store.Open(ctx, cfg.Database.URL, 2, cfg.Database.ConnectTimeout, logger)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer st.Close()
	o := org.New(st, cfg.Claim.TokenTTL, logger).WithGames(servers.GameIDs())
	got, err := o.Get(ctx)
	if errors.Is(err, store.ErrNotFound) {
		fmt.Fprintln(stderr, "gravel-hub export: this database holds no hub yet (it was never served)")
		return 1
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	sv := servers.New(st, got.ID, hub.DriverNames(), logger).WithProviders(hub.CloudProviders()...)
	files, name, err := archive.Readable(ctx, o, sv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	w := stdout
	if *out != "" {
		f, err := os.OpenFile(*out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Fprintln(stderr, "gravel-hub export:", err)
			return 1
		}
		w = f
		defer func() { _ = f.Close() }()
	}
	m, err := archive.Write(ctx, st, w, buildVersion(), name, files, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, err)
		if *out != "" {
			_ = os.Remove(*out)
		}
		return 1
	}
	if *out != "" {
		var rows int64
		for _, t := range m.Tables {
			rows += t.Rows
		}
		fmt.Fprintf(stderr, "exported %s: %d tables, %d rows, schema %d, to %s (it holds personal data and password hashes: keep it as you keep your backups)\n",
			name, len(m.Tables), rows, m.Schema, *out)
	}
	return 0
}

// importCmd is `gravel-hub import <archive> [--config hub.yaml]` (gravel#213): load an export into
// the database hub.yaml names, which must be empty (no hub has served it: start the new hub after
// the import, not before). The database is migrated to the archive's schema, loaded in one
// transaction, then migrated to this gravel's. Exit 0 loaded, 1 refused or failed, 2 usage.
func importCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("import", stderr)
	path := configFlag(fs)
	file, rest := manifestArg(args)
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if file == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: gravel-hub import <hub.tar.gz> [--config hub.yaml]")
		return 2
	}
	cfg, ok := load(*path, stderr)
	if !ok {
		return 1
	}
	f, err := os.Open(file) //nolint:gosec // the operator's own archive
	if err != nil {
		fmt.Fprintln(stderr, "gravel-hub import:", err)
		return 1
	}
	defer func() { _ = f.Close() }()
	st, err := store.Open(ctx, cfg.Database.URL, 2, cfg.Database.ConnectTimeout, cfg.Log.NewLogger(io.Discard))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer st.Close()
	migrations, err := store.Migrations()
	if err != nil || len(migrations) == 0 {
		fmt.Fprintln(stderr, "gravel-hub import: this gravel's migrations:", err)
		return 1
	}
	m, err := archive.Load(ctx, st, f, migrations[len(migrations)-1].Version)
	if err != nil {
		fmt.Fprintln(stderr, "gravel-hub import:", err)
		return 1
	}
	var rows int64
	for _, t := range m.Tables {
		rows += t.Rows
	}
	fmt.Fprintf(stdout, "imported %s: %d tables, %d rows, exported by gravel %s at %s (schema %d). Start the hub: its members sign in again.\n",
		m.Organization, len(m.Tables), rows, m.Gravel, m.ExportedAt.Format(time.RFC3339), m.Schema)
	return 0
}
