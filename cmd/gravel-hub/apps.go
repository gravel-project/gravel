package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/gravel-project/gravel/internal/apps"
	"github.com/gravel-project/gravel/internal/config"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/store"
)

// appsCmd is `gravel-hub apps create|list|revoke`: first-party app registrations (ADR-0008)
// managed from the hub's own binary with no API credential, the way `settings apply` bootstraps
// settings. `create` prints the secret once; the hub keeps only its hash.
func appsCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: gravel-hub apps create --name NAME --scopes SCOPES | list | revoke --client-id ID  [--config hub.yaml]")
		return 2
	}
	switch args[0] {
	case "create":
		return appsCreate(ctx, args[1:], stdout, stderr)
	case "list":
		return appsList(ctx, args[1:], stdout, stderr)
	case "revoke":
		return appsRevoke(ctx, args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "gravel-hub apps: unknown command %q\n", args[0])
	return 2
}

func openApps(ctx context.Context, cfg config.Config, stderr io.Writer) (*apps.Service, func(), bool) {
	logger := cfg.Log.NewLogger(io.Discard)
	st, err := store.Open(ctx, cfg.Database.URL, 1, cfg.Database.ConnectTimeout, logger)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return nil, nil, false
	}
	o, err := org.New(st, cfg.Claim.TokenTTL, logger).Get(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err)
		st.Close()
		return nil, nil, false
	}
	return apps.New(st, o.ID, cfg.Apps.TokenTTL, logger), st.Close, true
}

func scopeNames() string {
	names := make([]string, 0, len(apps.Scopes))
	for name := range apps.Scopes {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func appsCreate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("apps create", stderr)
	path := configFlag(fs)
	name := fs.String("name", "", "the app's name, for the owner's eyes (required)")
	scopes := fs.String("scopes", "", "comma-separated scopes the app may use (required): "+scopeNames())
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*name) == "" || strings.TrimSpace(*scopes) == "" {
		fmt.Fprintln(stderr, "usage: gravel-hub apps create --name NAME --scopes "+scopeNames()+" [--config hub.yaml]")
		return 2
	}
	cfg, ok := load(*path, stderr)
	if !ok {
		return 1
	}
	svc, closeStore, ok := openApps(ctx, cfg, stderr)
	if !ok {
		return 1
	}
	defer closeStore()
	created, err := svc.Create(ctx, *name, strings.Split(*scopes, ","))
	if err != nil {
		fmt.Fprintln(stderr, err)
		if errors.Is(err, apps.ErrInvalidName) || errors.Is(err, apps.ErrInvalidScope) {
			return 2
		}
		return 1
	}
	fmt.Fprintf(stdout, "app registered: %s\nclient_id: %s\nclient_secret: %s\nscopes: %s\n\nThe secret is shown once; the hub keeps its hash. Give it to the app as a secret file.\n",
		created.App.Name, created.App.ClientID, created.Secret, strings.Join(created.App.Scopes, ","))
	return 0
}

func appsList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("apps list", stderr)
	path := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, ok := load(*path, stderr)
	if !ok {
		return 1
	}
	svc, closeStore, ok := openApps(ctx, cfg, stderr)
	if !ok {
		return 1
	}
	defer closeStore()
	list, err := svc.List(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if len(list) == 0 {
		fmt.Fprintln(stdout, "no apps registered")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CLIENT_ID\tNAME\tSCOPES\tCREATED\tREVOKED")
	for _, a := range list {
		revoked := "-"
		if a.RevokedAt != nil {
			revoked = a.RevokedAt.UTC().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", a.ClientID, a.Name, strings.Join(a.Scopes, ","), a.CreatedAt.UTC().Format("2006-01-02 15:04"), revoked)
	}
	_ = tw.Flush()
	return 0
}

func appsRevoke(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("apps revoke", stderr)
	path := configFlag(fs)
	clientID := fs.String("client-id", "", "the app's client id (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*clientID) == "" {
		fmt.Fprintln(stderr, "usage: gravel-hub apps revoke --client-id ID [--config hub.yaml]")
		return 2
	}
	cfg, ok := load(*path, stderr)
	if !ok {
		return 1
	}
	svc, closeStore, ok := openApps(ctx, cfg, stderr)
	if !ok {
		return 1
	}
	defer closeStore()
	if err := svc.Revoke(ctx, strings.TrimSpace(*clientID)); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			fmt.Fprintf(stderr, "no app with client id %s, or it is already revoked\n", *clientID)
			return 1
		}
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "app revoked: %s (its tokens are gone; no new one will be issued)\n", *clientID)
	return 0
}
