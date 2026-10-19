package wardogs

import (
	"os"
	"testing"
	"time"

	"github.com/gravel-project/gravel/internal/ingest"
	"github.com/gravel-project/gravel/internal/store"
)

func parse(t *testing.T, body string) ingest.Parsed {
	t.Helper()
	got, err := Feed{}.Parse(ingest.Batch{ServerID: "wd-1", ReceivedAt: time.Date(2026, 10, 18, 17, 4, 31, 0, time.UTC), Body: []byte(body)})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func fixture(t *testing.T, name string) ingest.Parsed {
	t.Helper()
	body, err := os.ReadFile("testdata/CL-516586/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return parse(t, string(body))
}

// TestServerStart: the first batch Hidden Token Gaming's server stored, at its start with nobody
// on (testdata/README.md says what is the capture's).
func TestServerStart(t *testing.T) {
	got := fixture(t, "server-start.json")
	if len(got.Events) != 2 || len(got.Unknown) != 0 || got.Invalid != 0 {
		t.Fatalf("parsed = %+v", got)
	}
	want := []string{"MatchState.PreMatch", "MatchState.PreMatch.WaitingForWorldLoad"}
	for i, e := range got.Events {
		if e.Kind != KindMatchState || e.Key == "" || !e.ObservedAt.IsZero() || e.Player != (e.Target) || e.Player.Subject != "" {
			t.Errorf("event %d = %+v", i, e)
		}
		if e.Extensions["map"] != "Kavkazi" || e.Extensions["state"] != want[i] || e.Extensions["game_time"] != 0.13216013461351395 {
			t.Errorf("event %d extensions = %v", i, e.Extensions)
		}
	}
	if got.Events[0].Key == got.Events[1].Key {
		t.Error("two events share a key")
	}
	if _, ok := got.Events[0].Extensions["match_id"]; ok {
		t.Error("a match id before there is a match")
	}
}

// TestWaitingForPlayers: from the wait for players on, a match_state carries the match's id.
func TestWaitingForPlayers(t *testing.T) {
	got := fixture(t, "waiting-for-players.json")
	if len(got.Events) != 2 || len(got.Unknown) != 0 || got.Invalid != 0 {
		t.Fatalf("parsed = %+v", got)
	}
	for _, e := range got.Events {
		if e.Kind != KindMatchState || e.Extensions["match_id"] != "054b8989-0234-42bb-878c-96bf4945834c" || e.Extensions["game_time"] != 141.09025091305375 {
			t.Errorf("event = %+v", e)
		}
	}
}

// TestPlayerJoinedAndLeft: the first session with a player on (2026-10-18 19:18Z): one player
// joins, then leaves; each is the player by SteamID, the map, the match and the count after, and
// neither keeps the name or the game's own id for them.
func TestPlayerJoinedAndLeft(t *testing.T) {
	for _, c := range []struct {
		file, kind string
		players    int
		gameTime   float64
	}{
		{"player-joined.json", KindPlayerJoined, 1, 8039.4227929785848},
		{"player-left.json", KindPlayerLeft, 0, 8061.3008470498025},
	} {
		got := fixture(t, c.file)
		if len(got.Events) != 1 || len(got.Unknown) != 0 || got.Invalid != 0 {
			t.Fatalf("%s: parsed = %+v", c.file, got)
		}
		e := got.Events[0]
		if e.Kind != c.kind || e.Key == "" || e.Player != (store.PlayerKey{Provider: "steam", Subject: "76561190000000001"}) || e.Target != (store.PlayerKey{}) {
			t.Errorf("%s: event = %+v", c.file, e)
		}
		want := map[string]any{"map": "Kavkazi", "match_id": "054b8989-0234-42bb-878c-96bf4945834c", "players": c.players, "game_time": c.gameTime}
		if len(e.Extensions) != len(want) {
			t.Errorf("%s: extensions = %v", c.file, e.Extensions)
		}
		for k, v := range want {
			if e.Extensions[k] != v {
				t.Errorf("%s: %s = %v, want %v", c.file, k, e.Extensions[k], v)
			}
		}
	}
}

// TestPlayerChangeInvalid: a join or a leave with no SteamID, or one that is not a SteamID64, or
// fields that cannot be read, is invalid; with no count it is kept without one.
func TestPlayerChangeInvalid(t *testing.T) {
	got := parse(t, `{"serverId":"x","events":[
		{"eventId":"a","type":"player_joined","mapName":"Kavkazi","players":1},
		{"eventId":"b","type":"player_left","playerSteamId":"Player 1","players":0},
		{"eventId":"c","type":"player_joined","playerSteamId":"76561190000000001","players":"one"},
		{"eventId":"d","type":"player_left","playerSteamId":76561190000000002}]}`)
	if got.Invalid != 3 || len(got.Events) != 1 || len(got.Unknown) != 0 {
		t.Fatalf("parsed = %+v", got)
	}
	e := got.Events[0]
	if e.Key != "d" || e.Kind != KindPlayerLeft || e.Player.Subject != "76561190000000002" {
		t.Errorf("event = %+v", e)
	}
	if _, ok := e.Extensions["players"]; ok {
		t.Errorf("a count from nowhere: %v", e.Extensions)
	}
}

// TestUnknownAndInvalid: a type this version does not know is left in the batch (kills, until a
// capture shows their fields); an event it cannot key or read is invalid; neither fails the batch.
func TestUnknownAndInvalid(t *testing.T) {
	got := parse(t, `{"serverId":"x","events":[
		{"eventId":"a","type":"kill","eventTime":12.5,"killerSteamId":"76561190000000001"},
		{"type":"match_state","mapName":"Kavkazi","state":"MatchState.InProgress"},
		{"eventId":"b","mapName":"Kavkazi"},
		{"eventId":"c","type":"match_state","mapName":"Kavkazi"},
		{"eventId":"d","type":"match_state","mapName":7,"state":"MatchState.InProgress"},
		{"eventId":"e","type":"match_state","state":"MatchState.InProgress"}]}`)
	if len(got.Unknown) != 1 || got.Unknown[0] != "kill" || got.Invalid != 4 || len(got.Events) != 1 {
		t.Fatalf("parsed = %+v", got)
	}
	if e := got.Events[0]; e.Key != "e" || e.Extensions["map"] != "" || e.Extensions["state"] != "MatchState.InProgress" {
		t.Errorf("event = %+v", e)
	}
	if _, ok := got.Events[0].Extensions["game_time"]; ok {
		t.Error("an event with no time has a game_time")
	}
}

func TestNotABatch(t *testing.T) {
	for _, body := range []string{`[{"eventId":"a"}]`, `not json`, ``} {
		if _, err := (Feed{}).Parse(ingest.Batch{Body: []byte(body)}); err == nil {
			t.Errorf("%q parsed", body)
		}
	}
	// A quiet server's batch has no events and is no failure.
	if got := parse(t, `{"serverId":"x","serverName":"y","events":[]}`); len(got.Events)+len(got.Unknown)+got.Invalid != 0 {
		t.Errorf("parsed = %+v", got)
	}
}
