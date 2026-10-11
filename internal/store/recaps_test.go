package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

func TestSessionRecap(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	t0 := time.Date(2026, 10, 14, 23, 0, 0, 0, time.UTC) // a Wardogs Wednesday, 18:00 Chicago
	srv := func(id string) store.ManagedServer {
		return store.ManagedServer{ID: id, GameID: "wardogs", Name: id, Driver: "wardogs", Location: "slc", Endpoint: "http://203.0.113.10:7789",
			CredentialFile: "/run/secrets/wd", PollInterval: 15 * time.Second, Trust: "official"}
	}
	if err := st.ApplyServers(ctx, o.ID, []store.Game{{ID: "wardogs"}}, nil, []store.ManagedServer{srv("wd-1"), srv("wd-2")}, t0); err != nil {
		t.Fatal(err)
	}
	occ := func(server string, at time.Duration, fewest, most int) {
		t.Helper()
		if err := st.RecordOccupancy(ctx, o.ID, server, t0.Add(at), fewest, most); err != nil {
			t.Fatal(err)
		}
	}
	// Minute 0 is folded from two writes; minute 1 reaches 20 only at its peak; minutes 2 and 3 are
	// at 20 or more throughout; a minute before the window and another server's don't count.
	occ("wd-1", 15*time.Second, 7, 9)
	occ("wd-1", 45*time.Second, 5, 12)
	occ("wd-1", time.Minute, 18, 20)
	occ("wd-1", 2*time.Minute, 20, 23)
	occ("wd-1", 3*time.Minute+30*time.Second, 21, 25)
	occ("wd-1", -time.Minute, 30, 30)
	occ("wd-2", 2*time.Minute, 40, 40)
	if err := st.RecordOccupancy(ctx, o.ID, "wd-1", t0, 3, 2); err == nil {
		t.Error("a range whose most is under its fewest is refused")
	}

	// A match from before the window that runs into it, one inside it (split by a restart), and one
	// after it.
	before, _ := st.StartMatch(ctx, store.Match{OrganizationID: o.ID, ServerID: "wd-1", GameID: "wardogs", StartedAt: t0.Add(-30 * time.Minute), Map: "Ozeti", RotationIndex: 0})
	inside, _ := st.StartMatch(ctx, store.Match{OrganizationID: o.ID, ServerID: "wd-1", GameID: "wardogs", StartedAt: t0.Add(time.Hour), Map: "Europe", RotationIndex: 1})
	if err := st.SetMatchTeams(ctx, inside.ID, []store.TeamScore{{Name: "Alpha", Color: "#4CB1EF", Score: 1200}, {Name: "Bravo", Score: 900}}); err != nil {
		t.Fatal(err)
	}
	if err := st.EndMatch(ctx, inside.ID, t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	after, _ := st.StartMatch(ctx, store.Match{OrganizationID: o.ID, ServerID: "wd-1", GameID: "wardogs", StartedAt: t0.Add(5 * time.Hour)})
	row := func(match int64, subject string, kills, deaths int, first, last time.Duration) store.MatchPlayer {
		return store.MatchPlayer{MatchID: match, Provider: "steam", Subject: subject, Kills: kills, Deaths: deaths, SecondsOn: 600,
			FirstSeen: t0.Add(first), LastSeen: t0.Add(last), Source: "server_adapter", Trust: "official"}
	}
	if err := st.SaveMatchPlayers(ctx, []store.MatchPlayer{
		row(before.ID, "1", 4, 1, -30*time.Minute, 10*time.Minute),  // seen into the window
		row(before.ID, "9", 50, 0, -30*time.Minute, -5*time.Minute), // left before it: not counted
		row(inside.ID, "1", 6, 2, time.Hour, 2*time.Hour),
		row(inside.ID, "2", 10, 5, time.Hour, 2*time.Hour),
		row(inside.ID, "3", 0, 7, time.Hour, 2*time.Hour), // no kill: not in the top
		row(after.ID, "4", 99, 0, 5*time.Hour, 6*time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	r, err := st.SessionRecap(ctx, store.RecapQuery{OrganizationID: o.ID, ServerID: "wd-1", From: t0, To: t0.Add(4 * time.Hour), Floor: 20, Top: 5})
	if err != nil {
		t.Fatal(err)
	}
	if r.PeakPlayers != 25 || r.Minutes != 4 || r.AtFloor != 2 || r.Players != 3 {
		t.Errorf("recap: peak %d, minutes %d, at floor %d, players %d", r.PeakPlayers, r.Minutes, r.AtFloor, r.Players)
	}
	if len(r.Matches) != 2 || r.Matches[0].ID != before.ID || r.Matches[0].EndedAt == nil || r.Matches[1].ID != inside.ID ||
		r.Matches[0].Players != 1 || r.Matches[1].Players != 3 || r.Matches[1].Map != "Europe" {
		t.Fatalf("matches: %+v", r.Matches)
	}
	if len(r.Matches[0].Teams) != 0 || len(r.Matches[1].Teams) != 2 || r.Matches[1].Teams[0] != (store.TeamScore{Name: "Alpha", Color: "#4CB1EF", Score: 1200}) {
		t.Errorf("teams: %+v / %+v", r.Matches[0].Teams, r.Matches[1].Teams)
	}
	if len(r.Top) != 2 || r.Top[0].Subject != "1" || r.Top[0].Kills != 10 || r.Top[0].Matches != 2 || r.Top[1].Subject != "2" || r.Top[1].Kills != 10 { // a tie on kills: fewer deaths first
		t.Errorf("top: %+v", r.Top)
	}

	// Floor 0 counts no minute at the floor; Top 0 reads no top; a quiet window has nothing.
	r, err = st.SessionRecap(ctx, store.RecapQuery{OrganizationID: o.ID, ServerID: "wd-1", From: t0, To: t0.Add(4 * time.Hour)})
	if err != nil || r.AtFloor != 0 || r.Minutes != 4 || r.Top != nil {
		t.Errorf("no floor, no top: %+v %v", r, err)
	}
	r, err = st.SessionRecap(ctx, store.RecapQuery{OrganizationID: o.ID, ServerID: "wd-1", From: t0.Add(-48 * time.Hour), To: t0.Add(-47 * time.Hour), Floor: 20, Top: 5})
	if err != nil || r.PeakPlayers != 0 || r.Minutes != 0 || len(r.Matches) != 0 || r.Players != 0 || len(r.Top) != 0 {
		t.Errorf("nobody came: %+v %v", r, err)
	}
	// The open match counts while it overlaps.
	r, err = st.SessionRecap(ctx, store.RecapQuery{OrganizationID: o.ID, ServerID: "wd-1", From: t0.Add(5 * time.Hour), To: t0.Add(7 * time.Hour), Top: 5})
	if err != nil || len(r.Matches) != 1 || r.Matches[0].EndedAt != nil || len(r.Top) != 1 || r.Top[0].Kills != 99 {
		t.Errorf("open match: %+v %v", r, err)
	}
}
