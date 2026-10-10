package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/store"
)

// maxExport is the most batches one export prints.
const maxExport = 1000

// ingestCmd is `gravel-hub ingest export`: the batches a server pushed, as stored (ADR-0011), one
// JSON object per line, oldest first. It is how a deployment reads its first real captures (S1)
// before a parser exists. The batches hold players' SteamIDs: redact them before they leave the
// host or become fixtures.
func ingestCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "export" {
		fmt.Fprintln(stderr, "usage: gravel-hub ingest export --server ID [--after ID] [--limit N] [--config hub.yaml]")
		return 2
	}
	fs := newFlagSet("ingest export", stderr)
	path := configFlag(fs)
	server := fs.String("server", "", "the server whose batches to print (its id in servers.yaml)")
	after := fs.Int64("after", 0, "print batches with an id above this one")
	limit := fs.Int("limit", 100, fmt.Sprintf("at most this many batches (1-%d)", maxExport))
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *server == "" || *limit < 1 || *limit > maxExport || fs.NArg() > 0 {
		fmt.Fprintf(stderr, "usage: gravel-hub ingest export --server ID [--after ID] [--limit 1-%d] [--config hub.yaml]\n", maxExport)
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
	batches, err := st.ListIngestBatches(ctx, organization.ID, *server, *after, *limit)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	enc := json.NewEncoder(stdout)
	for _, b := range batches {
		if err := enc.Encode(exportLine(b)); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	fmt.Fprintf(stderr, "%d batches; they hold players' SteamIDs: redact before they leave the host\n", len(batches))
	return 0
}

// exported is one batch as `ingest export` prints it: a JSON body as JSON, anything else as
// base64.
type exported struct {
	ID         int64           `json:"id"`
	Server     string          `json:"server"`
	Source     string          `json:"source"`
	ReceivedAt string          `json:"received_at"`
	State      string          `json:"state"`
	SHA256     string          `json:"sha256"`
	Headers    json.RawMessage `json:"headers"`
	Body       json.RawMessage `json:"body,omitempty"`
	BodyBase64 string          `json:"body_base64,omitempty"`
}

func exportLine(b store.IngestBatch) exported {
	out := exported{
		ID: b.ID, Server: b.ServerID, Source: b.Source, ReceivedAt: b.ReceivedAt.UTC().Format(time.RFC3339Nano),
		State: b.State, SHA256: hex.EncodeToString(b.BodySHA256), Headers: b.Headers,
	}
	if json.Valid(b.Body) {
		out.Body = b.Body
	} else {
		out.BodyBase64 = base64.StdEncoding.EncodeToString(b.Body)
	}
	return out
}
