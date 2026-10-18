package wardogs

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseFeedBatch(t *testing.T) {
	body := `{"serverId":"00000000-0000-4000-8000-000000000001","serverName":"Hidden Token Gaming","events":[
		{"eventId":"4f6c1a52-8a1e-4d0e-9a57-1d2b3c4d5e6f","type":"match_state","eventTime":0.132,"mapName":"Kavkazi","state":"MatchState.PreMatch"},
		{"eventId":"b2d4e6f8-0a1c-4e3a-8b5d-7f9e1a3c5b7d","type":"kill","eventTime":"soon","killerSteamId":"76561190000000001","extra":{"x":1}},
		{"eventId":"c3","type":"match_state","mapName":"Kavkazi","state":"MatchState.PreMatch.WaitingForWorldLoad","unknown":true}]}`
	b, err := ParseFeedBatch([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if b.ServerID != "00000000-0000-4000-8000-000000000001" || b.ServerName != "Hidden Token Gaming" || len(b.Events) != 3 {
		t.Fatalf("batch = %+v", b)
	}
	first := b.Events[0]
	if first.EventID != "4f6c1a52-8a1e-4d0e-9a57-1d2b3c4d5e6f" || first.Type != FeedMatchState || !first.HasTime || first.EventTime != 0.132 {
		t.Errorf("first = %+v", first)
	}
	if m, err := first.MatchState(); err != nil || m.MapName != "Kavkazi" || m.State != "MatchState.PreMatch" {
		t.Errorf("match state = %+v, %v", m, err)
	}
	// A time that is not a number is no time, and the event is kept whole for a reader that knows
	// its type.
	kill := b.Events[1]
	if kill.HasTime || kill.Type != "kill" || !strings.Contains(string(kill.Raw), `"killerSteamId":"76561190000000001"`) {
		t.Errorf("kill = %+v", kill)
	}
	if _, err := kill.MatchState(); err == nil {
		t.Error("a kill read as a match state")
	}
	if b.Events[2].HasTime {
		t.Error("an event with no time has one")
	}
	// The event writes back as posted.
	out, err := json.Marshal(b.Events[1])
	if err != nil || string(out) != string(kill.Raw) {
		t.Errorf("marshal = %s, %v", out, err)
	}
}

func TestParseFeedBatchRefuses(t *testing.T) {
	for _, body := range []string{``, `[]`, `[{"eventId":"a"}]`, `{"events":{"a":1}}`, `{"events":[1]}`, `{"serverId":`} {
		if _, err := ParseFeedBatch([]byte(body)); err == nil {
			t.Errorf("%q parsed", body)
		}
	}
	// No events, as a quiet server's batch may be, is a batch.
	for _, body := range []string{`{"serverId":"x","events":[]}`, `{"serverId":"x"}`, ` {"events":null} `} {
		if b, err := ParseFeedBatch([]byte(body)); err != nil || len(b.Events) != 0 {
			t.Errorf("%q: %+v, %v", body, b, err)
		}
	}
}
