package cs2_test

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"

	"github.com/gravel-project/gravel/drivers"
	"github.com/gravel-project/gravel/drivers/cloudvm"
	cs2game "github.com/gravel-project/gravel/drivers/cloudvm/cs2"
	"github.com/gravel-project/gravel/games/cs2/cs2test"
)

func TestAdapter(t *testing.T) {
	srv := cs2test.New(t)
	srv.SetPlayers(cs2test.Player{UserID: 2, Name: "Player One", PingMs: 30}, cs2test.Player{UserID: 3, Name: "Two"})
	g, err := cs2game.New(srv.Addr(), cs2test.Password, cloudvm.Profile{LogTimezone: "Etc/UTC"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// The log names player 2 (connect line with their SteamID3).
	joined := g.Log([]byte(`10/12/2026 - 19:01:00.000 - "Player One<2><[U:1:22202]><>" connected, address "198.51.100.2:27005"` + "\n" +
		`10/12/2026 - 19:01:06.000 - "Player One<2><[U:1:22202]>" switched from team <Unassigned> to <CT>` + "\n"))
	steam := drivers.Identity{Provider: "steam", Subject: "76561197960287930"}
	if len(joined) != 1 || joined[0].UserID != 2 || joined[0].Identity != steam {
		t.Fatalf("joined %+v", joined)
	}
	st, err := g.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Players != 2 || st.MaxPlayers != 10 || st.Map != "de_inferno" || st.Build == "" || len(st.Rows) != 2 {
		t.Fatalf("status %+v", st)
	}
	if r := st.Rows[0]; r.Identity != steam || r.Team != "CT" || r.PingMs != 30 {
		t.Errorf("row 1 %+v", r)
	}
	if r := st.Rows[1]; r.Identity != (drivers.Identity{}) || r.Name != "Two" {
		t.Errorf("a player the log has not named yet: %+v", r)
	}
	p, ok := g.Find(steam)
	if !ok || p.UserID != 2 {
		t.Fatalf("Find %+v %v", p, ok)
	}
	if _, ok := g.Find(drivers.Identity{Provider: "xbox", Subject: "1"}); ok {
		t.Error("found a player by another provider's id")
	}
	if err := g.Kick(ctx, p.UserID, "bye"); err != nil {
		t.Fatal(err)
	}
	if err := g.Say(ctx, "hi"); err != nil {
		t.Fatal(err)
	}
	if err := g.Quit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := srv.Commands(); !slices.Equal(got, []string{`kickid 2 "bye"`, `say "hi"`, "quit"}) {
		t.Errorf("commands %q", got)
	}
	g.Forget()
	if _, ok := g.Find(steam); ok {
		t.Error("Forget kept the roster")
	}
}
