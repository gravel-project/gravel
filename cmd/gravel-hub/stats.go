package main

import (
	"context"
	"fmt"
	"io"

	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/stats"
	"github.com/gravel-project/gravel/internal/store"
)

// statsCmd is `gravel-hub stats erase`: a player's erasure in the stats store (ADR-0012 §7), for an
// operator acting on a request they have verified. Their identity is replaced by a random token in
// the per-match rows, the monthly totals and the stored ingest batches, and their pseudonym is
// deleted; the numbers stay, shown as a deleted player. The erasure flow members start themselves
// comes with identity's.
func statsCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	const usageLine = "usage: gravel-hub stats erase --provider PROVIDER --subject SUBJECT [--config hub.yaml]"
	if len(args) == 0 || args[0] != "erase" {
		fmt.Fprintln(stderr, usageLine)
		return 2
	}
	fs := newFlagSet("stats erase", stderr)
	path := configFlag(fs)
	provider := fs.String("provider", "", "the identity's provider (steam)")
	subject := fs.String("subject", "", "the identity's subject at the provider (a SteamID64)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *provider == "" || *subject == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, usageLine)
		return 2
	}
	cfg, ok := load(*path, stderr)
	if !ok {
		return 1
	}
	logger := cfg.Log.NewLogger(io.Discard)
	st, err := store.Open(ctx, cfg.Database.URL, 1, cfg.Database.ConnectTimeout, logger)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer st.Close()
	organization, err := org.New(st, cfg.Claim.TokenTTL, logger).Get(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	got, err := stats.Erase(ctx, st, organization.ID, store.PlayerKey{Provider: *provider, Subject: *subject})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	// The identity is not printed back: the command line already holds it, and a log should not.
	fmt.Fprintf(stdout, "erased: %d match rows, %d monthly rows, %d ingest batches re-keyed; pseudonym deleted: %t\n",
		got.MatchRows, got.MonthlyRows, got.IngestBatches, got.Pseudonym)
	return 0
}
