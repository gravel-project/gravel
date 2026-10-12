package org

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const newsChannel = "1556656220243239072" // HTG's #cs-news

func htgNews() News {
	return News{Feeds: []NewsFeed{
		{AppID: 730, Channel: newsChannel, Label: "Counter-Strike 2", Color: 0xde9b35},
		{AppID: 271590, Channel: card, Label: "Grand Theft Auto V", Feeds: []string{NewsDefaultFeed, "steam_updates"}},
	}}
}

func TestSettingsValidateNews(t *testing.T) {
	good := Settings{Version: 1, Discord: Discord{GuildID: guild, News: htgNews()}}
	if err := good.Validate(); err != nil {
		t.Fatalf("good news: %v", err)
	}
	if (Discord{News: htgNews()}).IsZero() {
		t.Error("news is a setting")
	}
	with := func(f func(*News)) Discord {
		n := htgNews()
		f(&n)
		return Discord{GuildID: guild, News: n}
	}
	cases := []struct {
		name string
		d    Discord
		want []string
	}{
		{"without a guild", Discord{News: htgNews()}, []string{"discord.guild_id: required when roles are mapped, server cards placed, seeding set, presence placed, recaps set, streams placed or news listed"}},
		{"app id", with(func(n *News) { n.Feeds[0].AppID = 0 }), []string{"discord.news.feeds[0].app_id: 0 is not a Steam app id"}},
		{"negative app id", with(func(n *News) { n.Feeds[0].AppID = -730 }), []string{"discord.news.feeds[0].app_id: -730 is not a Steam app id"}},
		{"huge app id", with(func(n *News) { n.Feeds[0].AppID = 1 << 40 }), []string{"discord.news.feeds[0].app_id"}},
		{"channel", with(func(n *News) { n.Feeds[0].Channel = "#cs-news" }), []string{`discord.news.feeds[0].channel: "#cs-news" is not a Discord id`}},
		{"no label", with(func(n *News) { n.Feeds[0].Label = "  " }), []string{"discord.news.feeds[0].label: required"}},
		{"long label", with(func(n *News) { n.Feeds[0].Label = strings.Repeat("é", MaxNewsLabel+1) }), []string{"discord.news.feeds[0].label: one line of at most 64 characters"}},
		{"label on two lines", with(func(n *News) { n.Feeds[0].Label = "Counter\nStrike" }), []string{"discord.news.feeds[0].label: one line"}},
		{"feed name", with(func(n *News) { n.Feeds[1].Feeds = []string{"steam community"} }), []string{`discord.news.feeds[1].feeds[0]: "steam community" is not a Steam feed name`}},
		{"feed twice", with(func(n *News) { n.Feeds[1].Feeds = []string{"steam_updates", "steam_updates"} }), []string{`discord.news.feeds[1].feeds[1]: "steam_updates" is listed twice`}},
		{"color", with(func(n *News) { n.Feeds[0].Color = 0x1000000 }), []string{"discord.news.feeds[0].color: 0x1000000 is not a color"}},
		{"negative color", with(func(n *News) { n.Feeds[0].Color = -1 }), []string{"discord.news.feeds[0].color"}},
		{"a game twice in a channel", with(func(n *News) { n.Feeds = append(n.Feeds, n.Feeds[0]) }), []string{"discord.news.feeds[2]: app 730 already posts to channel " + newsChannel + " (discord.news.feeds[0])"}},
		{"every problem at once", with(func(n *News) { n.Feeds[0].AppID, n.Feeds[1].Label = 0, "" }), []string{"discord.news.feeds[0].app_id", "discord.news.feeds[1].label"}},
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
	// One game in two channels, and two games in one, are fine.
	spread := with(func(n *News) {
		n.Feeds = []NewsFeed{{AppID: 730, Channel: newsChannel, Label: "CS2"}, {AppID: 730, Channel: card, Label: "CS2"}, {AppID: 570, Channel: card, Label: "Dota 2"}}
	})
	if err := (Settings{Version: 1, Discord: spread}).Validate(); err != nil {
		t.Errorf("spread: %v", err)
	}
	many := with(func(n *News) {
		n.Feeds = nil
		for i := range MaxNewsFeeds + 1 {
			n.Feeds = append(n.Feeds, NewsFeed{AppID: i + 1, Channel: newsChannel, Label: "g"})
		}
	})
	if err := (Settings{Version: 1, Discord: many}).Validate(); err == nil || !strings.Contains(err.Error(), "discord.news.feeds: 21 feeds, at most 20") {
		t.Errorf("too many feeds: %v", err)
	}
	n := (Settings{Discord: Discord{News: News{Feeds: []NewsFeed{{AppID: 730, Channel: newsChannel, Label: " CS2 ", Feeds: []string{}}}}}}).Normalized()
	if f := n.Discord.News.Feeds[0]; f.Label != "CS2" || f.Feeds != nil {
		t.Errorf("normalized: %+v", f)
	}
	if n := (Settings{Discord: Discord{News: News{Feeds: []NewsFeed{}}}}).Normalized(); n.Discord.News.Feeds != nil || !n.Discord.IsZero() {
		t.Errorf("no feeds normalizes away: %+v", n.Discord)
	}
}

func TestSettingsNewsManifest(t *testing.T) {
	s, err := ParseManifest([]byte(`
discord:
  guild_id: "` + guild + `"
  news:
    feeds:
      - app_id: 730
        channel: "` + newsChannel + `"
        label: Counter-Strike 2
        color: 0xde9b35
      - app_id: 271590
        channel: "` + card + `"
        label: Grand Theft Auto V
        feeds: [steam_community_announcements, steam_updates]
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.Discord.News, htgNews()) {
		t.Errorf("manifest: %+v", s.Discord.News)
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
	if !strings.Contains(string(raw), `"news":{"feeds":[{"app_id":730,"channel":"`+newsChannel+`","label":"Counter-Strike 2","color":14588725}`) {
		t.Errorf("stored as: %s", raw)
	}
	if _, err := ParseManifest([]byte("discord:\n  news:\n    feeds:\n      - {app_id: 730, chanel: \"1\"}\n")); err == nil || !strings.Contains(err.Error(), "chanel") {
		t.Errorf("unknown keys are refused by name: %v", err)
	}
}
