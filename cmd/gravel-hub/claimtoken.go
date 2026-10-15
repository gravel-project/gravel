package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/store"
)

// claimTokenCmd is `gravel-hub claim-token`: an owner-claim token on demand, on the hub's host
// (ADR-0036). Shell access to the host is the proof reading the start-up log is (ADR-0002), so a
// control plane that made the hub, or an operator who missed the log line, gets one without a
// restart and without anything on the network. The token alone goes to stdout, for a script; the
// note for a person goes to stderr. It replaces any earlier token and is recorded as an event.
func claimTokenCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("claim-token", stderr)
	path := configFlag(fs)
	ttl := fs.Duration("ttl", 0, fmt.Sprintf("how long the token works, %s to %s (default claim.token_ttl)", org.MinClaimTokenTTL, org.MaxClaimTokenTTL))
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: gravel-hub claim-token [--ttl DURATION] [--config hub.yaml]")
		return 2
	}
	if *ttl != 0 && (*ttl < org.MinClaimTokenTTL || *ttl > org.MaxClaimTokenTTL) {
		fmt.Fprintf(stderr, "gravel-hub claim-token: --ttl %s: a claim token works from %s to %s\n", *ttl, org.MinClaimTokenTTL, org.MaxClaimTokenTTL)
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
	// The row exists once the hub has started; a database that was only migrated gets it here,
	// with the configured name, as settings apply does.
	if _, err := svc.CreateBuiltinIfAbsent(ctx, cfg.Organization.Name); err != nil {
		fmt.Fprintln(stderr, "gravel-hub claim-token:", err)
		return 1
	}
	token, expires, err := svc.MintClaimToken(ctx, *ttl, store.Actor{Via: store.ViaCLI})
	switch {
	case errors.Is(err, org.ErrAlreadyOwned):
		fmt.Fprintln(stderr, "gravel-hub claim-token: the hub is owned; an owned hub never mints a claim token again (hand it over with gravel owner nominate)")
		return 1
	case err != nil:
		fmt.Fprintln(stderr, "gravel-hub claim-token:", err)
		return 1
	}
	fmt.Fprintln(stdout, token)
	fmt.Fprintf(stderr, "owner-claim token: works once, until %s, and replaces any earlier one. Log in to the hub, then gravel claim TOKEN (or paste it on the account page).\n",
		expires.UTC().Format("2006-01-02 15:04 UTC"))
	return 0
}
