package org

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const streamRole = "1300000000000000005" // the opt-in stream role

func htgStreams() Streams {
	return Streams{Channel: card, Role: streamRole, Note: "Grab the Stream role in #roles to be pinged.", Streamers: []Streamer{
		{Platform: StreamTwitch, Login: "hiddentoken", Label: "Hidden Token"},
		{Platform: StreamKick, Login: "hidden-token"},
	}}
}

func TestSettingsValidateStreams(t *testing.T) {
	good := Settings{Version: 1, Discord: Discord{GuildID: guild, Streams: htgStreams()}}
	if err := good.Validate(); err != nil {
		t.Fatalf("good streams: %v", err)
	}
	noRole := htgStreams()
	noRole.Role = ""
	if err := (Settings{Version: 1, Discord: Discord{GuildID: guild, Streams: noRole}}).Validate(); err != nil {
		t.Errorf("no role pings no one: %v", err)
	}
	if (Discord{Streams: htgStreams()}).IsZero() {
		t.Error("streams are a setting")
	}
	with := func(f func(*Streams)) Discord {
		s := htgStreams()
		f(&s)
		return Discord{GuildID: guild, Streams: s}
	}
	cases := []struct {
		name string
		d    Discord
		want []string
	}{
		{"without a guild", Discord{Streams: htgStreams()}, []string{"discord.guild_id: required when roles are mapped, server cards placed, seeding set, presence placed, recaps set, streams placed, news listed, match results or the events feed placed"}},
		{"channel", with(func(s *Streams) { s.Channel = "#live" }), []string{`discord.streams.channel: "#live" is not a Discord id`}},
		{"role", with(func(s *Streams) { s.Role = "@Stream" }), []string{`discord.streams.role: "@Stream" is not a Discord id`}},
		{"everyone", with(func(s *Streams) { s.Role = guild }), []string{"discord.streams.role: \"" + guild + "\" is the guild's @everyone role"}},
		{"note", with(func(s *Streams) { s.Note = strings.Repeat("x", MaxStreamsNote+1) }), []string{"discord.streams.note: one line of at most 200 characters"}},
		{"note on two lines", with(func(s *Streams) { s.Note = "a\nb" }), []string{"discord.streams.note"}},
		{"no streamers", with(func(s *Streams) { s.Streamers = nil }), []string{"discord.streams.streamers: at least one streamer"}},
		{"platform", with(func(s *Streams) { s.Streamers[0].Platform = "youtube" }), []string{`discord.streams.streamers[0].platform: "youtube" is not a platform (twitch, kick)`}},
		{"upper case", with(func(s *Streams) { s.Streamers[0].Login = "HiddenToken" }), []string{`discord.streams.streamers[0].login: "HiddenToken" is not a twitch channel name`}},
		{"a URL", with(func(s *Streams) { s.Streamers[0].Login = "twitch.tv/hiddentoken" }), []string{`discord.streams.streamers[0].login`}},
		{"twitch takes no hyphen", with(func(s *Streams) { s.Streamers[0].Login = "hidden-token" }), []string{`discord.streams.streamers[0].login: "hidden-token" is not a twitch channel name`}},
		{"too short", with(func(s *Streams) { s.Streamers[1].Login = "ht" }), []string{`discord.streams.streamers[1].login: "ht" is not a kick channel name`}},
		{"label", with(func(s *Streams) { s.Streamers[0].Label = strings.Repeat("x", MaxStreamerLabel+1) }), []string{"discord.streams.streamers[0].label: one line of at most 40 characters"}},
		{"duplicate", with(func(s *Streams) { s.Streamers = append(s.Streamers, s.Streamers[0]) }), []string{`discord.streams.streamers[2]: twitch "hiddentoken" is already listed (discord.streams.streamers[0])`}},
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
	// The same login on both platforms is two channels.
	both := with(func(s *Streams) {
		s.Streamers = []Streamer{{Platform: StreamTwitch, Login: "hiddentoken"}, {Platform: StreamKick, Login: "hiddentoken"}}
	})
	if err := (Settings{Version: 1, Discord: both}).Validate(); err != nil {
		t.Errorf("one login on two platforms: %v", err)
	}
	many := with(func(s *Streams) {
		s.Streamers = nil
		for i := range MaxStreamers + 1 {
			s.Streamers = append(s.Streamers, Streamer{Platform: StreamTwitch, Login: fmt.Sprintf("streamer%d", i)})
		}
	})
	if err := (Settings{Version: 1, Discord: many}).Validate(); err == nil || !strings.Contains(err.Error(), "discord.streams.streamers: 11 streamers, at most 10") {
		t.Errorf("too many streamers: %v", err)
	}
	n := (Settings{Discord: Discord{Streams: Streams{Channel: card, Note: "  padded ", Streamers: []Streamer{{Platform: StreamKick, Login: "abc", Label: " L "}}}}}).Normalized()
	if s := n.Discord.Streams; s.Note != "padded" || s.Streamers[0].Label != "L" {
		t.Errorf("normalized: %+v", s)
	}
	if n := (Settings{Discord: Discord{Streams: Streams{Streamers: []Streamer{}}}}).Normalized(); n.Discord.Streams.Streamers != nil || !n.Discord.IsZero() {
		t.Errorf("no streamers normalizes away: %+v", n.Discord)
	}
}

func TestSettingsStreamsManifest(t *testing.T) {
	s, err := ParseManifest([]byte(`
discord:
  guild_id: "` + guild + `"
  streams:
    channel: "` + card + `"
    role: "` + streamRole + `"
    note: "Grab the Stream role in #roles to be pinged."
    streamers:
      - {platform: twitch, login: hiddentoken, label: Hidden Token}
      - {platform: kick, login: hidden-token}
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.Discord.Streams, htgStreams()) {
		t.Errorf("manifest: %+v", s.Discord.Streams)
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
	if _, err := ParseManifest([]byte("discord:\n  streams:\n    channels: []\n")); err == nil || !strings.Contains(err.Error(), "channels") {
		t.Errorf("unknown keys are refused by name: %v", err)
	}
}
