package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"connectrpc.com/connect"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
)

// exportCmd is `gravel export -o FILE [--hub URL]` (gravel#213): everything the hub holds as the
// archive `gravel-hub export` writes, pulled over the API by its owner, in person, for a host
// without the box's shell (a hosted hub). FILE is written 0600 and never overwritten; "-" is
// stdout. `gravel-hub import` loads it into a fresh hub.
func exportCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("export", stderr)
	hf := addHubFlag(fs)
	out := fs.String("o", "", "the archive's file (written 0600; refused when it exists), or - for stdout (required)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *out == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: gravel export -o hub.tar.gz [--hub URL]")
		return exitUsage
	}
	c, err := openHub(hf)
	if err != nil {
		fmt.Fprintln(stderr, "gravel export:", err)
		return exitFailed
	}
	if c.cred != nil && !c.session() {
		fmt.Fprintf(stderr, "gravel export: the hub's owner exports it, in person; gravel login --local --hub %s first\n", c.hub)
		return exitLoggedIn
	}
	stream, err := c.organization().ExportHub(ctx, connect.NewRequest(&hubv1.ExportHubRequest{}))
	if err != nil {
		return c.fail(stderr, "export", err)
	}
	defer func() { _ = stream.Close() }()
	w := stdout
	var f *os.File
	if *out != "-" {
		if f, err = os.OpenFile(*out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err != nil {
			fmt.Fprintln(stderr, "gravel export:", err)
			return exitFailed
		}
		w = f
	}
	var n int64
	for stream.Receive() {
		m, err := w.Write(stream.Msg().GetChunk())
		n += int64(m)
		if err != nil {
			fmt.Fprintln(stderr, "gravel export:", err)
			return removeOnFail(f)
		}
	}
	if err := stream.Err(); err != nil {
		code := c.fail(stderr, "export", err)
		removeOnFail(f)
		return code
	}
	if f != nil {
		if err := f.Close(); err != nil {
			fmt.Fprintln(stderr, "gravel export:", err)
			return removeOnFail(f)
		}
		fmt.Fprintf(stderr, "exported %s to %s (%d bytes): it holds personal data and password hashes, keep it as you keep your backups\n", c.hub, *out, n)
	}
	return exitOK
}

// removeOnFail removes a half-written archive.
func removeOnFail(f *os.File) int {
	if f != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}
	return exitFailed
}
