package matchzy_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gravel-project/gravel/games/cs2/matchzy"
)

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func parseFile(t *testing.T, name string) (matchzy.Event, any) {
	t.Helper()
	env, v, err := matchzy.Parse(readFile(t, filepath.Join("testdata", "events", name)))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return env, v
}

func TestParseEvents(t *testing.T) {
	ev := func(name string) matchzy.Event { return matchzy.Event{Name: name, MatchID: 27} }
	tests := []struct {
		file string
		want any
	}{
		{"series_start.json", matchzy.SeriesStart{
			Event: ev(matchzy.EventSeriesStart),
			Team1: matchzy.Team{Name: "Hidden Token"}, Team2: matchzy.Team{Name: "War Dogs"}, NumMaps: 3,
		}},
		{"going_live.json", matchzy.GoingLive{Event: ev(matchzy.EventGoingLive), MapNumber: 0}},
		{"map_result.json", matchzy.MapResult{
			Event: ev(matchzy.EventMapResult), MapNumber: 0,
			Winner: matchzy.Winner{Side: matchzy.SideCT, Team: matchzy.Team1},
			Team1:  matchzy.StatsTeam{Name: "Hidden Token", SeriesScore: 1, Score: 13, Players: []matchzy.StatsPlayer{}},
			Team2:  matchzy.StatsTeam{Name: "War Dogs", Score: 9, Players: []matchzy.StatsPlayer{}},
		}},
		{"series_end.json", matchzy.SeriesEnd{
			Event:  ev(matchzy.EventSeriesEnd),
			Winner: matchzy.Winner{Side: matchzy.SideT, Team: matchzy.Team1}, Team1SeriesScore: 2, Team2SeriesScore: 1,
			TimeUntilRestore: 10,
		}},
		{"series_end_draw.json", matchzy.SeriesEnd{
			Event:  ev(matchzy.EventSeriesEnd),
			Winner: matchzy.Winner{Side: matchzy.SideNone, Team: matchzy.TeamNone}, Team1SeriesScore: 1, Team2SeriesScore: 1,
			TimeUntilRestore: 10,
		}},
		{"demo_upload_ended.json", matchzy.DemoUploadEnded{
			Event: ev(matchzy.EventDemoUploadEnded), MapNumber: 1,
			Filename: "2026-10-12_20-14-03_27_de_mirage_Hidden Token_vs_War Dogs.dem", Success: true,
		}},
		{"map_picked.json", matchzy.MapPicked{Event: ev(matchzy.EventMapPicked), Team: matchzy.Team2, MapName: "de_mirage", MapNumber: 1}},
		{"map_vetoed.json", matchzy.MapVetoed{Event: ev(matchzy.EventMapVetoed), Team: matchzy.Team1, MapName: "de_nuke"}},
		{"side_picked.json", matchzy.SidePicked{
			Event: ev(matchzy.EventSidePicked), Team: matchzy.Team1, MapName: "de_mirage", MapNumber: 1, Side: "ct",
		}},
		{"player_disconnect.json", matchzy.PlayerDisconnect{
			Event:  ev(matchzy.EventPlayerDisconnect),
			Player: matchzy.Player{SteamID: "76561198000000002", Name: "it's two", UserID: 4},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			env, got := parseFile(t, tt.file)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %#v\nwant %#v", got, tt.want)
			}
			if want := reflect.ValueOf(tt.want).FieldByName("Event").Interface(); env != want {
				t.Errorf("envelope %+v, want %+v", env, want)
			}
		})
	}
}

func TestParseRoundEnd(t *testing.T) {
	env, v := parseFile(t, "round_end.json")
	re, ok := v.(matchzy.RoundEnd)
	if !ok {
		t.Fatalf("%T, want RoundEnd", v)
	}
	if env != (matchzy.Event{Name: matchzy.EventRoundEnd, MatchID: 27}) || re.Event != env {
		t.Errorf("envelope %+v, event's %+v", env, re.Event)
	}
	if re.MapNumber != 0 || re.RoundNumber != 4 || re.RoundTime != 63412 || re.Reason != 8 {
		t.Errorf("round: %+v", re)
	}
	if re.Winner != (matchzy.Winner{Side: matchzy.SideCT, Team: matchzy.Team1}) {
		t.Errorf("winner %+v", re.Winner)
	}
	if re.Team1.Name != "Hidden Token" || re.Team1.Score != 3 || len(re.Team1.Players) != 2 {
		t.Errorf("team1 %+v", re.Team1)
	}
	if re.Team2.Name != "War Dogs" || re.Team2.Score != 2 || len(re.Team2.Players) != 1 {
		t.Errorf("team2 %+v", re.Team2)
	}
	// System.Text.Json escapes non-ASCII and the apostrophe; the names come back as sent.
	if re.Team1.Players[0].Name != "Jörg" || re.Team1.Players[1].Name != "it's two" {
		t.Errorf("names %q %q", re.Team1.Players[0].Name, re.Team1.Players[1].Name)
	}
	want := matchzy.StatsPlayer{SteamID: "76561198000000001", Name: "Jörg", Stats: matchzy.PlayerStats{
		Kills: 5, Deaths: 2, Assists: 1, Damage: 612, UtilityDamage: 48, EnemiesFlashed: 3, HeadshotKills: 3,
		RoundsPlayed: 5, BombDefuses: 1, Kills2: 1, OneV1: 1, Score: 14, MVPs: 2,
	}}
	if got := re.Team1.Players[0]; got != want {
		t.Errorf("player\ngot  %+v\nwant %+v", got, want)
	}
	if got := re.Team2.Players[0].Stats; got.BombPlants != 2 || got.Kills3 != 1 || got.MVPs != 1 {
		t.Errorf("team2 player %+v", got)
	}
}

// Every PlayerStats field must have a JSON name MatchZy sends, so none is silently never read.
func TestPlayerStatsFieldsAllRead(t *testing.T) {
	var raw struct {
		Team1 struct {
			Players []struct {
				Stats map[string]int `json:"stats"`
			} `json:"players"`
		} `json:"team1"`
	}
	if err := json.Unmarshal(readFile(t, "testdata/events/round_end.json"), &raw); err != nil {
		t.Fatal(err)
	}
	sent := raw.Team1.Players[0].Stats
	typ := reflect.TypeFor[matchzy.PlayerStats]()
	if typ.NumField() != len(sent) {
		t.Errorf("PlayerStats has %d fields, the fixture %d", typ.NumField(), len(sent))
	}
	for i := range typ.NumField() {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if _, ok := sent[name]; !ok {
			t.Errorf("field %s (%q) is not in the fixture", typ.Field(i).Name, name)
		}
	}
}

func TestParseUnknown(t *testing.T) {
	body := readFile(t, "testdata/events/player_death.json")
	env, v, err := matchzy.Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	u, ok := v.(matchzy.Unknown)
	if !ok {
		t.Fatalf("%T, want Unknown", v)
	}
	if env != (matchzy.Event{Name: "player_death", MatchID: 27}) || u.Event != env {
		t.Errorf("envelope %+v, unknown's %+v", env, u.Event)
	}
	if string(u.Body) != string(body) {
		t.Errorf("body not kept as received")
	}
	body[0] = ' '
	if u.Body[0] == ' ' {
		t.Error("Body shares the caller's buffer")
	}

	// An event with no matchid at all is still read.
	env, v, err = matchzy.Parse([]byte(`{"event":"something_new","extra":[1,2]}`))
	if err != nil || env.Name != "something_new" || env.MatchID != 0 {
		t.Errorf("no matchid: %+v %v", env, err)
	}
	if _, ok := v.(matchzy.Unknown); !ok {
		t.Errorf("%T, want Unknown", v)
	}
}

func TestParseIgnoresUnknownFields(t *testing.T) {
	_, v, err := matchzy.Parse([]byte(`{"event":"going_live","matchid":-1,"map_number":2,"added_later":{"x":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := (matchzy.GoingLive{Event: matchzy.Event{Name: "going_live", MatchID: -1}, MapNumber: 2}); v != want {
		t.Errorf("got %+v", v)
	}
}

func TestParseErrors(t *testing.T) {
	for name, body := range map[string]string{
		"not json":        `{"event":`,
		"empty":           ``,
		"array":           `[{"event":"going_live"}]`,
		"string":          `"going_live"`,
		"name not string": `{"event":5,"matchid":1}`,
		"matchid string":  `{"event":"going_live","matchid":"27"}`,
		"field type":      `{"event":"going_live","matchid":1,"map_number":"one"}`,
		"winner type":     `{"event":"series_end","matchid":1,"winner":"team1"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, v, err := matchzy.Parse([]byte(body)); err == nil {
				t.Errorf("parsed to %#v", v)
			}
		})
	}
	for name, body := range map[string]string{
		"no name":    `{"matchid":1,"map_number":2}`,
		"empty name": `{"event":"","matchid":1}`,
		"null":       `null`,
		"object":     `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := matchzy.Parse([]byte(body)); !errors.Is(err, matchzy.ErrNoEventName) {
				t.Errorf("err %v, want ErrNoEventName", err)
			}
		})
	}
}

// A parsed event written back with encoding/json parses to the same value, so a fake server can
// send the package's own types.
func TestEventRoundTrip(t *testing.T) {
	files, err := filepath.Glob("testdata/events/*.json")
	if err != nil || len(files) == 0 {
		t.Fatal(files, err)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			_, v := parseFile(t, filepath.Base(f))
			if _, ok := v.(matchzy.Unknown); ok {
				return
			}
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			_, again, err := matchzy.Parse(b)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(v, again) {
				t.Errorf("round trip\nfirst  %#v\nsecond %#v", v, again)
			}
		})
	}
}

// Go canonicalises header names ("Matchzy-Matchid"), so the upload headers are read with
// Header.Get, which canonicalises the constant too, never by indexing the map.
func TestUploadHeaders(t *testing.T) {
	h := http.Header{}
	h.Set(matchzy.HeaderFileName, "27_0_de_mirage.dem")
	h.Set(matchzy.HeaderMatchID, "27")
	h.Set(matchzy.HeaderMapNumber, "0")
	h.Set(matchzy.HeaderRoundNumber, "22")
	for k, want := range map[string]string{
		"MatchZy-FileName": "27_0_de_mirage.dem", "MatchZy-MatchId": "27", "MatchZy-MapNumber": "0", "MatchZy-RoundNumber": "22",
	} {
		if got := h.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}
