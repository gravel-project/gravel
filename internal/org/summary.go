package org

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Summary is the settings in one line, without a secret: what `gravel-hub settings` and the
// API's CheckSettings and ApplySettings say about a manifest.
func (s Settings) Summary() string {
	out := fmt.Sprintf("%d nav links, theme %s, discord %s", len(s.Nav), describeTheme(s.Theme), describeDiscord(s.Discord))
	if !s.Stats.IsZero() {
		out += ", stats " + describeStats(s.Stats)
	}
	if !s.Events.IsZero() {
		out += fmt.Sprintf(", events kept %d days", s.Events.RetentionDaysOrDefault())
	}
	if l := s.LocalAccounts; !l.IsZero() {
		reg := "registration closed"
		if l.Open() {
			reg = "registration open"
		}
		minLen := l.MinPasswordLength
		if minLen == 0 {
			minLen = DefaultMinPasswordLength
		}
		out += fmt.Sprintf(", local accounts (%s, passwords of %d+ characters)", reg, minLen)
	}
	if len(s.Games) > 0 {
		out += ", games " + describeGames(s.Games)
	}
	return out
}

func describeGames(games map[string]GameSettings) string {
	var parts []string
	for _, id := range slices.Sorted(maps.Keys(games)) {
		g := games[id]
		var what []string
		if g.Theme.Light.Accent != "" || g.Theme.Dark.Accent != "" {
			what = append(what, "accent")
		}
		if g.Theme.BannerURL != "" {
			what = append(what, "banner")
		}
		if n := len(g.Nav); n > 0 {
			what = append(what, fmt.Sprintf("%d links", n))
		}
		if n := len(g.Maps); n > 0 {
			what = append(what, fmt.Sprintf("%d maps", n))
		}
		if g.MapFallback != "" {
			what = append(what, "map fallback")
		}
		parts = append(parts, id+" ("+strings.Join(what, ", ")+")")
	}
	return strings.Join(parts, ", ")
}

func describeStats(st Stats) string {
	vis := "owner only"
	if st.Public {
		vis = "public"
	}
	tz := st.Timezone
	if tz == "" {
		tz = "UTC"
	}
	return fmt.Sprintf("%s, K/D from %d matches, %s, %d seasons", vis, st.MinMatchesOrDefault(), tz, len(st.Seasons))
}

func describeTheme(t Theme) string {
	if t == (Theme{}) {
		return "default"
	}
	parts := 0
	for _, v := range []string{t.Light.Accent, t.Light.Background, t.Light.Foreground, t.Light.Muted, t.Light.Line, t.Light.OK, t.Light.Err,
		t.Dark.Accent, t.Dark.Background, t.Dark.Foreground, t.Dark.Muted, t.Dark.Line, t.Dark.OK, t.Dark.Err} {
		if v != "" {
			parts++
		}
	}
	extra := ""
	if t.LogoURL != "" {
		extra += ", logo"
	}
	if t.Font != "" {
		extra += ", font"
	}
	return fmt.Sprintf("%d tokens%s", parts, extra)
}

func describeDiscord(d Discord) string {
	if d.IsZero() {
		return "unmapped"
	}
	out := fmt.Sprintf("guild %s, %d roles", d.GuildID, len(d.RoleIDs()))
	for _, r := range d.Recognition {
		if r.MinMembers > 0 {
			out += fmt.Sprintf(", recognition role %s off until %d members", r.Role, r.MinMembers)
		}
	}
	if n := len(d.ServerCards); n > 0 {
		out += fmt.Sprintf(", %d server cards", n)
	}
	if d.ModLog != "" {
		out += ", mod log"
	}
	if n := len(d.Seeding); n > 0 {
		out += fmt.Sprintf(", %d seeding rules", n)
	}
	if n := len(d.Recaps); n > 0 {
		out += fmt.Sprintf(", %d recap rules", n)
	}
	if n := len(d.Streams.Streamers); n > 0 {
		out += fmt.Sprintf(", go-live posts for %d streamers", n)
	}
	if n := len(d.News.Feeds); n > 0 {
		out += fmt.Sprintf(", %d Steam news feeds", n)
	}
	if r := d.MatchResults; !r.IsZero() {
		if n := len(r.Servers); n > 0 {
			out += fmt.Sprintf(", match results for %d servers", n)
		} else {
			out += ", match results for every server"
		}
	}
	if n := len(d.Presence.Games); n > 0 {
		out += fmt.Sprintf(", a presence card with %d games", n)
	}
	return out
}
