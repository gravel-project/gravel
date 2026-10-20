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

// TestFeedEndpoint: GET /v1/feed with its endpoint mid-outage, as Hidden Token Gaming's server
// answered on 2026-10-20 (deploy#49): two events queued after seven failed posts, none dropped.
func TestFeedEndpoint(t *testing.T) {
	var f Feed
	if err := json.Unmarshal([]byte(`{"enabled":true,"endpointCount":1,"anyUnhealthy":true,"endpoints":[{"index":0,"scheme":"https",
		"host":"ingest.example.org","healthy":false,"queued":2,"inFlight":0,"consecutiveFailures":7,"sentTotal":4,"droppedTotal":0,
		"lastHttpStatus":503,"lastSuccessUnixSeconds":1792429805}]}`), &f); err != nil {
		t.Fatal(err)
	}
	want := FeedEndpoint{Scheme: "https", Host: "ingest.example.org", Queued: 2, ConsecutiveFailures: 7, SentTotal: 4, LastHTTPStatus: 503, LastSuccessUnixSeconds: 1792429805}
	if !f.Enabled || !f.AnyUnhealthy || len(f.Endpoints) != 1 || f.Endpoints[0] != want {
		t.Errorf("feed = %+v", f)
	}
}

// TestKillAndMatchEnded: the type's own readers refuse another type's event.
func TestKillAndMatchEnded(t *testing.T) {
	b, err := ParseFeedBatch([]byte(`{"events":[{"eventId":"a","type":"killed","victimSteamId":"76561190000000002","contextTags":["` + TagHeadshot + `"]},
		{"eventId":"b","type":"match_ended","winningFaction":"Manticore","factionScores":[{"faction":"Manticore","score":100}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	k, err := b.Events[0].Kill()
	if err != nil || !k.Headshot() || k.Suicide() || k.KillerSteamID != "" || k.Distance != nil {
		t.Errorf("kill = %+v, %v", k, err)
	}
	m, err := b.Events[1].MatchEnded()
	if err != nil || m.WinningFaction != "Manticore" || len(m.FactionScores) != 1 || m.FactionScores[0] != (FeedFactionScore{Faction: "Manticore", Score: 100}) || m.Players != nil {
		t.Errorf("match ended = %+v, %v", m, err)
	}
	if _, err := b.Events[0].MatchEnded(); err == nil {
		t.Error("a kill read as a match end")
	}
	if _, err := b.Events[1].Kill(); err == nil {
		t.Error("a match end read as a kill")
	}
}
