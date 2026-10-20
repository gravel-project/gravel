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

// TestUnknownAndInvalid: a type this version does not know is left in the batch (itemUsed, which
// one community sink models and no stock server has been seen to send); an event it cannot key or
// read is invalid; neither fails the batch.
func TestUnknownAndInvalid(t *testing.T) {
	got := parse(t, `{"serverId":"x","events":[
		{"eventId":"a","type":"itemUsed","eventTime":12.5,"playerSteamId":"76561190000000001"},
		{"type":"match_state","mapName":"Kavkazi","state":"MatchState.InProgress"},
		{"eventId":"b","mapName":"Kavkazi"},
		{"eventId":"c","type":"match_state","mapName":"Kavkazi"},
		{"eventId":"d","type":"match_state","mapName":7,"state":"MatchState.InProgress"},
		{"eventId":"e","type":"match_state","state":"MatchState.InProgress"}]}`)
	if len(got.Unknown) != 1 || got.Unknown[0] != "itemUsed" || got.Invalid != 4 || len(got.Events) != 1 {
		t.Fatalf("parsed = %+v", got)
	}
	if e := got.Events[0]; e.Key != "e" || e.Extensions["map"] != "" || e.Extensions["state"] != "MatchState.InProgress" {
		t.Errorf("event = %+v", e)
	}
	if _, ok := got.Events[0].Extensions["game_time"]; ok {
		t.Error("an event with no time has a game_time")
	}
}

// TestMatchEnded: an empty match running out on Hidden Token Gaming's server (2026-10-19 17:04Z):
// the server drew the winner (random_winner), every faction on 0, then the post-match state.
func TestMatchEnded(t *testing.T) {
	got := fixture(t, "match-ended.json")
	if len(got.Events) != 2 || len(got.Unknown) != 0 || got.Invalid != 0 {
		t.Fatalf("parsed = %+v", got)
	}
	e := got.Events[0]
	if e.Kind != KindMatchEnded || e.Key != "BF338A3B-4905-41D0-AD9F-1401CFCA66DF" || e.Player != (store.PlayerKey{}) || e.Target != (store.PlayerKey{}) {
		t.Errorf("event = %+v", e)
	}
	want := map[string]any{"map": "Kavkazi", "match_id": "054b8989-0234-42bb-878c-96bf4945834c", "winner": "Valkyra", "random_winner": true, "players": 0, "game_time": 86425.941098406911}
	for k, v := range want {
		if e.Extensions[k] != v {
			t.Errorf("%s = %v, want %v", k, e.Extensions[k], v)
		}
	}
	scores, _ := e.Extensions["scores"].(map[string]any)
	if len(scores) != 3 || scores["Lonestar"] != int64(0) || scores["Valkyra"] != int64(0) || scores["Manticore"] != int64(0) {
		t.Errorf("scores = %v", e.Extensions["scores"])
	}
	if s := got.Events[1]; s.Kind != KindMatchState || s.Extensions["state"] != "MatchState.PostMatch" {
		t.Errorf("then = %+v", s)
	}
}

// TestMatchEndedInvalid: an end with no winner is invalid; one with no count is kept without one.
func TestMatchEndedInvalid(t *testing.T) {
	got := parse(t, `{"serverId":"x","events":[
		{"eventId":"a","type":"match_ended","mapName":"Kavkazi","factionScores":[]},
		{"eventId":"b","type":"match_ended","winningFaction":"Lonestar","factionScores":"none"},
		{"eventId":"c","type":"match_ended","winningFaction":"Lonestar","factionScores":[{"faction":"Lonestar","score":100},{"faction":"Manticore","score":59}]}]}`)
	if got.Invalid != 2 || len(got.Events) != 1 {
		t.Fatalf("parsed = %+v", got)
	}
	e := got.Events[0]
	if _, ok := e.Extensions["players"]; ok || e.Extensions["random_winner"] != false {
		t.Errorf("extensions = %v", e.Extensions)
	}
	if s := e.Extensions["scores"].(map[string]any); s["Lonestar"] != int64(100) || s["Manticore"] != int64(59) {
		t.Errorf("scores = %v", s)
	}
}

// TestKilled: the synthetic batch (testdata/README.md: the community's fields, our stand-ins): a
// headshot from 45 m, a fall with no killer, item or distance, a grenade suicide, and a vehicle's
// explosion with no distance.
func TestKilled(t *testing.T) {
	got := fixture(t, "killed-synthetic.json")
	if len(got.Events) != 4 || len(got.Unknown) != 0 || got.Invalid != 0 {
		t.Fatalf("parsed = %+v", got)
	}
	p1 := store.PlayerKey{Provider: "steam", Subject: "76561190000000001"}
	p2 := store.PlayerKey{Provider: "steam", Subject: "76561190000000002"}
	for i, c := range []struct {
		player, target store.PlayerKey
		want           map[string]any
		absent         []string
	}{
		{p1, p2, map[string]any{"cause": "Id.Item.AK74M", "distance_cm": 4512.25, "headshot": true, "suicide": false, "game_time": 1200.5}, nil},
		{store.PlayerKey{}, p1, map[string]any{"headshot": false, "suicide": false, "game_time": 1260.75}, []string{"cause", "distance_cm"}},
		{p2, p2, map[string]any{"cause": "Id.Item.M67", "headshot": false, "suicide": true, "game_time": 1302.0}, []string{"distance_cm"}},
		{p2, p1, map[string]any{"cause": "Id.Item.BTR80", "headshot": false, "suicide": false, "game_time": 1340.25}, []string{"distance_cm"}},
	} {
		e := got.Events[i]
		if e.Kind != KindKill || e.Player != c.player || e.Target != c.target {
			t.Errorf("event %d = %+v", i, e)
		}
		if e.Extensions["map"] != "Kavkazi" || e.Extensions["match_id"] != "00000000-0000-4000-8003-000000000001" {
			t.Errorf("event %d extensions = %v", i, e.Extensions)
		}
		for k, v := range c.want {
			if e.Extensions[k] != v {
				t.Errorf("event %d: %s = %v, want %v", i, k, e.Extensions[k], v)
			}
		}
		for _, k := range c.absent {
			if _, ok := e.Extensions[k]; ok {
				t.Errorf("event %d: %s from nowhere: %v", i, k, e.Extensions)
			}
		}
		for _, k := range []string{"killer_name", "victim_name", "killer_id", "victim_id"} {
			if _, ok := e.Extensions[k]; ok {
				t.Errorf("event %d keeps %s", i, k)
			}
		}
	}
}

// TestKilledSuicideByID: a suicide the tags don't say is still one when the killer is the victim.
func TestKilledSuicideByID(t *testing.T) {
	got := parse(t, `{"serverId":"x","events":[{"eventId":"a","type":"killed","killerSteamId":"76561190000000001","victimSteamId":"76561190000000001"}]}`)
	if len(got.Events) != 1 || got.Events[0].Extensions["suicide"] != true {
		t.Fatalf("parsed = %+v", got)
	}
}

// TestKilledInvalid: a kill with no victim, a victim or killer that is not a SteamID64, or fields
// that cannot be read, is invalid.
func TestKilledInvalid(t *testing.T) {
	got := parse(t, `{"serverId":"x","events":[
		{"eventId":"a","type":"killed","killerSteamId":"76561190000000001"},
		{"eventId":"b","type":"killed","victimSteamId":"Player 2"},
		{"eventId":"c","type":"killed","killerSteamId":"Player 1","victimSteamId":"76561190000000002"},
		{"eventId":"d","type":"killed","victimSteamId":"76561190000000002","distance":"far"},
		{"eventId":"e","type":"killed","victimSteamId":76561190000000002,"contextTags":[]}]}`)
	if got.Invalid != 4 || len(got.Events) != 1 || got.Events[0].Key != "e" {
		t.Fatalf("parsed = %+v", got)
	}
}

// TestSinkDownFlush: the batch the server posted 19 s after nine minutes of 503s (deploy#49,
// 2026-10-20): the join and the leave it queued, together, each with the game time it happened
// at (2 min 29 s apart), not the time it was sent.
func TestSinkDownFlush(t *testing.T) {
	got := fixture(t, "sink-down-flush.json")
	if len(got.Events) != 2 || got.Invalid != 0 {
		t.Fatalf("parsed = %+v", got)
	}
	j, l := got.Events[0], got.Events[1]
	if j.Kind != KindPlayerJoined || l.Kind != KindPlayerLeft || j.Player != l.Player {
		t.Fatalf("events = %+v", got.Events)
	}
	if d := l.Extensions["game_time"].(float64) - j.Extensions["game_time"].(float64); d < 149 || d > 150 {
		t.Errorf("leave - join = %v s of game time, want 149.1", d)
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
