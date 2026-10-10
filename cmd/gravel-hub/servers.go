package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gravel-project/gravel/internal/config"
	"github.com/gravel-project/gravel/internal/hub"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/store"
)

// serversCmd is `gravel-hub servers export|apply|check`: the games and servers the hub controls
// as a servers.yaml manifest, read from and written to the hub's database by the hub itself
// (ADR-0010, the settings commands' siblings). A running hub picks up an apply within
// servers' reconcile interval. `apply` checks that every credential file is readable where it
// runs (inside the hub's container, where the secrets are); `check` needs no configuration, no
// database and no secret, so a deployment's CI can gate on it.
func serversCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: gravel-hub servers export|apply|check <servers.yaml> [--dry-run] [--config hub.yaml]")
		return 2
	}
	switch args[0] {
	case "export":
		return serversExport(ctx, args[1:], stdout, stderr)
	case "apply":
		return serversApply(ctx, args[1:], stdout, stderr)
	case "check":
		return serversCheck(args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "gravel-hub servers: unknown command %q\n", args[0])
	return 2
}

// openServers opens the store and the built-in organization's servers. create makes the
// organization when a migrated database was never served (as settings apply does); without it a
// missing organization is store.ErrNotFound.
func openServers(ctx context.Context, cfg config.Config, create bool) (*servers.Service, func(), error) {
	logger := cfg.Log.NewLogger(io.Discard)
	st, err := store.Open(ctx, cfg.Database.URL, 1, cfg.Database.ConnectTimeout, logger)
	if err != nil {
		return nil, nil, err
	}
	o := org.New(st, cfg.Claim.TokenTTL, logger)
	var organization store.Organization
	if create {
		organization, err = o.CreateBuiltinIfAbsent(ctx, cfg.Organization.Name)
	} else {
		organization, err = o.Get(ctx)
	}
	if err != nil {
		st.Close()
		return nil, nil, err
	}
	return servers.New(st, organization.ID, hub.Drivers().Names(), logger), st.Close, nil
}

func serversExport(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("servers export", stderr)
	path := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, ok := load(*path, stderr)
	if !ok {
		return 1
	}
	m := servers.Manifest{}.Normalized()
	svc, closeStore, err := openServers(ctx, cfg, false)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// never served: nothing applied yet
	case err != nil:
		fmt.Fprintln(stderr, err)
		return 1
	default:
		defer closeStore()
		if m, err = svc.Manifest(ctx); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	out, err := m.Marshal()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	_, _ = stdout.Write(out)
	return 0
}

func serversApply(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("servers apply", stderr)
	path := configFlag(fs)
	dryRun := fs.Bool("dry-run", false, "validate and say what would change; change nothing (exit 3 when something would)")
	manifest, rest := manifestArg(args)
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if manifest == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: gravel-hub servers apply <servers.yaml> [--dry-run] [--config hub.yaml]")
		return 2
	}
	want, err := readServersManifest(manifest)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	cfg, ok := load(*path, stderr)
	if !ok {
		return 1
	}
	svc, closeStore, err := openServers(ctx, cfg, !*dryRun)
	if *dryRun && errors.Is(err, store.ErrNotFound) {
		// Never served, so nothing is stored: the whole manifest would be added.
		if len(want.Games)+len(want.Servers) == 0 {
			fmt.Fprintf(stdout, "servers unchanged (%s)\n", summarizeServers(want))
			return 0
		}
		fmt.Fprintf(stdout, "servers would change (%s)\n", summarizeServers(want))
		return 3
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer closeStore()
	changes, err := svc.Apply(ctx, want, servers.ApplyOptions{DryRun: *dryRun, CheckCredentials: true})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if len(changes) == 0 {
		fmt.Fprintf(stdout, "servers unchanged (%s)\n", summarizeServers(want))
		return 0
	}
	verb := "applied"
	if *dryRun {
		verb = "would change"
	}
	fmt.Fprintf(stdout, "servers %s (%s)\n", verb, summarizeServers(want))
	for _, c := range changes {
		fmt.Fprintf(stdout, "  %s\n", c)
	}
	if *dryRun {
		return 3
	}
	return 0
}

// serversCheck is `gravel-hub servers check <servers.yaml>`: the parse and validation apply runs
// before it opens the store, and nothing else (no credential file is read). Exit 0 valid, 1
// invalid (every problem named), 2 usage.
func serversCheck(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("servers check", stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: gravel-hub servers check <servers.yaml>")
		return 2
	}
	m, err := readServersManifest(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "servers valid (%s)\n", summarizeServers(m))
	return 0
}

// readServersManifest reads, strictly parses and validates a manifest: what apply and check
// share.
func readServersManifest(path string) (servers.Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return servers.Manifest{}, err
	}
	m, err := servers.ParseManifest(b)
	if err != nil {
		return servers.Manifest{}, err
	}
	if err := m.Validate(servers.Specs(), hub.Drivers().Names()); err != nil {
		return servers.Manifest{}, err
	}
	return m, nil
}

func summarizeServers(m servers.Manifest) string {
	games := make([]string, 0, len(m.Games))
	for _, g := range m.Games {
		games = append(games, g.ID)
	}
	ids := make([]string, 0, len(m.Servers))
	for _, s := range m.Servers {
		ids = append(ids, s.ID)
	}
	return fmt.Sprintf("%d games [%s], %d servers [%s]", len(games), strings.Join(games, " "), len(ids), strings.Join(ids, " "))
}

// manifestArg takes the manifest path out of the arguments: it may come before or after the
// flags, and --config's value is not it.
func manifestArg(args []string) (string, []string) {
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
	return manifest, rest
}
