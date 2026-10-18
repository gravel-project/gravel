package wardogs

import (
	"os"
	"testing"
	"time"

	"github.com/gravel-project/gravel/internal/ingest"
)

func parse(t *testing.T, body string) ingest.Parsed {
	t.Helper()
	got, err := Feed{}.Parse(ingest.Batch{ServerID: "wd-1", ReceivedAt: time.Date(2026, 10, 18, 17, 4, 31, 0, time.UTC), Body: []byte(body)})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestServerStart: the first batch Hidden Token Gaming's server stored, at its start with nobody
// on (testdata/README.md says what is the capture's).
func TestServerStart(t *testing.T) {
	body, err := os.ReadFile("testdata/CL-516586/server-start.json")
	if err != nil {
		t.Fatal(err)
	}
	got := parse(t, string(body))
	if len(got.Events) != 2 || len(got.Unknown) != 0 || got.Invalid != 0 {
		t.Fatalf("parsed = %+v", got)
	}
	want := []string{"MatchState.PreMatch", "MatchState.PreMatch.WaitingForWorldLoad"}
	for i, e := range got.Events {
		if e.Kind != KindMatchState || e.Key == "" || !e.ObservedAt.IsZero() || e.Player != (e.Target) || e.Player.Subject != "" {
			t.Errorf("event %d = %+v", i, e)
		}
		if e.Extensions["map"] != "Kavkazi" || e.Extensions["state"] != want[i] || e.Extensions["game_time"] != 0.132 {
			t.Errorf("event %d extensions = %v", i, e.Extensions)
		}
	}
	if got.Events[0].Key == got.Events[1].Key {
		t.Error("two events share a key")
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
