package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gravel-project/gravel/games/wardogs"
	"github.com/gravel-project/gravel/games/wardogs/record"
)

// wardogsCmd is `gravel-hub wardogs record`: a War Dogs server's read-only answers as per-build
// fixtures under games/wardogs/testdata/<build>/ (ADR-0010). A human reviews and commits them;
// the client's contract tests run against every recorded build.
func wardogsCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "record" {
		fmt.Fprintln(stderr, "usage: gravel-hub wardogs record --base-url URL [--token-file FILE] [--dir games/wardogs/testdata] [--audit-limit 20]")
		return 2
	}
	fs := newFlagSet("wardogs record", stderr)
	baseURL := fs.String("base-url", "", "the server's RCON origin, http://host:port")
	tokenFile := fs.String("token-file", "", "a file holding the RCON password; without one only the public routes are recorded")
	dir := fs.String("dir", "games/wardogs/testdata", "the testdata directory; the recording goes in <dir>/<build>/")
	auditLimit := fs.Int("audit-limit", 20, "admin-log entries to record (1-500)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *baseURL == "" || fs.NArg() > 0 || *auditLimit < 1 || *auditLimit > 500 {
		fmt.Fprintln(stderr, "usage: gravel-hub wardogs record --base-url URL [--token-file FILE] [--dir games/wardogs/testdata] [--audit-limit 20]")
		return 2
	}
	token := ""
	if *tokenFile != "" {
		data, err := os.ReadFile(*tokenFile)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if token = strings.TrimSpace(string(data)); token == "" {
			fmt.Fprintf(stderr, "%s is empty\n", *tokenFile)
			return 1
		}
		fmt.Fprintf(stdout, "token from %s\n", *tokenFile)
	} else {
		fmt.Fprintln(stdout, "no --token-file: recording the public routes only")
	}
	client, err := wardogs.New(*baseURL, wardogs.Options{Token: token, UserAgent: "gravel-hub/" + buildVersion() + " (wardogs record)"})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	rep, err := record.Record(ctx, client, record.Options{
		Dir:        *dir,
		Token:      token,
		AuditLimit: *auditLimit,
		Recorder:   "gravel-hub wardogs record " + buildVersion(),
	})
	if err != nil {
		if errors.Is(err, wardogs.ErrTokenRefused) {
			fmt.Fprintln(stderr, "the server refused the token; not retrying (three bad tokens lock its protected routes). Check the file and run again.")
		}
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "recorded %s (API %s) in %s: %d files\n", rep.Build, rep.APIVersion, rep.Dir, len(rep.Files))
	for _, s := range rep.Skipped {
		fmt.Fprintf(stdout, "  skipped %s: %s\n", s.Route, s.Reason)
	}
	fmt.Fprintf(stdout, "scrubbed %d SteamIDs, %d player names, %d addresses, %d server ids, %d secrets\n",
		rep.Scrubbed.SteamIDs, rep.Scrubbed.Names, rep.Scrubbed.Addresses, rep.Scrubbed.ServerIDs, rep.Scrubbed.Secrets)
	switch {
	case rep.Previous == "":
		fmt.Fprintln(stdout, "no earlier build recorded to compare with")
	case rep.Diff.Empty():
		fmt.Fprintf(stdout, "routes unchanged since %s\n", rep.Previous)
	default:
		fmt.Fprintf(stdout, "routes since %s:\n", rep.Previous)
		for _, r := range rep.Diff.Added {
			fmt.Fprintf(stdout, "  + %s\n", r)
		}
		for _, r := range rep.Diff.Removed {
			fmt.Fprintf(stdout, "  - %s\n", r)
		}
		for _, r := range rep.Diff.Renamed {
			fmt.Fprintf(stdout, "  ~ %s\n", r)
		}
	}
	fmt.Fprintln(stdout, "Review every file before committing: the scrub replaces SteamIDs, the names beside them, addresses and secrets, and cannot recognise a name in free text it has not seen beside a SteamID.")
	return 0
}
