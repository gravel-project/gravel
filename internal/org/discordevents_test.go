package org

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const eventsChannel = "1556656220243239081"

// The events feed (gravel#200): a channel, a lowest severity and kinds or groups the events log
// knows; it round-trips through JSON and needs the guild like every other Discord setting.
func TestSettingsValidateDiscordEvents(t *testing.T) {
	feed := DiscordEvents{Channel: eventsChannel, MinSeverity: "notice", Kinds: []string{"server", "host.failing", "backup"}}
	good := Settings{Version: 1, Discord: Discord{GuildID: guild, Events: feed}}
	if err := good.Validate(); err != nil {
		t.Fatalf("good feed: %v", err)
	}
	if (Discord{Events: DiscordEvents{Channel: eventsChannel}}).IsZero() {
		t.Error("the events feed is a setting")
	}
	if got := (DiscordEvents{}).MinSeverityOrDefault(); got != "warning" {
		t.Errorf("default severity = %q", got)
	}
	b, err := json.Marshal(good)
	if err != nil {
		t.Fatal(err)
	}
	var back Settings
	if err := json.Unmarshal(b, &back); err != nil || back.Discord.Events.MinSeverity != "notice" || len(back.Discord.Events.Kinds) != 3 {
		t.Errorf("round trip: %+v %v", back.Discord.Events, err)
	}
	with := func(f func(*DiscordEvents)) Discord {
		e := feed
		e.Kinds = append([]string(nil), feed.Kinds...)
		f(&e)
		return Discord{GuildID: guild, Events: e}
	}
	many := make([]string, MaxDiscordEventKinds+1)
	for i := range many {
		many[i] = "server"
	}
	cases := []struct {
		name string
		d    Discord
		want []string
	}{
		{"without a guild", Discord{Events: feed}, []string{"discord.guild_id: required when"}},
		{"channel", with(func(e *DiscordEvents) { e.Channel = "#infra-alerts" }), []string{`discord.events.channel: "#infra-alerts" is not a Discord id`}},
		{"a filter without a channel", with(func(e *DiscordEvents) { e.Channel = "" }), []string{`discord.events.channel: "" is not a Discord id`}},
		{"severity", with(func(e *DiscordEvents) { e.MinSeverity = "loud" }), []string{`discord.events.min_severity: "loud" is not info, notice, warning or critical`}},
		{"kind", with(func(e *DiscordEvents) { e.Kinds[1] = "server.exploded" }), []string{`discord.events.kinds[1]: "server.exploded" is neither an event kind nor a group of them`}},
		{"too many kinds", with(func(e *DiscordEvents) { e.Kinds = many }), []string{"discord.events.kinds: 21 kinds, at most 20"}},
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
	// A message about a field names its path, for the admin dashboard (gravel#205).
	fe := FieldErrors((Settings{Version: 1, Discord: with(func(e *DiscordEvents) { e.Kinds[0] = "nope" })}).Validate())
	if len(fe) != 1 || fe[0].Path != "discord.events.kinds[0]" {
		t.Errorf("field errors = %+v", fe)
	}
}
