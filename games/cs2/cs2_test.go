package cs2_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gravel-project/gravel/games/cs2"
	"github.com/gravel-project/gravel/games/cs2/cs2test"
)

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseStatus(t *testing.T) {
	st, err := cs2.ParseStatus(read(t, "testdata/status/two_players.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Hostname != "HTG CS2 | 10-MAN #1" || st.Version != "1.40.6.4/14064 10551" || st.Map != "de_mirage" {
		t.Errorf("header: %q %q %q", st.Hostname, st.Version, st.Map)
	}
	if st.Humans != 2 || st.Bots != 1 || st.MaxPlayers != 10 || st.Hibernating {
		t.Errorf("counts: %+v", st)
	}
	if len(st.Players) != 3 {
		t.Fatalf("players %+v (the NoChan placeholder must be skipped)", st.Players)
	}
	p := st.Players[0]
	if p.UserID != 2 || p.Name != "Player One" || p.TimeOn != 23*time.Second || p.PingMs != 38 || p.State != "active" || p.Address != "198.51.100.10:27005" {
		t.Errorf("row 1: %+v", p)
	}
	if p := st.Players[1]; p.Name != "it's two" || p.TimeOn != time.Hour+2*time.Minute+11*time.Second || p.Loss != 1 {
		t.Errorf("row 2: %+v", p)
	}
	if p := st.Players[2]; p.Name != "Bot Ted" || p.TimeOn != 0 || p.Address != "" {
		t.Errorf("bot row: %+v", p)
	}

	st, err = cs2.ParseStatus(read(t, "testdata/status/empty.txt"))
	if err != nil || st.Humans != 0 || len(st.Players) != 0 || st.Map != "de_inferno" {
		t.Errorf("empty: %+v %v", st, err)
	}
	st, err = cs2.ParseStatus(read(t, "testdata/status/hibernating.txt"))
	if err != nil || !st.Hibernating || st.Map != "de_dust2" {
		t.Errorf("hibernating: %+v %v", st, err)
	}
	if _, err := cs2.ParseStatus("Unknown command \"status\"\n"); err == nil {
		t.Error("an answer that is not a status parsed")
	}
}

func newClient(t *testing.T, srv *cs2test.Server, password string) *cs2.Client {
	t.Helper()
	c, err := cs2.New(srv.Addr(), password, cs2.Options{DialTimeout: 2 * time.Second, Deadline: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClientCommands(t *testing.T) {
	srv := cs2test.New(t)
	srv.SetPlayers(cs2test.Player{UserID: 2, Name: "One", PingMs: 30}, cs2test.Player{UserID: 3, Name: "Two"})
	c := newClient(t, srv, cs2test.Password)
	ctx := context.Background()

	st, err := c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Humans != 2 || len(st.Players) != 2 || st.Players[0].PingMs != 30 || st.Map != "de_inferno" {
		t.Errorf("status %+v", st)
	}
	if err := c.Kick(ctx, 3, `bye "now"; quit`); err != nil {
		t.Fatal(err)
	}
	if err := c.Say(ctx, "server\nrestarting; in 10 s"); err != nil {
		t.Fatal(err)
	}
	if err := c.ChangeLevel(ctx, "de_nuke"); err != nil {
		t.Fatal(err)
	}
	if err := c.Quit(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{`kickid 3 "bye 'now', quit"`, `say "server restarting, in 10 s"`, "changelevel de_nuke", "quit"}
	if got := srv.Commands(); !slices.Equal(got, want) {
		t.Errorf("commands\n got %q\nwant %q", got, want)
	}
	if st, _ := c.Status(ctx); st.Humans != 1 || st.Map != "de_nuke" {
		t.Errorf("after kick and changelevel: %+v", st)
	}
}

func TestClientRefusals(t *testing.T) {
	srv := cs2test.New(t)
	ctx := context.Background()
	if _, err := newClient(t, srv, "wrong").Status(ctx); !errors.Is(err, cs2.ErrAuth) {
		t.Errorf("a wrong password: %v, want ErrAuth", err)
	}
	c := newClient(t, srv, "wrong")
	c.SetPassword(cs2test.Password)
	if _, err := c.Status(ctx); err != nil {
		t.Errorf("after SetPassword: %v", err)
	}
	if err := c.Say(ctx, strings.Repeat("x", cs2.MaxText+1)); !errors.Is(err, cs2.ErrTooLong) {
		t.Errorf("a long message: %v", err)
	}
	if err := c.ChangeLevel(ctx, "de_nuke; quit"); !errors.Is(err, cs2.ErrInvalid) {
		t.Errorf("a map with a command in it: %v", err)
	}
	if err := c.Kick(ctx, -1, "x"); !errors.Is(err, cs2.ErrInvalid) {
		t.Errorf("a negative user id: %v", err)
	}
	if len(srv.Commands()) != 0 {
		t.Errorf("refused commands were sent: %q", srv.Commands())
	}
	if _, err := cs2.New("no-port", "x", cs2.Options{}); err == nil {
		t.Error("an address without a port was taken")
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.Exec(cctx, "status"); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled context: %v", err)
	}
	dead, _ := cs2.New("127.0.0.1:1", "x", cs2.Options{DialTimeout: time.Second})
	if _, err := dead.Status(ctx); err == nil {
		t.Error("a closed port answered")
	}
}

func TestRoster(t *testing.T) {
	r, err := cs2.NewRoster("Etc/UTC")
	if err != nil {
		t.Fatal(err)
	}
	changes := r.PushBatch([]byte(read(t, "testdata/log/session.log")))
	var connected, gone []uint64
	over := false
	for _, c := range changes {
		if c.Connected != nil {
			connected = append(connected, c.Connected.SteamID64)
		}
		if c.Disconnected != nil {
			gone = append(gone, c.Disconnected.SteamID64)
		}
		over = over || c.MatchOver
	}
	const one, two = 76561197960287930, 76561197960287931 // [U:1:22202], [U:1:22203]
	if !slices.Equal(connected, []uint64{one, two}) || !slices.Equal(gone, []uint64{two}) || !over {
		t.Errorf("changes: connected %v gone %v over %v (the bot must not appear)", connected, gone, over)
	}
	ps := r.Players()
	if len(ps) != 1 || ps[0].SteamID64 != one || ps[0].Side != "CT" || !ps[0].Entered || ps[0].Name != "Player One" {
		t.Fatalf("roster %+v", ps)
	}
	if ps[0].Kills != 0 || ps[0].Deaths != 0 {
		t.Errorf("counters survived the game over: %+v", ps[0])
	}
	if p, ok := r.BySteamID64(one); !ok || p.UserID != 2 {
		t.Errorf("BySteamID64: %+v %v", p, ok)
	}
	if _, ok := r.ByUserID(3); ok {
		t.Error("a disconnected player is still listed")
	}
	if p, _ := r.ByUserID(2); !p.ConnectedAt.Equal(time.Date(2026, 10, 12, 19, 1, 0, 0, time.UTC)) {
		t.Errorf("connected at %v", p.ConnectedAt)
	}
}

func TestRosterCountsAndRestart(t *testing.T) {
	r, _ := cs2.NewRoster("")
	// The hub started mid-session: players it never saw connect are added from the lines they
	// appear in, keyed by their SteamID.
	changes := r.PushBatch([]byte(read(t, "testdata/log/restart.log")))
	if len(changes) != 1 || !changes[0].Reset {
		t.Errorf("changes %+v, want the restart", changes)
	}
	if n := len(r.Players()); n != 0 {
		t.Errorf("%d players after a restart", n)
	}
	r.PushBatch([]byte(strings.SplitN(read(t, "testdata/log/restart.log"), "\n", 2)[0]))
	ps := r.Players()
	if len(ps) != 2 || ps[0].Kills != 1 || ps[1].Deaths != 1 {
		t.Errorf("mid-session players %+v", ps)
	}
	r.Clear()
	if len(r.Players()) != 0 {
		t.Error("Clear left players")
	}
	if _, err := cs2.NewRoster("Not/AZone"); err == nil {
		t.Error("an unknown timezone was taken")
	}
}

func TestSteamID64(t *testing.T) {
	for in, want := range map[string]uint64{"[U:1:22202]": 76561197960287930, "STEAM_1:0:11101": 76561197960287930} {
		if got, ok := cs2.SteamID64(in); !ok || got != want {
			t.Errorf("SteamID64(%q) = %d %v", in, got, ok)
		}
	}
	for _, in := range []string{"BOT", "", "[U:1:x]"} {
		if _, ok := cs2.SteamID64(in); ok {
			t.Errorf("SteamID64(%q) ok", in)
		}
	}
}

func TestRedactLog(t *testing.T) {
	in := read(t, "testdata/log/session.log") + `10/12/2026 - 19:06:00.000 - rcon from "203.0.113.1:5000": command "echo tok-123"` + "\n"
	out := string(cs2.RedactLog([]byte(in), "tok-123", ""))
	for _, leaked := range []string{"hunter2-join", "tok-123"} {
		if strings.Contains(out, leaked) {
			t.Errorf("%q survived redaction", leaked)
		}
	}
	if !strings.Contains(out, `"sv_password" = "[REDACTED]"`) || !strings.Contains(out, `"tv_relaypassword" = ""`) || !strings.Contains(out, `"mp_friendlyfire" = "false"`) {
		t.Errorf("redaction changed the wrong lines:\n%s", out)
	}
	if strings.Count(out, "\n") != strings.Count(in, "\n") {
		t.Error("redaction changed the line count")
	}
}

func FuzzParseStatus(f *testing.F) {
	f.Add("players  : 1 humans, 0 bots (10 max)\n---------players--------\n    2 00:01 1 0 active 1 'x'\n#end\n")
	f.Fuzz(func(_ *testing.T, s string) { _, _ = cs2.ParseStatus(s) })
}

func FuzzRoster(f *testing.F) {
	f.Add(`10/12/2026 - 19:01:00.000 - "A<2><[U:1:1]><>" connected, address ""`)
	f.Fuzz(func(_ *testing.T, s string) {
		r, _ := cs2.NewRoster("")
		_, _ = r.Push(s)
		_ = cs2.RedactLog([]byte(s), "x")
	})
}
