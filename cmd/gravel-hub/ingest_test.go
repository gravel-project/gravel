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
	// --redact refuses a body it cannot read, and prints nothing then.
	if code, out, errOut := run("ingest", "export", "--server", "wd-1", "--redact", "--config", cfgPath); code != 1 || out != "" || !strings.Contains(errOut, "is not JSON") {
		t.Errorf("--redact over a binary body: %d %q %q", code, out, errOut)
	}
	if code, out, errOut := run("ingest", "export", "--server", "wd-1", "--limit", "1", "--redact", "--config", cfgPath); code != 0 ||
		strings.Count(out, "\n") != 1 || strings.Contains(out, "sha256") || !strings.Contains(errOut, "1 batches, redacted") {
		t.Errorf("--redact: %d %q %q", code, out, errOut)
	}
}

// TestRedactExport: one stand-in per value across the whole export, the server id and the names
// beside a SteamID too; the hash goes (it is the original's); a body that is not JSON is refused.
func TestRedactExport(t *testing.T) {
	lines := []exported{
		{ID: 1, Server: "wd-1", SHA256: "abcd", Headers: json.RawMessage(`{}`),
			Body: json.RawMessage(`{"serverId":"a968511e-0000-4000-8000-123456789abc","serverName":"Hidden Token Gaming","events":[{"eventId":"k1","type":"kill","eventTime":12.25,"killerSteamId":"76561198000000042","killerName":"Sniper Wolf","victimSteamId":"76561198000000077"}]}`)},
		{ID: 2, Server: "wd-1", Headers: json.RawMessage(`{}`),
			Body: json.RawMessage(`{"serverId":"a968511e-0000-4000-8000-123456789abc","events":[{"eventId":"k2","type":"kill","killerSteamId":"76561198000000077","note":"Sniper Wolf from 203.0.113.9"}]}`)},
	}
	out, counts, err := redactExport(lines)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, leak := range []string{"76561198000000042", "76561198000000077", "Sniper Wolf", "a968511e", "203.0.113.9", "abcd"} {
		if strings.Contains(text, leak) {
			t.Errorf("the export holds %q:\n%s", leak, text)
		}
	}
	got := strings.Split(strings.TrimSpace(text), "\n")
	if len(got) != 2 {
		t.Fatalf("lines = %q", text)
	}
	// The victim of the first batch is the killer of the second: one stand-in for both.
	var a, b struct {
		Body struct {
			ServerID string `json:"serverId"`
			Events   []map[string]any
		} `json:"body"`
	}
	if json.Unmarshal([]byte(got[0]), &a) != nil || json.Unmarshal([]byte(got[1]), &b) != nil {
		t.Fatalf("not JSON lines: %q", text)
	}
	if victim, killer := a.Body.Events[0]["victimSteamId"], b.Body.Events[0]["killerSteamId"]; victim != killer || !strings.HasPrefix(victim.(string), "76561190000000") {
		t.Errorf("victim %v, then killer %v", victim, killer)
	}
	if a.Body.ServerID != b.Body.ServerID || !strings.HasPrefix(a.Body.ServerID, "00000000-0000-4000-8000-") {
		t.Errorf("server ids %q and %q", a.Body.ServerID, b.Body.ServerID)
	}
	if a.Body.Events[0]["killerName"] != "Player 1" || a.Body.Events[0]["eventTime"] != 12.25 || !strings.Contains(got[1], "Player 1 from 192.0.2.1") {
		t.Errorf("first = %v, second = %s", a.Body.Events[0], got[1])
	}
	if counts.SteamIDs != 2 || counts.Names != 1 || counts.ServerIDs != 1 || counts.Addresses != 1 {
		t.Errorf("counts = %+v", counts)
	}
	if _, _, err := redactExport([]exported{{ID: 3, BodyBase64: "/wE="}}); err == nil || !strings.Contains(err.Error(), "batch 3 is not JSON") {
		t.Errorf("a body that is not JSON: %v", err)
	}
}
