package matchzy_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gravel-project/gravel/games/cs2/matchzy"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/")

// listed is a best of three with both rosters and every optional field set, which no real match
// would combine (a veto's fields with skip_veto); it shows how each is written.
func listed() matchzy.Config {
	return matchzy.Config{
		MatchID: 27,
		Team1: matchzy.ConfigTeam{ID: "htg", Name: "Hidden Token", Players: map[string]string{
			"76561198000000001": "Jörg", "76561198000000002": "",
		}},
		Team2: matchzy.ConfigTeam{Name: "War Dogs", Players: map[string]string{
			"76561198000000003": "Player Three", "76561198000000004": "Player Four",
		}},
		NumMaps:           3,
		Maplist:           []string{"de_mirage", "de_inferno", "de_nuke", "de_ancient", "de_anubis", "de_dust2", "de_train"},
		MapSides:          []string{matchzy.MapSideTeam1CT, matchzy.MapSideTeam2CT, matchzy.MapSideKnife},
		SkipVeto:          true,
		VetoFirst:         matchzy.VetoFirstRandom,
		VetoMode:          []string{matchzy.VetoTeam1Ban, matchzy.VetoTeam2Ban, matchzy.VetoTeam1Pick, matchzy.VetoTeam2Pick, matchzy.VetoTeam1Ban, matchzy.VetoTeam2Ban},
		SideType:          matchzy.SideTypeAlwaysKnife,
		PlayersPerTeam:    2,
		MinPlayersToReady: 2,
		PlayAllMaps:       true,
		Wingman:           true,
		Spectators:        map[string]string{"76561198000000005": "Caster"},
		Cvars:             map[string]string{"hostname": "HTG Match #27", "mp_friendlyfire": "1", "matchzy_remote_log_url": "https://hub.example/ingest/matchzy?x=1"},
	}
}

// unlisted is a best of one on one map with no rosters and no optional field.
func unlisted() matchzy.Config {
	return matchzy.Config{
		MatchID: 28,
		Team1:   matchzy.ConfigTeam{Name: "Team A"},
		Team2:   matchzy.ConfigTeam{Name: "Team B"},
		NumMaps: 1,
		Maplist: []string{"de_dust2"},
	}
}

func TestConfigGolden(t *testing.T) {
	for name, c := range map[string]matchzy.Config{"listed.json": listed(), "unlisted.json": unlisted()} {
		t.Run(name, func(t *testing.T) {
			if err := c.Validate(); err != nil {
				t.Fatal(err)
			}
			b, err := json.MarshalIndent(c, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			b = append(b, '\n')
			path := filepath.Join("testdata", "config", name)
			if *update {
				if err := os.WriteFile(path, b, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if want := readFile(t, path); !bytes.Equal(b, want) {
				t.Errorf("%s differs (go test -update rewrites it)\ngot:\n%s", path, b)
			}
		})
	}
}

// What MatchZy requires of the JSON, beyond what the golden files show.
func TestConfigJSONShape(t *testing.T) {
	b, err := json.Marshal(unlisted())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	// MatchZy refuses a team without "players"; an empty object is a team with no list.
	for _, team := range []string{"team1", "team2"} {
		var tm map[string]json.RawMessage
		if err := json.Unmarshal(m[team], &tm); err != nil {
			t.Fatal(err)
		}
		if string(tm["players"]) != "{}" {
			t.Errorf("%s.players = %s, want {}", team, tm["players"])
		}
		if _, ok := tm["id"]; ok {
			t.Errorf("%s.id written when empty", team)
		}
	}
	if string(m["matchid"]) != "28" {
		t.Errorf("matchid = %s, want a number", m["matchid"])
	}
	for _, k := range []string{"skip_veto", "clinch_series", "wingman", "spectators", "cvars", "map_sides", "veto_mode", "veto_first", "side_type", "players_per_team", "min_players_to_ready"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s written while unset, overriding MatchZy's default", k)
		}
	}

	// The zero Config still writes the required fields, as MatchZy's types expect them.
	b, err = json.Marshal(matchzy.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"maplist":[]`) || !strings.Contains(string(b), `"players":{}`) {
		t.Errorf("zero config: %s", b)
	}
}

func TestConfigRoundTrip(t *testing.T) {
	for name, c := range map[string]matchzy.Config{"listed": listed(), "unlisted": unlisted()} {
		t.Run(name, func(t *testing.T) {
			b, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			var got matchzy.Config
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c) {
				t.Errorf("round trip\ngot  %#v\nwant %#v", got, c)
			}
		})
	}
}

// A config in any form MatchZy accepts reads to the same Config: matchid and numbers as strings,
// a roster as an array of SteamID64s (numbers too, without losing digits), booleans as 1/0 or
// strings, sides in any case, cvar values as numbers, and fields MatchZy does not read.
func TestConfigReadsWhatMatchZyReads(t *testing.T) {
	var got matchzy.Config
	if err := json.Unmarshal(readFile(t, "testdata/config/get5_style.json"), &got); err != nil {
		t.Fatal(err)
	}
	want := matchzy.Config{
		MatchID: 41,
		Team1: matchzy.ConfigTeam{ID: "7", Name: "Hidden Token", Players: map[string]string{
			"76561198000000001": "", "76561198000000002": "",
		}},
		Team2: matchzy.ConfigTeam{Name: "War Dogs", Players: map[string]string{
			"76561198000000003": "Player Three", "76561198000000004": "",
		}},
		NumMaps:           3,
		Maplist:           []string{"de_mirage", "de_inferno", "de_nuke", "de_ancient", "de_anubis"},
		VetoFirst:         matchzy.VetoFirstRandom,
		VetoMode:          []string{matchzy.VetoTeam1Ban, matchzy.VetoTeam2Ban, matchzy.VetoTeam1Pick, matchzy.VetoTeam2Pick},
		SideType:          matchzy.SideTypeNeverKnife,
		PlayersPerTeam:    5,
		MinPlayersToReady: 10,
		PlayAllMaps:       true,
		Spectators:        map[string]string{"76561198000000005": ""},
		Cvars:             map[string]string{"hostname": "HTG #41", "mp_friendlyfire": "1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %#v\nwant %#v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("validate: %v", err)
	}
}

func TestConfigUnmarshalErrors(t *testing.T) {
	for name, body := range map[string]string{
		"not json":         `{"matchid":`,
		"matchid word":     `{"matchid":"twenty"}`,
		"matchid object":   `{"matchid":{}}`,
		"num_maps float":   `{"num_maps":1.5}`,
		"bool word":        `{"skip_veto":"yes"}`,
		"bool two":         `{"clinch_series":2}`,
		"players string":   `{"team1":{"name":"A","players":"76561198000000001"}}`,
		"players objects":  `{"team1":{"name":"A","players":[{"id":1}]}}`,
		"cvar value array": `{"cvars":{"hostname":["a"]}}`,
		"maplist object":   `{"maplist":{"0":"de_dust2"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			var c matchzy.Config
			if err := json.Unmarshal([]byte(body), &c); err == nil {
				t.Errorf("read to %#v", c)
			}
		})
	}
}

func TestConfigValidate(t *testing.T) {
	if err := listed().Validate(); err != nil {
		t.Errorf("listed: %v", err)
	}
	if err := unlisted().Validate(); err != nil {
		t.Errorf("unlisted: %v", err)
	}
	tests := []struct {
		name   string
		change func(*matchzy.Config)
		want   string
	}{
		{"negative matchid", func(c *matchzy.Config) { c.MatchID = -1 }, "matchid -1"},
		{"matchid past int32", func(c *matchzy.Config) { c.MatchID = 1 << 31 }, "matchid 2147483648"},
		{"no maps", func(c *matchzy.Config) { c.NumMaps = 0 }, "num_maps 0"},
		{"even maps", func(c *matchzy.Config) { c.NumMaps = 2 }, "num_maps 2"},
		{"empty maplist", func(c *matchzy.Config) { c.Maplist = nil }, "maplist: must have"},
		{"short maplist", func(c *matchzy.Config) { c.NumMaps = 3 }, "maplist: 1 maps, fewer than num_maps 3"},
		{"map name case", func(c *matchzy.Config) { c.Maplist = []string{"de_Dust2"} }, `maplist[0] "de_Dust2"`},
		{"map name path", func(c *matchzy.Config) { c.Maplist = []string{"workshop/123"} }, `maplist[0] "workshop/123"`},
		{"map name command", func(c *matchzy.Config) { c.Maplist = []string{"de_dust2;quit"} }, `maplist[0]`},
		{"short map_sides", func(c *matchzy.Config) {
			c.NumMaps, c.Maplist, c.MapSides = 3, []string{"a", "b", "c"}, []string{matchzy.MapSideKnife}
		}, "map_sides: 1 entries"},
		{"bad map side", func(c *matchzy.Config) { c.MapSides = []string{"ct"} }, `map_sides[0] "ct"`},
		{"bad veto_first", func(c *matchzy.Config) { c.VetoFirst = "Team1" }, `veto_first "Team1"`},
		{"bad veto step", func(c *matchzy.Config) { c.VetoMode = []string{"team3_ban"} }, `veto_mode[0] "team3_ban"`},
		{"bad side_type", func(c *matchzy.Config) { c.SideType = "knife" }, `side_type "knife"`},
		{"negative players_per_team", func(c *matchzy.Config) { c.PlayersPerTeam = -1 }, "players_per_team -1"},
		{"negative min_players_to_ready", func(c *matchzy.Config) { c.MinPlayersToReady = -2 }, "min_players_to_ready -2"},
		{"no team name", func(c *matchzy.Config) { c.Team1.Name = " " }, "team1.name: must not be empty"},
		{"team name MatchZy strips", func(c *matchzy.Config) { c.Team2.Name = `War "Dogs"` }, `team2.name "War \"Dogs\""`},
		{"short steamid", func(c *matchzy.Config) { c.Team1.Players = map[string]string{"7656119800000000": ""} }, `team1.players "7656119800000000": not a SteamID64`},
		{"steamid letters", func(c *matchzy.Config) { c.Team2.Players = map[string]string{"STEAM_0:1:1234567": ""} }, `team2.players "STEAM_0:1:1234567"`},
		{"player on both teams", func(c *matchzy.Config) {
			c.Team1.Players = map[string]string{"76561198000000001": ""}
			c.Team2.Players = map[string]string{"76561198000000001": ""}
		}, `team2.players "76561198000000001": also in team1.players`},
		{"spectator on a team", func(c *matchzy.Config) {
			c.Team1.Players = map[string]string{"76561198000000001": ""}
			c.Spectators = map[string]string{"76561198000000001": ""}
		}, `spectators.players "76561198000000001": also in team1.players`},
		{"player name quote", func(c *matchzy.Config) { c.Team1.Players = map[string]string{"76561198000000001": `a"b`} }, "name has a quote"},
		{"player name newline", func(c *matchzy.Config) { c.Team1.Players = map[string]string{"76561198000000001": "a\nb"} }, "name has a quote"},
		{"cvar name", func(c *matchzy.Config) { c.Cvars = map[string]string{"mp friendlyfire": "1"} }, `cvars "mp friendlyfire": a name`},
		{"cvar blocked", func(c *matchzy.Config) { c.Cvars = map[string]string{"RCON_PASSWORD": "x"} }, `cvars "RCON_PASSWORD": MatchZy never sets it`},
		{"cvar value semicolon", func(c *matchzy.Config) { c.Cvars = map[string]string{"hostname": "a;quit"} }, `cvars "hostname": value has`},
		{"cvar value quote", func(c *matchzy.Config) { c.Cvars = map[string]string{"hostname": `a"b`} }, `cvars "hostname": value has`},
		{"cvar value newline", func(c *matchzy.Config) { c.Cvars = map[string]string{"hostname": "a\r\nb"} }, `cvars "hostname": value has`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := unlisted()
			tt.change(&c)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err %v, want it to contain %q", err, tt.want)
			}
		})
	}

	// Every problem is reported, not only the first.
	err := matchzy.Config{MatchID: -1}.Validate()
	if err == nil {
		t.Fatal("zero config with a bad matchid validated")
	}
	for _, want := range []string{"matchid", "num_maps", "maplist", "team1.name", "team2.name"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q missing from %v", want, err)
		}
	}
}
