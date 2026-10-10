package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gravel-project/gravel/internal/store"
)

// `stats erase` re-keys a player and says what it changed, without printing the identity back.
func TestStatsErase(t *testing.T) {
	st, cfgPath, dir, run := freshHub(t)
	if code, _, errOut := run("stats"); code != 2 || !strings.Contains(errOut, "usage: gravel-hub stats erase") {
		t.Errorf("no subcommand: %d %q", code, errOut)
	}
	if code, _, _ := run("stats", "erase", "--provider", "steam", "--config", cfgPath); code != 2 {
		t.Errorf("no --subject: %d", code)
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
	at := time.Now().Add(-time.Hour)
	match, err := st.StartMatch(ctx, store.Match{OrganizationID: o.ID, ServerID: "wd-1", GameID: "wardogs", StartedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	const subject = "76561190000000001"
	if err := st.SaveMatchPlayers(ctx, []store.MatchPlayer{{MatchID: match.ID, Provider: "steam", Subject: subject, Kills: 4,
		FirstSeen: at, LastSeen: at, Source: "server_adapter", Trust: "official"}}); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := run("stats", "erase", "--provider", "steam", "--subject", subject, "--config", cfgPath)
	if code != 0 || !strings.Contains(out, "erased: 1 match rows, 0 monthly rows, 0 ingest batches re-keyed; pseudonym deleted: false") {
		t.Fatalf("erase: %d %q %q", code, out, errOut)
	}
	if strings.Contains(out+errOut, subject) {
		t.Errorf("the identity was printed back: %q %q", out, errOut)
	}
	rows, err := st.MatchPlayers(ctx, match.ID)
	if err != nil || len(rows) != 1 || rows[0].Provider != store.ErasedProvider || rows[0].Kills != 4 {
		t.Errorf("rows = %+v, %v", rows, err)
	}
}
