package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

func TestPlayerStats(t *testing.T) {
	st := storetest.Open(t)
	storetest.Reset(t, st)
	ctx := context.Background()
	o := newOrg(t, st)
	t0 := time.Date(2026, 10, 10, 20, 0, 0, 0, time.UTC)
	srv := func(id, trust string) store.ManagedServer {
		return store.ManagedServer{ID: id, GameID: "wardogs", Name: id, Driver: "wardogs", Location: "slc", Endpoint: "http://203.0.113.10:7789",
			CredentialFile: "/run/secrets/wd", PollInterval: 15 * time.Second, Trust: trust}
	}
	if err := st.ApplyServers(ctx, o.ID, []store.Game{{ID: "wardogs"}}, []store.ManagedServer{srv("wd-1", "official"), srv("wd-2", "community")}, t0); err != nil {
		t.Fatal(err)
	}

	// Two matches on wd-1 (the second starting ends the first), one on the community server.
	m1, err := st.StartMatch(ctx, store.Match{OrganizationID: o.ID, ServerID: "wd-1", GameID: "wardogs", StartedAt: t0, Map: "Ozeti", RotationIndex: 0})
	if err != nil {
		t.Fatal(err)
	}
	if open, err := st.OpenMatch(ctx, o.ID, "wd-1"); err != nil || open.ID != m1.ID || open.Map != "Ozeti" {
		t.Fatalf("open = %+v, %v", open, err)
	}
	row := func(match int64, subject, trust string, kills, deaths, secs int) store.MatchPlayer {
		return store.MatchPlayer{MatchID: match, Provider: "steam", Subject: subject, Team: "A", Kills: kills, Deaths: deaths, SecondsOn: secs,
			FirstSeen: t0, LastSeen: t0, Source: "server_adapter", Trust: trust}
	}
	if err := st.SaveMatchPlayers(ctx, []store.MatchPlayer{row(m1.ID, "1", "official", 5, 2, 600), row(m1.ID, "2", "official", 1, 4, 300)}); err != nil {
		t.Fatal(err)
	}
	// An update keeps first_seen, source and trust.
	up := row(m1.ID, "1", "community", 7, 2, 900)
	up.FirstSeen = t0.Add(time.Hour)
	if err := st.SaveMatchPlayers(ctx, []store.MatchPlayer{up}); err != nil {
		t.Fatal(err)
	}
	ps, err := st.MatchPlayers(ctx, m1.ID)
	if err != nil || len(ps) != 2 || ps[0].Kills != 7 || ps[0].SecondsOn != 900 || ps[0].Trust != "official" || !ps[0].FirstSeen.Equal(t0) {
		t.Fatalf("players = %+v, %v", ps, err)
	}
	m2, err := st.StartMatch(ctx, store.Match{OrganizationID: o.ID, ServerID: "wd-1", GameID: "wardogs", StartedAt: t0.Add(24 * time.Hour), RotationIndex: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveMatchPlayers(ctx, []store.MatchPlayer{row(m2.ID, "1", "official", 3, 3, 600), row(m2.ID, "3", "official", 9, 1, 600)}); err != nil {
		t.Fatal(err)
	}
	m3, err := st.StartMatch(ctx, store.Match{OrganizationID: o.ID, ServerID: "wd-2", GameID: "wardogs", StartedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveMatchPlayers(ctx, []store.MatchPlayer{row(m3.ID, "4", "community", 50, 0, 600)}); err != nil {
		t.Fatal(err)
	}
	if err := st.EndMatch(ctx, m2.ID, t0.Add(25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.OpenMatch(ctx, o.ID, "wd-1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("no open match after the end: %v", err)
	}

	board := func(q store.BoardQuery) []store.BoardRow {
		t.Helper()
		q.OrganizationID = o.ID
		if q.Limit == 0 {
			q.Limit = 10
		}
		if q.Trusts == nil {
			q.Trusts = []string{"official"}
		}
		if q.MinMatches == 0 {
			q.MinMatches = 1
		}
		rows, err := st.Board(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	subjects := func(rows []store.BoardRow) string {
		var s string
		for _, r := range rows {
			s += r.Subject
		}
		return s
	}
	// The organization's kills board: Official rows only (the community server's 50 kills don't count).
	rows := board(store.BoardQuery{Metric: store.BoardKills})
	if subjects(rows) != "132" || rows[0].Kills != 10 || rows[0].Deaths != 5 || rows[0].Matches != 2 || rows[0].SecondsOn != 1500 {
		t.Errorf("kills board = %+v", rows)
	}
	// The community server's own board counts its rows.
	if rows := board(store.BoardQuery{ServerID: "wd-2", Metric: store.BoardKills, Trusts: []string{"official", "community"}}); subjects(rows) != "4" {
		t.Errorf("a community server's board = %+v", rows)
	}
	// K/D with a floor of 2 matches: only player 1 qualifies.
	if rows := board(store.BoardQuery{Metric: store.BoardKD, MinMatches: 2}); subjects(rows) != "1" {
		t.Errorf("kd board = %+v", rows)
	}
	// A window: matches starting on the first day only.
	if rows := board(store.BoardQuery{Metric: store.BoardKills, From: t0, To: t0.Add(time.Hour)}); subjects(rows) != "12" {
		t.Errorf("windowed board = %+v", rows)
	}
	if rows := board(store.BoardQuery{Metric: store.BoardKills, Limit: 1, Offset: 1}); subjects(rows) != "3" {
		t.Errorf("a page = %+v", rows)
	}
	if _, err := st.Board(ctx, store.BoardQuery{OrganizationID: o.ID, Metric: "headshots", Limit: 1}); err == nil {
		t.Error("an unknown metric is refused")
	}

	ms, err := st.ListMatches(ctx, o.ID, "wd-1", 0, 10)
	if err != nil || len(ms) != 2 || ms[0].ID != m2.ID || ms[0].Players != 2 || ms[0].Kills != 12 || ms[1].EndedAt == nil {
		t.Errorf("matches = %+v, %v", ms, err)
	}
	if older, _ := st.ListMatches(ctx, o.ID, "wd-1", m2.ID, 10); len(older) != 1 || older[0].ID != m1.ID {
		t.Errorf("before m2 = %+v", older)
	}

	// Pseudonyms: stored once, unique in the organization.
	k1, k2 := store.PlayerKey{Provider: "steam", Subject: "1"}, store.PlayerKey{Provider: "steam", Subject: "2"}
	if n, err := st.AddPseudonym(ctx, o.ID, k1, "Brave Falcon", t0); err != nil || n != "Brave Falcon" {
		t.Fatalf("add = %q %v", n, err)
	}
	if n, err := st.AddPseudonym(ctx, o.ID, k1, "Other Name", t0); err != nil || n != "Brave Falcon" {
		t.Errorf("an identity keeps its pseudonym: %q %v", n, err)
	}
	if _, err := st.AddPseudonym(ctx, o.ID, k2, "Brave Falcon", t0); !errors.Is(err, store.ErrPseudonymTaken) {
		t.Errorf("a taken name: %v", err)
	}
	got, err := st.Pseudonyms(ctx, o.ID, []store.PlayerKey{k1, k2})
	if err != nil || len(got) != 1 || got[k1] != "Brave Falcon" {
		t.Errorf("pseudonyms = %v, %v", got, err)
	}

	// Members: a linked identity resolves, with the member's choice to be shown.
	u := uuid.New()
	if _, err := st.RegisterUser(ctx, store.User{ID: u, OrganizationID: o.ID, DisplayName: "Ana"},
		store.Identity{Provider: "steam", Subject: "1", UserID: u, VerificationMethod: "openid", VerifiedAt: t0}); err != nil {
		t.Fatal(err)
	}
	mem, err := st.BoardMembers(ctx, []store.PlayerKey{k1, k2})
	if err != nil || len(mem) != 1 || mem[k1].DisplayName != "Ana" || mem[k1].ShowName {
		t.Errorf("members = %+v, %v", mem, err)
	}
	if err := st.SetShowNameOnBoards(ctx, u, true); err != nil {
		t.Fatal(err)
	}
	if show, err := st.ShowNameOnBoards(ctx, u); err != nil || !show {
		t.Errorf("show = %v, %v", show, err)
	}
	if err := st.SetShowNameOnBoards(ctx, uuid.New(), true); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an unknown user: %v", err)
	}
}
