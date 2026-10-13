package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gravel-project/gravel/internal/config"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/store"
)

// usersCmd is `gravel-hub users create-local|reset-password`: local accounts (ADR-0026) made from
// the hub's own binary with no API credential, the way `apps create` bootstraps apps. It is how a
// hub with no provider gets its first member, who then claims it (gravel#187). Each prints a
// one-time link that sets the password; the hub keeps only its hash.
func usersCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	const usage = "usage: gravel-hub users create-local --username NAME [--email ADDRESS] | reset-password --username NAME  [--config hub.yaml]"
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	sub := args[0]
	if sub != "create-local" && sub != "reset-password" {
		fmt.Fprintf(stderr, "gravel-hub users: unknown command %q\n%s\n", sub, usage)
		return 2
	}
	fs := newFlagSet("users "+sub, stderr)
	path := configFlag(fs)
	username := fs.String("username", "", "the local account's username (required)")
	email := fs.String("email", "", "create-local: an optional address, only ever for recovery")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if strings.TrimSpace(*username) == "" || (sub == "reset-password" && *email != "") {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	cfg, ok := load(*path, stderr)
	if !ok {
		return 1
	}
	if !cfg.Auth.Local.Enabled {
		fmt.Fprintln(stderr, "gravel-hub users: local accounts are off; set auth.local.enabled: true in hub.yaml and restart the hub first")
		return 1
	}
	ids, closeStore, ok := openLocal(ctx, cfg, stderr)
	if !ok {
		return 1
	}
	defer closeStore()
	var link identity.PasswordLink
	var err error
	if sub == "create-local" {
		_, link, err = ids.CreateLocal(ctx, *username, *email, nil)
	} else {
		link, err = ids.IssueReset(ctx, *username, nil)
	}
	if err != nil {
		fmt.Fprintln(stderr, "gravel-hub users:", err)
		if errors.Is(err, identity.ErrInvalidUsername) || errors.Is(err, identity.ErrInvalidEmail) {
			return 2
		}
		return 1
	}
	if sub == "create-local" {
		fmt.Fprintf(stdout, "local account created: %s\n", link.Username)
	} else {
		fmt.Fprintf(stdout, "password reset for: %s (its earlier links no longer work)\n", link.Username)
	}
	url := strings.TrimSuffix(cfg.Auth.BaseURL, "/") + link.Path()
	if cfg.Auth.BaseURL == "" {
		url = link.Path() + "  (prefix the address members reach the hub at; auth.base_url is not set)"
	}
	fmt.Fprintf(stdout, "password link (shown once, works once, until %s):\n%s\n", link.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC"), url)
	return 0
}

// openLocal is the identity service with local accounts and password links over the hub's
// database, reading the host's policy from the Organization settings.
func openLocal(ctx context.Context, cfg config.Config, stderr io.Writer) (*identity.Service, func(), bool) {
	logger := cfg.Log.NewLogger(io.Discard)
	st, err := store.Open(ctx, cfg.Database.URL, 1, cfg.Database.ConnectTimeout, logger)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return nil, nil, false
	}
	orgSvc := org.New(st, cfg.Claim.TokenTTL, logger)
	o, err := orgSvc.Get(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err)
		st.Close()
		return nil, nil, false
	}
	ids := identity.New(st, o.ID, nil, cfg.Auth.AttemptTTL, logger,
		identity.WithLocalAccounts(st, func(ctx context.Context) (identity.LocalPolicy, error) {
			set, _, err := orgSvc.Settings(ctx)
			if err != nil {
				return identity.LocalPolicy{}, err
			}
			return identity.LocalPolicy{RegistrationOpen: set.LocalAccounts.Open(), MinPasswordLength: set.LocalAccounts.MinPasswordLength}, nil
		}),
		identity.WithPasswordLinks(st, nil))
	return ids, st.Close, true
}
