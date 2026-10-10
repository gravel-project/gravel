package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gravel-project/gravel/internal/store"
)

func TestIngestExport(t *testing.T) {
	st, cfgPath, dir, run := freshHub(t)
	if code, _, errOut := run("ingest"); code != 2 || !strings.Contains(errOut, "usage: gravel-hub ingest export") {
		t.Errorf("no subcommand: %d %q", code, errOut)
	}
	if code, _, _ := run("ingest", "export", "--config", cfgPath); code != 2 {
		t.Errorf("no --server: %d", code)
	}
	if code, _, _ := run("ingest", "export", "--server", "wd-1", "--limit", "0", "--config", cfgPath); code != 2 {
		t.Errorf("limit 0: %d", code)
	}

	cred := filepath.Join(dir, "rcon")
	writeFile(t, cred, "secret\n")
	m := filepath.Join(dir, "servers.yaml")
	writeFile(t, m, serversManifest(cred, "War Dogs #1"))
	if code, _, errOut := run("servers", "apply", m, "--config", cfgPath); code != 0 {
		t.Fatalf("apply: %d %q", code, errOut)
	}
	ctx := context.Background()
	o, err := st.GetBuiltinOrganization(ctx)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	for _, body := range [][]byte{[]byte(`[{"eventId":"e-1"}]`), {0xff, 0x01}} {
		if _, err := st.InsertIngestBatch(ctx, store.IngestBatch{OrganizationID: o.ID, ServerID: "wd-1", Source: "wardogs", ReceivedAt: at, Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	code, out, errOut := run("ingest", "export", "--server", "wd-1", "--config", cfgPath)
	if code != 0 || !strings.Contains(errOut, "2 batches") || !strings.Contains(errOut, "SteamIDs") {
		t.Fatalf("export: %d %q", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %q", out)
	}
	var first, second map[string]any
	if json.Unmarshal([]byte(lines[0]), &first) != nil || json.Unmarshal([]byte(lines[1]), &second) != nil {
		t.Fatalf("not JSON lines: %q", out)
	}
	if body, _ := json.Marshal(first["body"]); string(body) != `[{"eventId":"e-1"}]` || first["state"] != "pending" || first["received_at"] != "2026-10-10T12:00:00Z" {
		t.Errorf("a JSON body: %v", first)
	}
	if second["body_base64"] != "/wE=" || second["body"] != nil {
		t.Errorf("a binary body: %v", second)
	}
	if code, out, _ := run("ingest", "export", "--server", "wd-1", "--after", "1", "--config", cfgPath); code != 0 || strings.Count(out, "\n") != 1 {
		t.Errorf("--after: %d %q", code, out)
	}
}
