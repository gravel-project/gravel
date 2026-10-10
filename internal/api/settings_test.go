package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
)

func TestOrganizationSettings(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	anon := hubv1connect.NewOrganizationServiceClient(http.DefaultClient, r.srv.URL)

	got, err := anon.GetOrganizationSettings(ctx, connect.NewRequest(&hubv1.GetOrganizationSettingsRequest{}))
	if err != nil || got.Msg.GetSettings().GetVersion() != 1 || got.Msg.GetSettings().GetUpdatedAt() != nil || len(got.Msg.GetSettings().GetNav()) != 0 {
		t.Fatalf("defaults are public: %v %v", err, got)
	}

	want := &hubv1.OrganizationSettings{
		Theme: &hubv1.Theme{Dark: &hubv1.ThemeTokens{Accent: "#3ee07a", Background: "#070b17"}, Light: &hubv1.ThemeTokens{Accent: "#0a7a3a"}, Font: "Archivo, system-ui, sans-serif", LogoUrl: "https://hiddentoken.com/brand/mark.svg"},
		Nav:   []*hubv1.NavLink{{Label: "Discord", Url: "https://discord.gg/x"}, {Label: "Rules", Url: "https://hiddentoken.com/rules/", Placement: "footer"}, {Label: "Admin", Url: "/admin", Role: "owner"}},
		Discord: &hubv1.DiscordSettings{GuildId: "519496143298756611",
			Roles:       &hubv1.DiscordRoles{Linked: "1300000000000000001", Providers: map[string]string{"steam": "1300000000000000002"}},
			Recognition: []*hubv1.DiscordRecognition{{Role: "1300000000000000003", Rule: "first_members", Count: 50}},
			ServerCards: []*hubv1.DiscordServerCard{{Server: "htg-wardogs-1", Channel: "1300000000000000100", Note: "Matches start at 20 players."}}, ModLog: "1300000000000000200"},
		Stats: &hubv1.StatsSettings{MinMatches: 5, Timezone: "America/Chicago", RawRetentionMonths: 6, Seasons: []*hubv1.StatsSeason{{Name: "Season 02", Game: "wardogs", From: "2026-10-15", To: "2027-01-15"}}},
	}
	if _, err := anon.UpdateOrganizationSettings(ctx, connect.NewRequest(&hubv1.UpdateOrganizationSettingsRequest{Settings: want})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("anonymous update: %v", err)
	}
	jo := r.user(t, "Jo", "1")
	sam := r.user(t, "Sam", "2")
	joClient := hubv1connect.NewOrganizationServiceClient(cookieClient(http.DefaultClient, r.login(t, jo.ID)), r.srv.URL)
	samClient := hubv1connect.NewOrganizationServiceClient(cookieClient(http.DefaultClient, r.login(t, sam.ID)), r.srv.URL)
	if _, err := joClient.UpdateOrganizationSettings(ctx, connect.NewRequest(&hubv1.UpdateOrganizationSettingsRequest{Settings: want})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a member before any claim: %v", err)
	}
	if _, err := r.org.Claim(ctx, r.token, jo.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := samClient.UpdateOrganizationSettings(ctx, connect.NewRequest(&hubv1.UpdateOrganizationSettingsRequest{Settings: want})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a member: %v", err)
	}
	upd, err := joClient.UpdateOrganizationSettings(ctx, connect.NewRequest(&hubv1.UpdateOrganizationSettingsRequest{Settings: want}))
	if err != nil {
		t.Fatal(err)
	}
	s := upd.Msg.GetSettings()
	if s.GetVersion() != 1 || s.GetUpdatedAt() == nil || s.GetTheme().GetDark().GetAccent() != "#3ee07a" || s.GetTheme().GetLogoUrl() != "https://hiddentoken.com/brand/mark.svg" || len(s.GetNav()) != 3 || s.GetNav()[0].GetPlacement() != "header" || s.GetNav()[1].GetPlacement() != "footer" || s.GetNav()[2].GetRole() != "owner" {
		t.Errorf("update: %v", s)
	}
	got, _ = anon.GetOrganizationSettings(ctx, connect.NewRequest(&hubv1.GetOrganizationSettingsRequest{}))
	if got.Msg.GetSettings().GetTheme().GetFont() != "Archivo, system-ui, sans-serif" || got.Msg.GetSettings().GetUpdatedAt() == nil {
		t.Errorf("read back: %v", got.Msg)
	}
	// The Discord mapping is public too: a bot reads it with its app token, the pages anonymously.
	d := got.Msg.GetSettings().GetDiscord()
	if d.GetGuildId() != "519496143298756611" || d.GetRoles().GetLinked() != "1300000000000000001" || d.GetRoles().GetProviders()["steam"] != "1300000000000000002" ||
		len(d.GetRecognition()) != 1 || d.GetRecognition()[0].GetRule() != "first_members" || d.GetRecognition()[0].GetCount() != 50 ||
		d.GetModLog() != "1300000000000000200" || len(d.GetServerCards()) != 1 || d.GetServerCards()[0].GetServer() != "htg-wardogs-1" || d.GetServerCards()[0].GetNote() != "Matches start at 20 players." {
		t.Errorf("discord read back: %v", d)
	}
	if st := got.Msg.GetSettings().GetStats(); st.GetMinMatches() != 5 || st.GetRawRetentionMonths() != 6 || st.GetTimezone() != "America/Chicago" || len(st.GetSeasons()) != 1 || st.GetSeasons()[0].GetGame() != "wardogs" || st.GetPublic() {
		t.Errorf("stats read back: %v", st)
	}

	bad := &hubv1.OrganizationSettings{Theme: &hubv1.Theme{Light: &hubv1.ThemeTokens{Accent: "red; }"}, LogoUrl: "http://insecure.example/x.png"}, Nav: []*hubv1.NavLink{{Label: "", Url: "javascript:alert(1)", Placement: "sidebar"}},
		Discord: &hubv1.DiscordSettings{Roles: &hubv1.DiscordRoles{Providers: map[string]string{"xbox": "1300000000000000002"}}}}
	_, err = joClient.UpdateOrganizationSettings(ctx, connect.NewRequest(&hubv1.UpdateOrganizationSettingsRequest{Settings: bad}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid: %v", err)
	}
	for _, want := range []string{"theme.light.accent", "theme.logo_url", "nav[0].label", "nav[0].url", "nav[0].placement", "discord.guild_id", "discord.roles.providers.xbox"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error names %s: %v", want, err)
		}
	}
	got, _ = anon.GetOrganizationSettings(ctx, connect.NewRequest(&hubv1.GetOrganizationSettingsRequest{}))
	if got.Msg.GetSettings().GetTheme().GetDark().GetAccent() != "#3ee07a" {
		t.Error("an invalid update must leave the settings as they were")
	}
	// Clearing the mapping: a document without the section has none.
	if _, err := joClient.UpdateOrganizationSettings(ctx, connect.NewRequest(&hubv1.UpdateOrganizationSettingsRequest{Settings: &hubv1.OrganizationSettings{}})); err != nil {
		t.Fatal(err)
	}
	if got, _ = anon.GetOrganizationSettings(ctx, connect.NewRequest(&hubv1.GetOrganizationSettingsRequest{})); got.Msg.GetSettings().GetDiscord() != nil {
		t.Errorf("a cleared mapping is absent: %v", got.Msg.GetSettings().GetDiscord())
	}
}
