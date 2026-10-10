package org

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gravel-project/gravel/internal/org/orgtest"
)

func TestSettingsValidate(t *testing.T) {
	good := Settings{Version: 1, Theme: Theme{Light: Tokens{Accent: "#0a7a3a", Background: "#f2f5f9"}, Dark: Tokens{Accent: "rgb(62, 224, 122)"}, Font: "Archivo, system-ui, sans-serif", LogoURL: "https://hiddentoken.com/brand/mark.svg", FaviconURL: "/static/favicon.svg"},
		Nav: []NavLink{{Label: "Discord", URL: "https://discord.gg/x"}, {Label: "Rules", URL: "https://hiddentoken.com/rules/", Placement: "footer"}, {Label: "Admin", URL: "/admin", Role: "owner"}}}
	if err := good.Validate(); err != nil {
		t.Fatalf("good settings: %v", err)
	}
	bad := Settings{Version: 2,
		Theme: Theme{Light: Tokens{Accent: "red; }"}, Dark: Tokens{Background: "url(x)"}, Font: "Archivo; background: url(x)", LogoURL: "http://insecure.example/logo.png", FaviconURL: "//evil.example/f.ico"},
		Nav:   []NavLink{{Label: "", URL: "javascript:alert(1)"}, {Label: strings.Repeat("x", 41), URL: "https://ok.example", Placement: "sidebar", Role: "admin"}}}
	err := bad.Validate()
	if err == nil {
		t.Fatal("bad settings must fail")
	}
	if !errors.Is(err, ErrInvalidSettings) {
		t.Errorf("every failure wraps ErrInvalidSettings: %v", err)
	}
	for _, want := range []string{"version:", "theme.light.accent", "theme.dark.background", "theme.font", "theme.logo_url", "theme.favicon_url", "nav[0].label", "nav[0].url", "nav[1].label", "nav[1].placement", "nav[1].role"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %s:\n%v", want, err)
		}
	}
	many := Settings{Version: 1}
	for range MaxNavLinks + 1 {
		many.Nav = append(many.Nav, NavLink{Label: "x", URL: "/x"})
	}
	if err := many.Validate(); err == nil || !strings.Contains(err.Error(), "nav: 13 links") {
		t.Errorf("too many links: %v", err)
	}
}

func TestSettingsParseAndManifest(t *testing.T) {
	if s, err := ParseSettings(nil); err != nil || s.Version != SettingsVersion || len(s.Nav) != 0 {
		t.Errorf("empty: %+v %v", s, err)
	}
	if s, err := ParseSettings([]byte("{}")); err != nil || s.Version != SettingsVersion {
		t.Errorf("braces: %+v %v", s, err)
	}
	if _, err := ParseSettings([]byte("{nope")); err == nil {
		t.Error("corrupt json must fail")
	}
	manifest := []byte(`
theme:
  dark:
    accent: "#3ee07a"
    background: "#070b17"
  light:
    accent: "#0a7a3a"
  font: Archivo, system-ui, sans-serif
  logo_url: https://hiddentoken.com/brand/mark.svg
nav:
  - label: Discord
    url: https://discord.gg/x
  - label: Rules
    url: https://hiddentoken.com/rules/
    placement: footer
`)
	s, err := ParseManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if s.Version != 1 || s.Theme.Dark.Accent != "#3ee07a" || s.Nav[0].Placement != PlacementHeader || s.Nav[1].Placement != PlacementFooter {
		t.Errorf("manifest: %+v", s)
	}
	if _, err := ParseManifest([]byte("theme:\n  colour: red\n")); err == nil || !strings.Contains(err.Error(), "colour") {
		t.Errorf("unknown keys are refused by name: %v", err)
	}
	out, err := s.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	again, err := ParseManifest(out)
	if err != nil || again.Theme.Dark.Accent != "#3ee07a" || len(again.Nav) != 2 || again.Nav[1].Placement != "footer" {
		t.Errorf("round trip: %+v %v\n%s", again, err, out)
	}
	if strings.Contains(string(out), "favicon_url") || !strings.Contains(string(out), "version: 1") {
		t.Errorf("empty fields are omitted and the version is written:\n%s", out)
	}
}

func TestSettingsService(t *testing.T) {
	f := &orgtest.FakeStore{}
	s, now := newTestService(f)
	if _, _, err := s.EnsureBuiltin(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	set, at, err := s.Settings(context.Background())
	if err != nil || set.Version != 1 || !at.IsZero() {
		t.Fatalf("defaults: %+v %v %v", set, at, err)
	}
	want := Settings{Theme: Theme{Dark: Tokens{Accent: "#3ee07a"}}, Nav: []NavLink{{Label: "  Discord ", URL: "https://discord.gg/x"}}}
	got, err := s.UpdateSettings(context.Background(), want)
	if err != nil || got.Version != 1 || got.Nav[0].Label != "Discord" || got.Nav[0].Placement != "header" {
		t.Fatalf("update: %+v %v", got, err)
	}
	set, at, err = s.Settings(context.Background())
	if err != nil || set.Theme.Dark.Accent != "#3ee07a" || !at.Equal(*now) {
		t.Errorf("after update: %+v %v %v", set, at, err)
	}
	if _, err := s.UpdateSettings(context.Background(), Settings{Theme: Theme{Font: "x; y"}}); !errors.Is(err, ErrInvalidSettings) {
		t.Errorf("invalid: %v", err)
	}
	if set, _, _ := s.Settings(context.Background()); set.Theme.Font != "" {
		t.Error("an invalid update must not be stored")
	}
	f.ErrOn = "settings"
	if _, err := s.UpdateSettings(context.Background(), want); err == nil || errors.Is(err, ErrInvalidSettings) {
		t.Errorf("store error propagates as itself: %v", err)
	}
}

const (
	guild    = "519496143298756611"
	linked   = "1300000000000000001"
	steam    = "1300000000000000002"
	founders = "1300000000000000003"
	card     = "1300000000000000100" // a channel
)

func TestSettingsValidateDiscord(t *testing.T) {
	good := Settings{Version: 1, Discord: Discord{GuildID: guild,
		Roles:       DiscordRoles{Linked: linked, Providers: map[string]string{"steam": steam}},
		Recognition: []Recognition{{Role: founders, Rule: RuleFirstMembers, Count: 50}},
		ServerCards: []ServerCard{{Server: "htg-wardogs-1", Channel: card, Note: "Matches start at 20 players."}, {Server: "htg-wardogs-2", Channel: card}}}}
	if err := good.Validate(); err != nil {
		t.Fatalf("good mapping: %v", err)
	}
	if got := good.Discord.RoleIDs(); !slices.Equal(got, []string{linked, steam, founders}) {
		t.Errorf("RoleIDs = %v", got)
	}
	if err := (Settings{Version: 1, Discord: Discord{GuildID: guild}}).Validate(); err != nil {
		t.Errorf("a guild with no roles yet is valid: %v", err)
	}

	cases := []struct {
		name string
		d    Discord
		want []string
	}{
		{"roles without a guild", Discord{Roles: DiscordRoles{Linked: linked}}, []string{"discord.guild_id: required"}},
		{"bad ids", Discord{GuildID: "12345", Roles: DiscordRoles{Linked: "abc", Providers: map[string]string{"steam": "1234567890123456789012"}}},
			[]string{"discord.guild_id: \"12345\"", "discord.roles.linked: \"abc\"", "discord.roles.providers.steam"}},
		{"an id past uint64", Discord{GuildID: "99999999999999999999"}, []string{"discord.guild_id"}},
		{"unknown provider", Discord{GuildID: guild, Roles: DiscordRoles{Providers: map[string]string{"xbox": steam}}}, []string{"discord.roles.providers.xbox: \"xbox\" is not a provider"}},
		{"duplicate role", Discord{GuildID: guild, Roles: DiscordRoles{Linked: linked, Providers: map[string]string{"steam": linked}}}, []string{"discord.roles.providers.steam: role " + linked + " is already mapped by discord.roles.linked"}},
		{"@everyone", Discord{GuildID: guild, Roles: DiscordRoles{Linked: guild}}, []string{"@everyone"}},
		{"bad recognition", Discord{GuildID: guild, Recognition: []Recognition{{Role: founders, Rule: "member_since", Count: 0}, {Role: steam, Rule: RuleFirstMembers, Count: MaxFirstMembers + 1}}},
			[]string{"discord.recognition[0].rule: \"member_since\"", "discord.recognition[0].count: 0", "discord.recognition[1].count"}},
		{"cards without a guild", Discord{ServerCards: []ServerCard{{Server: "s", Channel: card}}}, []string{"discord.guild_id: required"}},
		{"bad cards", Discord{GuildID: guild, ServerCards: []ServerCard{
			{Server: "HTG Wardogs", Channel: "abc", Note: strings.Repeat("x", MaxCardNote+1)},
			{Server: "s", Channel: card, Note: "two\nlines"},
			{Server: "s", Channel: card},
		}}, []string{"discord.server_cards[0].server: \"HTG Wardogs\" is not a server id", "discord.server_cards[0].channel: \"abc\"", "discord.server_cards[0].note",
			"discord.server_cards[1].note", "discord.server_cards[2].server: \"s\" already has a card (discord.server_cards[1])"}},
		{"bad mod log", Discord{GuildID: guild, ModLog: "#mod-log"}, []string{"discord.mod_log: \"#mod-log\" is not a Discord channel id"}},
		{"recognition duplicates a linked role", Discord{GuildID: guild, Roles: DiscordRoles{Linked: linked}, Recognition: []Recognition{{Role: linked, Rule: RuleFirstMembers, Count: 5}}}, []string{"discord.recognition[0].role: role " + linked}},
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
	for i := range MaxRecognition + 1 {
		many.Discord.Recognition = append(many.Discord.Recognition, Recognition{Role: fmt.Sprintf("13000000000000001%02d", i), Rule: RuleFirstMembers, Count: 1})
	}
	if err := many.Validate(); err == nil || !strings.Contains(err.Error(), "discord.recognition: 11 entries") {
		t.Errorf("too many recognition entries: %v", err)
	}
	cards := Settings{Version: 1, Discord: Discord{GuildID: guild}}
	for i := range MaxServerCards + 1 {
		cards.Discord.ServerCards = append(cards.Discord.ServerCards, ServerCard{Server: fmt.Sprintf("s%d", i), Channel: card})
	}
	if err := cards.Validate(); err == nil || !strings.Contains(err.Error(), "discord.server_cards: 11 cards") {
		t.Errorf("too many cards: %v", err)
	}
	if n := (Settings{Discord: Discord{GuildID: guild, ServerCards: []ServerCard{{Server: "s", Channel: card, Note: "  padded  "}}}}).Normalized(); n.Discord.ServerCards[0].Note != "padded" {
		t.Errorf("a note is trimmed: %q", n.Discord.ServerCards[0].Note)
	}
}

const seeder = "1300000000000000004" // the opt-in seeder role

func htgSeeding() SeedingRule {
	return SeedingRule{Server: "htg-wardogs-1", Channel: card, Role: seeder, Below: 20,
		Hours: []SeedingHours{{Days: []string{"wed", "sat", "sun"}, From: "18:00", To: "23:00"}}, Cooldown: "2h", CallAtStart: true, Note: "Join from the server browser."}
}

func TestSettingsValidateSeeding(t *testing.T) {
	good := Settings{Version: 1, Stats: Stats{Timezone: "America/Chicago"}, Discord: Discord{GuildID: guild, Seeding: []SeedingRule{
		htgSeeding(),
		{Server: "htg-wardogs-2", Channel: card, Role: seeder, Below: 2}, // no hours: never calls
		{Server: "htg-wardogs-3", Channel: card, Role: seeder, Below: 1000, Cooldown: "15m", Hours: []SeedingHours{{Days: []string{"fri"}, From: "22:00", To: "02:00"}}},
	}}}
	if err := good.Validate(); err != nil {
		t.Fatalf("good rules: %v", err)
	}
	if got := good.Discord.RoleIDs(); len(got) != 0 {
		t.Errorf("a seeder role is the members' own choice; role sync never manages it: %v", got)
	}
	if d := good.Discord.Seeding[0].CooldownOrDefault(); d != 2*time.Hour {
		t.Errorf("cooldown: %s", d)
	}
	if d := good.Discord.Seeding[1].CooldownOrDefault(); d != DefaultSeedingCooldown {
		t.Errorf("default cooldown: %s", d)
	}

	rule := func(f func(*SeedingRule)) Discord {
		r := htgSeeding()
		f(&r)
		return Discord{GuildID: guild, Seeding: []SeedingRule{r}}
	}
	cases := []struct {
		name string
		d    Discord
		want []string
	}{
		{"without a guild", Discord{Seeding: []SeedingRule{htgSeeding()}}, []string{"discord.guild_id: required when roles are mapped, server cards placed or seeding set"}},
		{"server", rule(func(r *SeedingRule) { r.Server = "HTG 1" }), []string{`discord.seeding[0].server: "HTG 1" is not a server id`}},
		{"channel", rule(func(r *SeedingRule) { r.Channel = "#wd-seeding" }), []string{`discord.seeding[0].channel: "#wd-seeding" is not a Discord id`}},
		{"role missing", rule(func(r *SeedingRule) { r.Role = "" }), []string{`discord.seeding[0].role: "" is not a Discord id`}},
		{"@everyone", rule(func(r *SeedingRule) { r.Role = guild }), []string{"discord.seeding[0].role: \"" + guild + "\" is the guild's @everyone role"}},
		{"below 1", rule(func(r *SeedingRule) { r.Below = 1 }), []string{"discord.seeding[0].below: 1 is not between 2 and 1000"}},
		{"below 0", rule(func(r *SeedingRule) { r.Below = 0 }), []string{"discord.seeding[0].below: 0"}},
		{"below 1001", rule(func(r *SeedingRule) { r.Below = 1001 }), []string{"discord.seeding[0].below: 1001"}},
		{"cooldown not a duration", rule(func(r *SeedingRule) { r.Cooldown = "two hours" }), []string{`discord.seeding[0].cooldown: "two hours" is not a duration`}},
		{"cooldown short", rule(func(r *SeedingRule) { r.Cooldown = "14m" }), []string{"discord.seeding[0].cooldown: 14m is not between 15m0s and 24h0m0s"}},
		{"cooldown long", rule(func(r *SeedingRule) { r.Cooldown = "25h" }), []string{"discord.seeding[0].cooldown: 25h"}},
		{"note", rule(func(r *SeedingRule) { r.Note = strings.Repeat("x", MaxSeedingNote+1) }), []string{"discord.seeding[0].note: one line of at most 200 characters"}},
		{"note on two lines", rule(func(r *SeedingRule) { r.Note = "a\nb" }), []string{"discord.seeding[0].note"}},
		{"hours", rule(func(r *SeedingRule) {
			r.Hours = []SeedingHours{{Days: []string{"wednesday"}, From: "18:00", To: "23:00"}, {Days: []string{"sat"}, From: "18", To: "18"}, {Days: nil, From: "07:00", To: "07:00"}}
		}), []string{`discord.seeding[0].hours[0].days: "wednesday" is not a day`, `discord.seeding[0].hours[1].from: "18" is not a time of day`, `discord.seeding[0].hours[1].to`,
			"discord.seeding[0].hours[2].days: required", "discord.seeding[0].hours[2]: from and to are both 07:00"}},
		{"overlapping hours", rule(func(r *SeedingRule) {
			r.Hours = append(r.Hours, SeedingHours{Days: []string{"sun"}, From: "22:00", To: "23:30"})
		}), []string{"discord.seeding[0].hours[1]: overlaps hours[0] on sun"}},
		{"duplicate server", Discord{GuildID: guild, Seeding: []SeedingRule{htgSeeding(), htgSeeding()}}, []string{`discord.seeding[1].server: "htg-wardogs-1" already has a rule (discord.seeding[0])`}},
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
	var hours []SeedingHours
	for i := range 15 {
		hours = append(hours, SeedingHours{Days: []string{"mon"}, From: fmt.Sprintf("%02d:00", i), To: fmt.Sprintf("%02d:30", i)})
	}
	if err := (Settings{Version: 1, Discord: rule(func(r *SeedingRule) { r.Hours = hours })}).Validate(); err == nil || !strings.Contains(err.Error(), "discord.seeding[0].hours: 15 windows, at most 14") {
		t.Errorf("too many windows: %v", err)
	}
	many := Settings{Version: 1, Discord: Discord{GuildID: guild}}
	for i := range MaxSeedingRules + 1 {
		r := htgSeeding()
		r.Server = fmt.Sprintf("s%d", i)
		many.Discord.Seeding = append(many.Discord.Seeding, r)
	}
	if err := many.Validate(); err == nil || !strings.Contains(err.Error(), "discord.seeding: 11 rules, at most 10") {
		t.Errorf("too many rules: %v", err)
	}
	n := (Settings{Discord: Discord{GuildID: guild, Seeding: []SeedingRule{{Server: "s", Note: "  padded  ", Hours: []SeedingHours{}}}}}).Normalized()
	if n.Discord.Seeding[0].Note != "padded" || n.Discord.Seeding[0].Hours != nil {
		t.Errorf("normalized: %+v", n.Discord.Seeding[0])
	}
	if n := (Settings{Discord: Discord{Seeding: []SeedingRule{}}}).Normalized(); n.Discord.Seeding != nil || !n.Discord.IsZero() {
		t.Errorf("no rules normalizes away: %+v", n.Discord)
	}
	if (Discord{Seeding: []SeedingRule{htgSeeding()}}).IsZero() {
		t.Error("a seeding rule is a setting")
	}
}

func TestSettingsSeedingManifest(t *testing.T) {
	s, err := ParseManifest([]byte(`
stats:
  timezone: America/Chicago
discord:
  guild_id: "` + guild + `"
  seeding:
    - server: htg-wardogs-1
      channel: "` + card + `"
      role: "` + seeder + `"
      below: 20
      hours:
        - {days: [wed, sat, sun], from: "18:00", to: "23:00"}
      cooldown: 2h
      call_at_start: true
      note: Join from the server browser.
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.Discord.Seeding, []SeedingRule{htgSeeding()}) {
		t.Errorf("manifest: %+v", s.Discord.Seeding)
	}
	out, err := s.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "days: [wed, sat, sun]") {
		t.Errorf("days print on one line:\n%s", out)
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
	if _, err := ParseManifest([]byte("discord:\n  seeding:\n    - server: s\n      threshold: 20\n")); err == nil || !strings.Contains(err.Error(), "threshold") {
		t.Errorf("unknown keys are refused by name: %v", err)
	}
}

func TestSettingsDiscordManifest(t *testing.T) {
	s, err := ParseManifest([]byte(`
discord:
  guild_id: "` + guild + `"
  roles:
    linked: "` + linked + `"
    providers:
      steam: "` + steam + `"
  recognition:
    - role: "` + founders + `"
      rule: first_members
      count: 50
  server_cards:
    - server: htg-wardogs-1
      channel: "` + card + `"
      note: Matches start at 20 players.
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if s.Discord.GuildID != guild || s.Discord.Roles.Providers["steam"] != steam || s.Discord.Recognition[0].Count != 50 || s.Discord.ServerCards[0].Note != "Matches start at 20 players." {
		t.Errorf("manifest: %+v", s.Discord)
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
	if _, err := ParseManifest([]byte("discord:\n  roles:\n    steam_linked: \"1\"\n")); err == nil || !strings.Contains(err.Error(), "steam_linked") {
		t.Errorf("unknown keys are refused by name: %v", err)
	}
	empty, _ := DefaultSettings().Manifest()
	if strings.Contains(string(empty), "discord") {
		t.Errorf("an unset mapping is omitted:\n%s", empty)
	}
	raw, _ = json.Marshal(DefaultSettings())
	if strings.Contains(string(raw), "discord") {
		t.Errorf("an unset mapping is omitted from the stored document: %s", raw)
	}
	if s := (Settings{Discord: Discord{Roles: DiscordRoles{Providers: map[string]string{}}}}).Normalized(); s.Discord.Roles.Providers != nil || !s.Discord.IsZero() {
		t.Errorf("an empty provider map normalizes away: %+v", s.Discord)
	}
}

func TestSettingsValidateStats(t *testing.T) {
	good := Settings{Version: 1, Stats: Stats{Public: true, MinMatches: 5, Timezone: "America/Chicago",
		Seasons: []Season{{Name: "Season 02", Game: "wardogs", From: "2026-10-15", To: "2027-01-15"}, {Name: "Launch", From: "2026-10-01", To: "2026-11-01"}}}}
	if err := good.Validate(); err != nil {
		t.Fatalf("good stats: %v", err)
	}
	if loc := good.Stats.Location(); loc.String() != "America/Chicago" || (Stats{}).Location() != time.UTC {
		t.Errorf("locations: %v", loc)
	}
	if (Stats{}).MinMatchesOrDefault() != DefaultMinMatches || good.Stats.MinMatchesOrDefault() != 5 || !(Stats{}).IsZero() || good.Stats.IsZero() {
		t.Error("defaults")
	}
	if (Stats{}).RawRetentionMonthsOrDefault() != DefaultRawRetentionMonths || (Stats{RawRetentionMonths: 6}).RawRetentionMonthsOrDefault() != 6 ||
		(Stats{RawRetentionMonths: 6}).IsZero() {
		t.Error("retention defaults")
	}
	bad := Settings{Version: 1, Stats: Stats{MinMatches: MaxMinMatches + 1, Timezone: "Central", RawRetentionMonths: MaxRawRetentionMonths + 1, Seasons: []Season{
		{Name: "", From: "2026-10-15", To: "2027-01-15"},
		{Name: "Same", From: "15/10/2026", To: "2027-01-15"},
		{Name: "same", Game: "War Dogs", From: "2027-01-15", To: "2026-10-15"},
	}}}
	err := bad.Validate()
	for _, want := range []string{"stats.min_matches", "stats.raw_retention_months: 121", "stats.timezone: \"Central\"", "stats.seasons[0].name: required", "stats.seasons[1]: from and to must be dates",
		"stats.seasons[2].name: \"same\" is also stats.seasons[1]", "stats.seasons[2].game", "stats.seasons[2]: from 2027-01-15 is not before"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
	m, err := ParseManifest([]byte("stats:\n  public: true\n  timezone: America/Chicago\n  seasons:\n    - name: \" Season 02 \"\n      from: \"2026-10-15\"\n      to: \"2027-01-15\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if n := m.Normalized(); n.Stats.Seasons[0].Name != "Season 02" || !n.Stats.Public {
		t.Errorf("manifest: %+v", n.Stats)
	}
}

func TestSettingsValidateGames(t *testing.T) {
	good := Settings{Version: 1, Games: map[string]GameSettings{
		"wardogs": {Theme: GameTheme{Light: Tokens{Accent: "#8a5a00"}, Dark: Tokens{Accent: "#f0b429"}, BannerURL: "https://hiddentoken.com/brand/wardogs.png", BannerAlt: "War Dogs"},
			Nav: []NavLink{{Label: "Playbook", URL: "https://hiddentoken.com/wardogs/"}, {Label: "Config", URL: "/servers/htg-wardogs-1", Role: "owner"}}},
		"cs2":  {Theme: GameTheme{Dark: Tokens{Accent: "orange"}}}, // one scheme only
		"pubg": {Nav: []NavLink{{Label: "Squads", URL: "/boards?scope=game:pubg"}}},
	}}
	if err := good.Validate(); err != nil {
		t.Fatalf("good games: %v", err)
	}
	var nav []NavLink
	for range MaxGameNav + 1 {
		nav = append(nav, NavLink{Label: "x", URL: "/x"})
	}
	bad := Settings{Version: 1, Games: map[string]GameSettings{
		"War Dogs": {},
		"wardogs": {Theme: GameTheme{Light: Tokens{Accent: "red; }", Background: "#fff"}, Dark: Tokens{Foreground: "#000"}, BannerURL: "http://insecure.example/b.png", BannerAlt: strings.Repeat("x", MaxBannerAlt+1)},
			Nav: []NavLink{{Label: "", URL: "javascript:alert(1)", Placement: "footer", Role: "admin"}}},
		"cs2":  {Nav: nav},
		"pubg": {Theme: GameTheme{BannerAlt: "a banner nobody set"}},
		"sot":  {Theme: GameTheme{BannerURL: "/b.png", BannerAlt: "two\nlines"}},
	}}
	err := bad.Validate()
	if !errors.Is(err, ErrInvalidSettings) {
		t.Fatalf("every failure wraps ErrInvalidSettings: %v", err)
	}
	for _, want := range []string{
		`games: "War Dogs" is not a game id`,
		"games.wardogs.theme.light.accent: \"red; }\" is not a plain CSS colour",
		"games.wardogs.theme.light: a game overlays only the accent",
		"games.wardogs.theme.dark: a game overlays only the accent",
		"games.wardogs.theme.banner_url",
		"games.wardogs.theme.banner_alt: one line of at most 120",
		"games.wardogs.nav[0].label", "games.wardogs.nav[0].url", "games.wardogs.nav[0].role", "games.wardogs.nav[0].placement",
		"games.cs2.nav: 7 links, at most 6",
		"games.pubg.theme.banner_alt: set without a banner_url",
		"games.sot.theme.banner_alt: one line",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
	many := Settings{Version: 1, Games: map[string]GameSettings{}}
	for i := range MaxGames + 1 {
		many.Games[fmt.Sprintf("g%d", i)] = GameSettings{}
	}
	if err := many.Validate(); err == nil || !strings.Contains(err.Error(), "games: 21 games, at most 20") {
		t.Errorf("too many games: %v", err)
	}
}

func TestSettingsCheckGames(t *testing.T) {
	known := []string{"cs2", "wardogs"}
	ok := Settings{Version: 1, Games: map[string]GameSettings{"wardogs": {Theme: GameTheme{Dark: Tokens{Accent: "#f0b429"}}}},
		Stats: Stats{Seasons: []Season{{Name: "S2", Game: "wardogs", From: "2026-10-15", To: "2027-01-15"}, {Name: "All", From: "2026-10-15", To: "2027-01-15"}}}}
	if err := ok.CheckGames(known); err != nil {
		t.Errorf("known games: %v", err)
	}
	if err := (Settings{}).CheckGames(known); err != nil {
		t.Errorf("no games: %v", err)
	}
	bad := Settings{Version: 1, Games: map[string]GameSettings{"wardog": {}}, Stats: Stats{Seasons: []Season{{Name: "S2", Game: "war-dogs", From: "2026-10-15", To: "2027-01-15"}}}}
	err := bad.CheckGames(known)
	if !errors.Is(err, ErrInvalidSettings) {
		t.Fatalf("wraps ErrInvalidSettings: %v", err)
	}
	for _, want := range []string{`games: "wardog" is not a game this hub ships (cs2, wardogs)`, `stats.seasons[0].game: "war-dogs" is not a game this hub ships`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestSettingsGamesManifest(t *testing.T) {
	s, err := ParseManifest([]byte(`
games:
  wardogs:
    theme:
      light: {accent: "#8a5a00"}
      dark: {accent: "#f0b429"}
      banner_url: /static/wardogs.png
      banner_alt: " War Dogs "
    nav:
      - label: " Playbook "
        url: https://hiddentoken.com/wardogs/
  cs2: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	w := s.Games["wardogs"]
	if w.Theme.Dark.Accent != "#f0b429" || w.Theme.BannerAlt != "War Dogs" || w.Nav[0].Label != "Playbook" || w.Nav[0].Placement != "" {
		t.Errorf("parsed: %+v", w)
	}
	if _, ok := s.Games["cs2"]; ok {
		t.Error("a game that sets nothing is dropped")
	}
	out, err := s.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	again, err := ParseManifest(out)
	if err != nil || !reflect.DeepEqual(again.Games, s.Games) {
		t.Errorf("round trip: %+v %v\n%s", again.Games, err, out)
	}
	if strings.Contains(string(out), "cs2") || strings.Contains(string(out), "background") {
		t.Errorf("empty games and tokens are omitted:\n%s", out)
	}
	if _, err := ParseManifest([]byte("games:\n  wardogs:\n    theme:\n      colour: red\n")); err == nil || !strings.Contains(err.Error(), "colour") {
		t.Errorf("unknown keys are refused by name: %v", err)
	}
	if n := (Settings{Games: map[string]GameSettings{"cs2": {}}}).Normalized(); n.Games != nil {
		t.Errorf("only empty games normalize to none: %+v", n.Games)
	}
	raw, err := json.Marshal(s.Normalized())
	if err != nil {
		t.Fatal(err)
	}
	stored, err := ParseSettings(raw)
	if err != nil || !reflect.DeepEqual(stored.Games, s.Games) {
		t.Errorf("json round trip: %+v %v\n%s", stored.Games, err, raw)
	}
}

func TestSettingsServiceChecksGames(t *testing.T) {
	ctx := context.Background()
	set := Settings{Games: map[string]GameSettings{"wardog": {Theme: GameTheme{Dark: Tokens{Accent: "#f0b429"}}}}}

	open, _ := newTestService(&orgtest.FakeStore{})
	if _, _, err := open.EnsureBuiltin(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := open.UpdateSettings(ctx, set); err != nil {
		t.Errorf("without WithGames the ids are only format-checked: %v", err)
	}

	s, _ := newTestService(&orgtest.FakeStore{})
	s.WithGames([]string{"wardogs", "cs2"})
	if _, _, err := s.EnsureBuiltin(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSettings(ctx, set); !errors.Is(err, ErrInvalidSettings) || !strings.Contains(err.Error(), `"wardog" is not a game this hub ships (cs2, wardogs)`) {
		t.Errorf("an unknown game is refused: %v", err)
	}
	if got, _, _ := s.Settings(ctx); got.Games != nil {
		t.Error("a refused update must not be stored")
	}
	set.Games = map[string]GameSettings{"wardogs": set.Games["wardog"]}
	got, err := s.UpdateSettings(ctx, set)
	if err != nil || got.Games["wardogs"].Theme.Dark.Accent != "#f0b429" {
		t.Errorf("a known game is stored: %+v %v", got.Games, err)
	}
	if stored, _, _ := s.Settings(ctx); !reflect.DeepEqual(stored.Games, got.Games) {
		t.Errorf("stored: %+v", stored.Games)
	}
}

func TestSettingsValidateGameMaps(t *testing.T) {
	good := Settings{Version: 1, Games: map[string]GameSettings{"wardogs": {
		Maps: map[string]string{"Ozeti": "https://hiddentoken.com/games/wardogs/maps/ozeti.jpg", "NorthAmerica": "https://cdn.example/na.png",
			"Europe/DayClear": "https://hiddentoken.com/games/wardogs/maps/Europe/DayClear.webp"},
		MapFallback: "https://hiddentoken.com/games/wardogs/maps/default.jpg"}}}
	if err := good.Validate(); err != nil {
		t.Fatalf("good maps: %v", err)
	}
	if good.Games["wardogs"].IsZero() || (GameSettings{MapFallback: "https://x.example/f.png"}).IsZero() {
		t.Error("maps or a fallback alone are settings")
	}
	many := map[string]string{}
	for i := range MaxGameMaps + 1 {
		many[fmt.Sprintf("Map%d", i)] = "https://x.example/m.png"
	}
	bad := Settings{Version: 1, Games: map[string]GameSettings{
		"wardogs": {Maps: map[string]string{
			"Ozeti":                 "/games/wardogs/ozeti.jpg", // site-relative: Discord can't fetch it
			"Bakurani":              "http://insecure.example/b.jpg",
			" Padded":               "https://x.example/p.png",
			"":                      "https://x.example/e.png",
			strings.Repeat("m", 65): "https://x.example/l.png",
			"Europe/":               "https://x.example/1.png",
			"/DayClear":             "https://x.example/2.png",
			"Europe/Day/Clear":      "https://x.example/3.png",
			"Europe/ DayClear":      "https://x.example/4.png",
		}, MapFallback: "javascript:alert(1)"},
		"cs2": {Maps: many},
	}}
	err := bad.Validate()
	for _, want := range []string{
		`games.wardogs.maps.Ozeti: "/games/wardogs/ozeti.jpg" must be an https URL`,
		`games.wardogs.maps.Bakurani: "http://insecure.example/b.jpg" must be an https URL`,
		`games.wardogs.maps: " Padded" is not a map name`,
		`games.wardogs.maps: "" is not a map name`,
		`games.wardogs.maps: "mmmm`,
		`games.wardogs.maps: "Europe/" is not a map name or map/lighting`,
		`games.wardogs.maps: "/DayClear" is not`,
		`games.wardogs.maps: "Europe/Day/Clear" is not`,
		`games.wardogs.maps: "Europe/ DayClear" is not`,
		`games.wardogs.map_fallback: "javascript:alert(1)" must be an https URL`,
		"games.cs2.maps: 51 maps, at most 50",
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
	s, err := ParseManifest([]byte("games:\n  wardogs:\n    maps:\n      Ozeti: https://x.example/o.jpg\n    map_fallback: https://x.example/f.jpg\n  cs2:\n    maps: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if w := s.Games["wardogs"]; w.Maps["Ozeti"] != "https://x.example/o.jpg" || w.MapFallback != "https://x.example/f.jpg" {
		t.Errorf("parsed: %+v", w)
	}
	if _, ok := s.Games["cs2"]; ok {
		t.Error("an empty maps section is no setting")
	}
	out, _ := s.Manifest()
	if again, err := ParseManifest(out); err != nil || !reflect.DeepEqual(again.Games, s.Games) {
		t.Errorf("round trip: %+v %v\n%s", again.Games, err, out)
	}
}
