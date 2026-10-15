package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

// Migration 32 (ADR-0033) moves the five fixed CS2 columns into a player's extensions and gives
// each series its server's trust; going down puts the columns back from the extensions.
func TestMigrationStatExtensions(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	seedServers(t, st, o.ID)
	ids := map[string]uuid.UUID{}
	for _, server := range []string{"wd-1", "wd-2"} {
		r, err := st.StartMatchResult(ctx, store.MatchResult{ID: uuid.New(), OrganizationID: o.ID, ServerID: server, Source: "matchzy", ExternalID: 1,
			StartedAt: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		ids[server] = r.ID
	}
	if err := st.MigrateDownTo(ctx, 31); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, storetest.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, `INSERT INTO match_result_players (result_id, map_number, provider, subject, name, team, kills, deaths,
		assists, damage, headshot_kills, mvps, score) VALUES ($1, 1, 'steam', '1', 'Jo', 'team1', 21, 12, 4, 2310, 9, 0, 55)`, ids["wd-1"]); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetMatchResult(ctx, o.ID, ids["wd-1"])
	if err != nil || len(got.Players) != 1 {
		t.Fatalf("after the migration: %+v, %v", got, err)
	}
	want := map[string]float64{"assists": 4, "damage": 2310, "headshot_kills": 9, "mvps": 0, "score": 55}
	if p := got.Players[0]; p.Kills != 21 || p.Deaths != 12 || len(p.Extensions) != len(want) {
		t.Errorf("player = %+v", p)
	} else {
		for k, v := range want {
			if p.Extensions[k] != v {
				t.Errorf("%s = %v, want %v", k, p.Extensions[k], v)
			}
		}
	}
	if got.Trust != "official" {
		t.Errorf("wd-1's series has trust %q", got.Trust)
	}
	if r, err := st.GetMatchResult(ctx, o.ID, ids["wd-2"]); err != nil || r.Trust != "community" {
		t.Errorf("wd-2's series: trust %q, %v", r.Trust, err)
	}

	// Down again: the columns come back from the extensions.
	if err := st.MigrateDownTo(ctx, 31); err != nil {
		t.Fatal(err)
	}
	var assists, damage, mvps, score int
	if err := conn.QueryRow(ctx, `SELECT assists, damage, mvps, score FROM match_result_players WHERE result_id = $1`, ids["wd-1"]).
		Scan(&assists, &damage, &mvps, &score); err != nil || assists != 4 || damage != 2310 || mvps != 0 || score != 55 {
		t.Errorf("after going down: %d %d %d %d, %v", assists, damage, mvps, score, err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
}
