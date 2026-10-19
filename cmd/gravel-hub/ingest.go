package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/gravel-project/gravel/games/wardogs/record"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/store"
)

// maxExport is the most batches one export prints.
const maxExport = 1000

// ingestCmd is `gravel-hub ingest export`: the batches a server pushed, as stored (ADR-0011), one
// JSON object per line, oldest first. It is how a deployment reads its captures: the first ones
// (S1), and each new kind of event a parser is to learn. The batches hold players' SteamIDs and
// the host's server id; with --redact they are replaced as a recording replaces them (the same
// stand-in for the same value across the export), so the output may leave the host and become a
// fixture.
func ingestCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "export" {
		fmt.Fprintln(stderr, "usage: gravel-hub ingest export --server ID [--after ID] [--limit N] [--redact] [--config hub.yaml]")
		return 2
	}
	fs := newFlagSet("ingest export", stderr)
	path := configFlag(fs)
	server := fs.String("server", "", "the server whose batches to print (its id in servers.yaml)")
	after := fs.Int64("after", 0, "print batches with an id above this one")
	limit := fs.Int("limit", 100, fmt.Sprintf("at most this many batches (1-%d)", maxExport))
	redact := fs.Bool("redact", false, "replace SteamIDs, the names beside them, addresses, server ids and secrets with stand-ins (JSON bodies only)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *server == "" || *limit < 1 || *limit > maxExport || fs.NArg() > 0 {
		fmt.Fprintf(stderr, "usage: gravel-hub ingest export --server ID [--after ID] [--limit 1-%d] [--redact] [--config hub.yaml]\n", maxExport)
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
	lines := make([]exported, len(batches))
	for i, b := range batches {
		lines[i] = exportLine(b)
	}
	if !*redact {
		enc := json.NewEncoder(stdout)
		for _, l := range lines {
			if err := enc.Encode(l); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
		}
		fmt.Fprintf(stderr, "%d batches; they hold players' SteamIDs: redact before they leave the host (--redact)\n", len(batches))
		return 0
	}
	out, counts, err := redactExport(lines)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if _, err := stdout.Write(out); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stderr, "%d batches, redacted: %d SteamIDs, %d names, %d addresses, %d server ids, %d secrets replaced\n",
		len(batches), counts.SteamIDs, counts.Names, counts.Addresses, counts.ServerIDs, counts.Secrets)
	return 0
}

// redactExport scrubs every batch's body and headers with one scrubber, so a player is the same
// stand-in in every batch, and checks the result holds none of what it replaced before any of it
// is printed. A body that is not JSON cannot be read for what to replace, so it is refused. The
// body's hash is left out: it is the hash of the body before.
func redactExport(lines []exported) ([]byte, record.Counts, error) {
	docs := make([]any, 0, 2*len(lines))
	for _, l := range lines {
		if l.Body == nil {
			return nil, record.Counts{}, fmt.Errorf("batch %d is not JSON: --redact reads JSON bodies only", l.ID)
		}
		for _, raw := range []json.RawMessage{l.Body, l.Headers} {
			var v any
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.UseNumber()
			if err := dec.Decode(&v); err != nil {
				return nil, record.Counts{}, fmt.Errorf("batch %d: %w", l.ID, err)
			}
			docs = append(docs, v)
		}
	}
	sc := record.NewScrubber("")
	docs = sc.Scrub(docs)
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	for i, l := range lines {
		body, err := json.Marshal(docs[2*i])
		if err != nil {
			return nil, record.Counts{}, err
		}
		headers, err := json.Marshal(docs[2*i+1])
		if err != nil {
			return nil, record.Counts{}, err
		}
		l.Body, l.Headers, l.SHA256 = body, headers, ""
		if err := enc.Encode(l); err != nil {
			return nil, record.Counts{}, err
		}
	}
	for _, o := range sc.Originals() {
		if bytes.Contains(b.Bytes(), []byte(o)) {
			return nil, record.Counts{}, errors.New("the redacted export still holds a value it replaced; nothing was printed")
		}
	}
	return b.Bytes(), sc.Counts(), nil
}

// exported is one batch as `ingest export` prints it: a JSON body as JSON, anything else as
// base64.
type exported struct {
	ID         int64           `json:"id"`
	Server     string          `json:"server"`
	Source     string          `json:"source"`
	ReceivedAt string          `json:"received_at"`
	State      string          `json:"state"`
	SHA256     string          `json:"sha256,omitempty"`
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
