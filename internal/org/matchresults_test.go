package org

import (
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

const resultsChannel = "1556656220243239080"

func htgMatchResults() MatchResults {
	return MatchResults{Channel: resultsChannel, Servers: []string{"htg-cs2-1", "htg-cs2-2"}}
}

func TestSettingsValidateMatchResults(t *testing.T) {
	good := Settings{Version: 1, Discord: Discord{GuildID: guild, MatchResults: htgMatchResults()}}
	if err := good.Validate(); err != nil {
		t.Fatalf("good match results: %v", err)
	}
	every := Settings{Version: 1, Discord: Discord{GuildID: guild, MatchResults: MatchResults{Channel: resultsChannel}}}
	if err := every.Validate(); err != nil {
		t.Fatalf("a channel alone, every server: %v", err)
	}
	if (Discord{MatchResults: MatchResults{Channel: resultsChannel}}).IsZero() {
		t.Error("match results are a setting")
	}
	with := func(f func(*MatchResults)) Discord {
		r := htgMatchResults()
		f(&r)
		return Discord{GuildID: guild, MatchResults: r}
	}
	cases := []struct {
		name string
		d    Discord
		want []string
	}{
		{"without a guild", Discord{MatchResults: htgMatchResults()}, []string{"discord.guild_id: required when roles are mapped, server cards placed, seeding set, presence placed, recaps set, streams placed, news listed or match results placed"}},
		{"channel", with(func(r *MatchResults) { r.Channel = "#results" }), []string{`discord.match_results.channel: "#results" is not a Discord id`}},
		{"servers without a channel", with(func(r *MatchResults) { r.Channel = "" }), []string{`discord.match_results.channel: "" is not a Discord id`}},
		{"server id", with(func(r *MatchResults) { r.Servers[1] = "HTG CS2" }), []string{`discord.match_results.servers[1]: "HTG CS2" is not a server id`}},
		{"server twice", with(func(r *MatchResults) { r.Servers = append(r.Servers, "htg-cs2-1") }), []string{`discord.match_results.servers[2]: "htg-cs2-1" is already listed (discord.match_results.servers[0])`}},
		{"every problem at once", with(func(r *MatchResults) { r.Channel, r.Servers[0] = "x", "" }), []string{"discord.match_results.channel", "discord.match_results.servers[0]"}},
	}
	for _, c := range cases {
		err := (Settings{Version: 1, Discord: c.d}).Validate()
		if !errors.Is(err, ErrInvalidSettings) {
			t.Errorf("%s: want ErrInvalidSettings, got %v", c.name, err)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: error should contain %q:\n%v", c.name, w, err)
			}
		}
	}
	many := with(func(r *MatchResults) {
		r.Servers = nil
		for i := range MaxMatchResultsServers + 1 {
			r.Servers = append(r.Servers, "s"+strconv.Itoa(i))
		}
	})
	if err := (Settings{Version: 1, Discord: many}).Validate(); err == nil || !strings.Contains(err.Error(), "discord.match_results.servers: 51 servers, at most 50") {
		t.Errorf("too many servers: %v", err)
	}
	n := (Settings{Discord: Discord{MatchResults: MatchResults{Channel: resultsChannel, Servers: []string{}}}}).Normalized()
	if n.Discord.MatchResults.Servers != nil {
		t.Errorf("no servers normalizes to nil: %+v", n.Discord.MatchResults)
	}
	if n := (Settings{Discord: Discord{MatchResults: MatchResults{Servers: []string{}}}}).Normalized(); !n.Discord.IsZero() {
		t.Errorf("nothing placed normalizes away: %+v", n.Discord)
	}
}

func TestSettingsMatchResultsManifest(t *testing.T) {
	s, err := ParseManifest([]byte(`
discord:
  guild_id: "` + guild + `"
  match_results:
    channel: "` + resultsChannel + `"
    servers: [htg-cs2-1, htg-cs2-2]
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.Discord.MatchResults, htgMatchResults()) {
		t.Errorf("manifest: %+v", s.Discord.MatchResults)
	}
	out, err := s.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	again, err := ParseManifest(out)
	if err != nil || !reflect.DeepEqual(again, s) {
		t.Errorf("round trip: %+v %v\n%s", again, err, out)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := ParseSettings(raw)
	if err != nil || !reflect.DeepEqual(stored, s) {
		t.Errorf("json round trip: %+v %v", stored, err)
	}
	if !strings.Contains(string(raw), `"match_results":{"channel":"`+resultsChannel+`","servers":["htg-cs2-1","htg-cs2-2"]}`) {
		t.Errorf("stored as: %s", raw)
	}
	if strings.Contains(string(mustJSON(t, Settings{Version: 1})), "match_results") {
		t.Error("unset match results are left out")
	}
	if _, err := ParseManifest([]byte("discord:\n  match_results:\n    chanel: \"1\"\n")); err == nil || !strings.Contains(err.Error(), "chanel") {
		t.Errorf("unknown keys are refused by name: %v", err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
