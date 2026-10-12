package org

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func htgPresence() Presence {
	return Presence{Channel: card, Note: "Counts members who share their activity on Discord.", Games: []PresenceGame{
		{Game: "wardogs", Label: "War Dogs", Activities: []string{"WARDOGS"}},
		{Game: "cs2", Label: "Counter-Strike 2", Activities: []string{"Counter-Strike 2"}, ApplicationIDs: []string{"1158877933042143272"}},
	}}
}

func TestSettingsValidatePresence(t *testing.T) {
	good := Settings{Version: 1, Discord: Discord{GuildID: guild, Presence: htgPresence()}}
	if err := good.Validate(); err != nil {
		t.Fatalf("good card: %v", err)
	}
	if good.Discord.IsZero() || (Discord{Presence: htgPresence()}).IsZero() {
		t.Error("a presence card is a setting")
	}
	if (Presence{}).IsZero() != true {
		t.Error("an empty card is none")
	}

	with := func(f func(*Presence)) Discord {
		p := htgPresence()
		f(&p)
		return Discord{GuildID: guild, Presence: p}
	}
	cases := []struct {
		name string
		d    Discord
		want []string
	}{
		{"without a guild", Discord{Presence: htgPresence()}, []string{"discord.guild_id: required when roles are mapped, server cards placed, seeding set, presence placed, recaps set, streams placed, news listed or match results placed"}},
		{"channel", with(func(p *Presence) { p.Channel = "#whos-playing" }), []string{`discord.presence.channel: "#whos-playing" is not a Discord id`}},
		{"no channel", with(func(p *Presence) { p.Channel = "" }), []string{`discord.presence.channel: "" is not a Discord id`}},
		{"note", with(func(p *Presence) { p.Note = strings.Repeat("x", MaxPresenceNote+1) }), []string{"discord.presence.note: one line of at most 200 characters"}},
		{"note on two lines", with(func(p *Presence) { p.Note = "a\nb" }), []string{"discord.presence.note"}},
		{"no games", with(func(p *Presence) { p.Games = nil }), []string{"discord.presence.games: at least one game"}},
		{"game key", with(func(p *Presence) { p.Games[0].Game = "War Dogs" }), []string{`discord.presence.games[0].game: "War Dogs" is not a key`}},
		{"duplicate game", with(func(p *Presence) { p.Games[1].Game = "wardogs" }), []string{`discord.presence.games[1].game: "wardogs" is already on the card (discord.presence.games[0])`}},
		{"no label", with(func(p *Presence) { p.Games[0].Label = " " }), []string{"discord.presence.games[0].label: one line of 1 to 40 characters"}},
		{"long label", with(func(p *Presence) { p.Games[0].Label = strings.Repeat("x", MaxPresenceLabel+1) }), []string{"discord.presence.games[0].label"}},
		{"nothing to match", with(func(p *Presence) { p.Games[0].Activities = nil }), []string{"discord.presence.games[0]: names no activity and no application id"}},
		{"empty activity", with(func(p *Presence) { p.Games[0].Activities = []string{""} }), []string{"discord.presence.games[0].activities[0]: one line of 1 to 128 characters"}},
		{"activity on two lines", with(func(p *Presence) { p.Games[0].Activities = []string{"WAR\nDOGS"} }), []string{"discord.presence.games[0].activities[0]"}},
		{"activity in two games, by case", with(func(p *Presence) { p.Games[1].Activities = []string{"wardogs"} }), []string{`discord.presence.games[1].activities[0]: "wardogs" already counts for wardogs`}},
		{"application id", with(func(p *Presence) { p.Games[1].ApplicationIDs = []string{"cs2"} }), []string{`discord.presence.games[1].application_ids[0]: "cs2" is not a Discord id`}},
		{"application id in two games", with(func(p *Presence) { p.Games[0].ApplicationIDs = []string{"1158877933042143272"} }), []string{"discord.presence.games[1].application_ids[0]: 1158877933042143272 already counts for wardogs"}},
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

	many := htgPresence()
	many.Games = nil
	for i := range MaxPresenceGames + 1 {
		many.Games = append(many.Games, PresenceGame{Game: fmt.Sprintf("g%d", i), Label: "G", Activities: []string{fmt.Sprintf("G%d", i)}})
	}
	many.Games[0].Activities = make([]string, MaxPresenceActivities+1)
	many.Games[0].ApplicationIDs = make([]string, MaxPresenceActivities+1)
	for i := range many.Games[0].Activities {
		many.Games[0].Activities[i] = fmt.Sprintf("A%d", i)
		many.Games[0].ApplicationIDs[i] = fmt.Sprintf("13000000000000001%02d", i)
	}
	err := (Settings{Version: 1, Discord: Discord{GuildID: guild, Presence: many}}).Validate()
	for _, w := range []string{"discord.presence.games: 21 games, at most 20", "discord.presence.games[0].activities: 11 names, at most 10", "discord.presence.games[0].application_ids: 11 ids, at most 10"} {
		if err == nil || !strings.Contains(err.Error(), w) {
			t.Errorf("bounds: want %q in %v", w, err)
		}
	}

	n := (Settings{Discord: Discord{GuildID: guild, Presence: Presence{Channel: card, Note: "  padded ", Games: []PresenceGame{{Game: "g", Label: " G ", Activities: []string{" WARDOGS "}, ApplicationIDs: []string{}}}}}}).Normalized()
	if g := n.Discord.Presence.Games[0]; n.Discord.Presence.Note != "padded" || g.Label != "G" || g.Activities[0] != "WARDOGS" || g.ApplicationIDs != nil {
		t.Errorf("normalized: %+v", n.Discord.Presence)
	}
	if n := (Settings{Discord: Discord{Presence: Presence{Games: []PresenceGame{}}}}).Normalized(); n.Discord.Presence.Games != nil || !n.Discord.IsZero() {
		t.Errorf("an empty card normalizes away: %+v", n.Discord)
	}
}

func TestSettingsPresenceManifest(t *testing.T) {
	s, err := ParseManifest([]byte(`
discord:
  guild_id: "` + guild + `"
  presence:
    channel: "` + card + `"
    note: Counts members who share their activity on Discord.
    games:
      - game: wardogs
        label: War Dogs
        activities: [WARDOGS]
      - game: cs2
        label: Counter-Strike 2
        activities: [Counter-Strike 2]
        application_ids: ["1158877933042143272"]
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.Discord.Presence, htgPresence()) {
		t.Errorf("manifest: %+v", s.Discord.Presence)
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
	if strings.Contains(string(raw), "presence") != true {
		t.Errorf("stored: %s", raw)
	}
	none, _ := json.Marshal(Settings{Version: 1})
	if strings.Contains(string(none), "presence") {
		t.Errorf("no card stores no key: %s", none)
	}
	if out, _ := (Settings{Version: 1}).Manifest(); strings.Contains(string(out), "presence") {
		t.Errorf("no card exports no key:\n%s", out)
	}
	if _, err := ParseManifest([]byte("discord:\n  presence:\n    channel: \"1\"\n    games:\n      - game: g\n        names: [G]\n")); err == nil || !strings.Contains(err.Error(), "names") {
		t.Errorf("unknown keys are refused by name: %v", err)
	}
}
