package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

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

	app := store.App{ID: uuid.New(), OrganizationID: o.ID, Name: "seeder", ClientID: "gravel_seeder", SecretHash: []byte("x"), CreatedAt: at}
	if err := st.CreateApp(ctx, app, store.Actor{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordParticipation(ctx, []store.ParticipationEvent{{OrganizationID: o.ID, AppID: app.ID, Key: "s-1", Kind: "seeding",
		Provider: "steam", Subject: subject, OccurredAt: at, DurationSeconds: 600, ReceivedAt: at, Source: "first_party", SchemaVersion: 1}}); err != nil {
		t.Fatal(err)
	}

	// A two-map series the player played both maps of (ADR-0024): two result rows.
	res, err := st.StartMatchResult(ctx, store.MatchResult{ID: uuid.New(), OrganizationID: o.ID, ServerID: "wd-1", Source: "matchzy", ExternalID: 7, NumMaps: 2, StartedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	for m := 1; m <= 2; m++ {
		if err := st.ReplaceMatchResultPlayers(ctx, res.ID, m, []store.MatchResultPlayer{{Provider: "steam", Subject: subject, Name: "Jo", Team: "team1", Kills: m}}); err != nil {
			t.Fatal(err)
		}
	}

	// A session the poll stitched (ADR-0035).
	if _, err := st.RecordSessions(ctx, o.ID, "wd-1", "wardogs", at, at.Add(-time.Minute), []store.PlayerKey{{Provider: "steam", Subject: subject}}); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := run("stats", "erase", "--provider", "steam", "--subject", subject, "--config", cfgPath)
	if code != 0 || !strings.Contains(out, "erased: 1 match rows, 0 monthly rows, 1 participation events, 0 participation monthly rows, 2 match result rows, 1 sessions, 0 activity monthly rows, 0 ingest batches re-keyed; pseudonym deleted: false") {
		t.Fatalf("erase: %d %q %q", code, out, errOut)
	}
	if strings.Contains(out+errOut, subject) {
		t.Errorf("the identity was printed back: %q %q", out, errOut)
	}
	rows, err := st.MatchPlayers(ctx, match.ID)
	if err != nil || len(rows) != 1 || rows[0].Provider != store.ErasedProvider || rows[0].Kills != 4 {
		t.Errorf("rows = %+v, %v", rows, err)
	}
	events, err := st.ParticipationEvents(ctx, o.ID)
	if err != nil || len(events) != 1 || events[0].Provider != store.ErasedProvider || events[0].Subject == subject || events[0].DurationSeconds != 600 {
		t.Errorf("participation = %+v, %v", events, err)
	}
}
