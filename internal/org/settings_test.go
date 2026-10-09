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
)

func TestSettingsValidateDiscord(t *testing.T) {
	good := Settings{Version: 1, Discord: Discord{GuildID: guild,
		Roles:       DiscordRoles{Linked: linked, Providers: map[string]string{"steam": steam}},
		Recognition: []Recognition{{Role: founders, Rule: RuleFirstMembers, Count: 50}}}}
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
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if s.Discord.GuildID != guild || s.Discord.Roles.Providers["steam"] != steam || s.Discord.Recognition[0].Count != 50 {
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
