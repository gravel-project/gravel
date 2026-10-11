package org

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func htgRecap() RecapRule {
	return RecapRule{Server: "htg-wardogs-1", Channel: card, Title: "Wardogs Wednesday",
		Hours: []SeedingHours{{Days: []string{"wed"}, From: "18:00", To: "22:00"}}, Floor: 20, Note: "See you next week."}
}

func TestSettingsValidateRecaps(t *testing.T) {
	good := Settings{Version: 1, Stats: Stats{Timezone: "America/Chicago"}, Discord: Discord{GuildID: guild, Recaps: []RecapRule{
		htgRecap(),
		{Server: "htg-wardogs-2", Channel: card, Title: "Night"}, // no hours: never posts; no floor
	}}}
	if err := good.Validate(); err != nil {
		t.Fatalf("good rules: %v", err)
	}
	if (Discord{Recaps: []RecapRule{htgRecap()}}).IsZero() {
		t.Error("a recap rule is a setting")
	}
	rule := func(f func(*RecapRule)) Discord {
		r := htgRecap()
		f(&r)
		return Discord{GuildID: guild, Recaps: []RecapRule{r}}
	}
	cases := []struct {
		name string
		d    Discord
		want []string
	}{
		{"without a guild", Discord{Recaps: []RecapRule{htgRecap()}}, []string{"discord.guild_id: required when roles are mapped, server cards placed, seeding set, presence placed or recaps set"}},
		{"server", rule(func(r *RecapRule) { r.Server = "HTG 1" }), []string{`discord.recaps[0].server: "HTG 1" is not a server id`}},
		{"channel", rule(func(r *RecapRule) { r.Channel = "#wd-chat" }), []string{`discord.recaps[0].channel: "#wd-chat" is not a Discord id`}},
		{"no title", rule(func(r *RecapRule) { r.Title = " " }), []string{"discord.recaps[0].title: one line of 1 to 60 characters"}},
		{"long title", rule(func(r *RecapRule) { r.Title = strings.Repeat("x", MaxRecapTitle+1) }), []string{"discord.recaps[0].title"}},
		{"title on two lines", rule(func(r *RecapRule) { r.Title = "a\nb" }), []string{"discord.recaps[0].title"}},
		{"negative floor", rule(func(r *RecapRule) { r.Floor = -1 }), []string{"discord.recaps[0].floor: -1 is not between 0 and 1000"}},
		{"floor too high", rule(func(r *RecapRule) { r.Floor = 1001 }), []string{"discord.recaps[0].floor: 1001"}},
		{"note", rule(func(r *RecapRule) { r.Note = strings.Repeat("x", MaxRecapNote+1) }), []string{"discord.recaps[0].note: one line of at most 200 characters"}},
		{"hours", rule(func(r *RecapRule) {
			r.Hours = []SeedingHours{{Days: []string{"wednesday"}, From: "18:00", To: "22:00"}}
		}), []string{`discord.recaps[0].hours[0].days: "wednesday" is not a day`}},
		{"duplicate server", Discord{GuildID: guild, Recaps: []RecapRule{htgRecap(), htgRecap()}}, []string{`discord.recaps[1].server: "htg-wardogs-1" already has a rule (discord.recaps[0])`}},
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
	many := Settings{Version: 1, Discord: Discord{GuildID: guild}}
	for i := range MaxRecapRules + 1 {
		r := htgRecap()
		r.Server = fmt.Sprintf("s%d", i)
		many.Discord.Recaps = append(many.Discord.Recaps, r)
	}
	if err := many.Validate(); err == nil || !strings.Contains(err.Error(), "discord.recaps: 11 rules, at most 10") {
		t.Errorf("too many rules: %v", err)
	}
	n := (Settings{Discord: Discord{GuildID: guild, Recaps: []RecapRule{{Server: "s", Title: " T ", Note: "  padded  ", Hours: []SeedingHours{}}}}}).Normalized()
	if r := n.Discord.Recaps[0]; r.Title != "T" || r.Note != "padded" || r.Hours != nil {
		t.Errorf("normalized: %+v", r)
	}
	if n := (Settings{Discord: Discord{Recaps: []RecapRule{}}}).Normalized(); n.Discord.Recaps != nil || !n.Discord.IsZero() {
		t.Errorf("no rules normalizes away: %+v", n.Discord)
	}
}

func TestSettingsRecapsManifest(t *testing.T) {
	s, err := ParseManifest([]byte(`
stats:
  timezone: America/Chicago
discord:
  guild_id: "` + guild + `"
  recaps:
    - server: htg-wardogs-1
      channel: "` + card + `"
      title: Wardogs Wednesday
      hours:
        - {days: [wed], from: "18:00", to: "22:00"}
      floor: 20
      note: See you next week.
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.Discord.Recaps, []RecapRule{htgRecap()}) {
		t.Errorf("manifest: %+v", s.Discord.Recaps)
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
	if _, err := ParseManifest([]byte("discord:\n  recaps:\n    - server: s\n      window: wed\n")); err == nil || !strings.Contains(err.Error(), "window") {
		t.Errorf("unknown keys are refused by name: %v", err)
	}
}
