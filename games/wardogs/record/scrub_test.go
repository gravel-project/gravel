package record

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestScrubberReplacesTheGamesPlayerIDs: beside a SteamID, the game's own id for the player (the
// feed's playerId) is a person too, so it gets a stand-in, the same one for the same player across
// batches; a match's, an event's or a team's id is kept, and a scrubbed batch scrubs to itself.
func TestScrubberReplacesTheGamesPlayerIDs(t *testing.T) {
	doc := func(s string) any {
		var v any
		d := json.NewDecoder(strings.NewReader(s))
		d.UseNumber()
		if err := d.Decode(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	joined := `{"serverId":"9f1c","events":[{"eventId":"E1","type":"player_joined","matchId":"M1","teamId":"T1",
		"playerId":"acct-77","playerName":"Jo","playerSteamId":"76561198012345678","players":1}]}`
	left := `{"serverId":"9f1c","events":[{"eventId":"E2","type":"player_left","matchId":"M1",
		"playerId":"acct-77","playerName":"Jo","playerSteamId":"76561198012345678","players":0}]}`
	s := NewScrubber("")
	out := s.Scrub([]any{doc(joined), doc(left)})
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, gone := range []string{"acct-77", "Jo\"", "76561198012345678", "9f1c"} {
		if strings.Contains(got, gone) {
			t.Errorf("%s is still in %s", gone, got)
		}
	}
	if strings.Count(got, `"playerId":"00000000-0000-4000-8001-000000000001"`) != 2 {
		t.Errorf("the player's id has no stand-in, or two: %s", got)
	}
	for _, kept := range []string{`"eventId":"E1"`, `"matchId":"M1"`, `"teamId":"T1"`} {
		if !strings.Contains(got, kept) {
			t.Errorf("%s was changed: %s", kept, got)
		}
	}
	if c := s.Counts(); c.PlayerIDs != 1 || c.SteamIDs != 1 || c.Names != 1 || c.ServerIDs != 1 {
		t.Errorf("counts = %+v", c)
	}
	// An id with no SteamID beside it is no player's.
	lone := NewScrubber("")
	if b, _ := json.Marshal(lone.Scrub([]any{doc(`{"playerId":"acct-77"}`)})); !strings.Contains(string(b), "acct-77") {
		t.Errorf("an id with no SteamID beside it was replaced: %s", b)
	}
	// Stable: the scrubbed batches scrub to themselves.
	scrubbed, ok := doc(got).([]any)
	if !ok {
		t.Fatalf("not a list: %s", got)
	}
	again := NewScrubber("")
	b2, _ := json.Marshal(again.Scrub(scrubbed))
	if c := again.Counts(); c.PlayerIDs+c.SteamIDs+c.Names+c.ServerIDs != 0 || string(b2) != got {
		t.Errorf("re-scrubbed %+v: %s", c, b2)
	}
}
