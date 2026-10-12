package cloudvm

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/gravel-project/gravel/games/cs2"
)

// cs2Profile is the cloudvm section gravel ships for Counter-Strike 2.
func cs2Profile(t *testing.T) Profile {
	t.Helper()
	var spec struct {
		CloudVM Profile `yaml:"cloudvm"`
	}
	if err := yaml.Unmarshal(cs2.GameSpec, &spec); err != nil {
		t.Fatal(err)
	}
	return spec.CloudVM
}

func TestShippedCS2Profile(t *testing.T) {
	p := cs2Profile(t)
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if p.RCONPort != 27015 || p.RuntimeUID != 1000 || p.InstallDir != "/home/steam/cs2-dedicated" {
		t.Errorf("profile %+v", p)
	}
	if p.Env["CS2_SERVER_HIBERNATE"] != "0" {
		t.Error("hibernation must stay off: the image warns it crashes, and the poll needs the server awake")
	}
	for _, s := range p.Settings {
		if s.Env == "CS2_ADDITIONAL_ARGS" {
			t.Error("the image evals CS2_ADDITIONAL_ARGS; it must not be a setting")
		}
	}
}

func TestProfileValidate(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(*Profile)
		want string
	}{
		{"image by tag", func(p *Profile) { p.Image = "ghcr.io/joedwards32/cs2:5.0.0" }, "pinned by digest"},
		{"install dir", func(p *Profile) { p.InstallDir = "relative" }, "install_dir"},
		{"timezone", func(p *Profile) { p.LogTimezone = "Mars/Olympus" }, "log_timezone"},
		{"ready timeout", func(p *Profile) { p.ReadyTimeout = 0 }, "ready_timeout"},
		{"public rcon", func(p *Profile) { p.Ports[1].Public = true }, "never public"},
		{"no rcon port", func(p *Profile) { p.RCONPort = 1 }, "rcon_port"},
		{"bad env", func(p *Profile) { p.Env["lower"] = "x" }, "environment variable name"},
		{"setting env twice", func(p *Profile) { p.Settings[1].Env = p.Settings[0].Env }, "no other line sets"},
		{"setting env is a secret's", func(p *Profile) { p.Settings[0].Env = "CS2_RCONPW" }, "no other line sets"},
		{"bad type", func(p *Profile) { p.Settings[0].Type = "float" }, "type"},
		{"bad default", func(p *Profile) { p.Settings[6].Default = "200" }, "the default"},
		{"conflict with nothing", func(p *Profile) { p.Settings[0].Conflicts = []string{"nope"} }, `"nope" is not a setting`},
		{"port when not a bool", func(p *Profile) { p.Ports[2].When = "server_name" }, "not a bool setting"},
		{"team without a name", func(p *Profile) { p.Teams = append(p.Teams, Team{Key: "X"}) }, "a name is required"},
		{"team twice", func(p *Profile) { p.Teams = append(p.Teams, Team{Name: "ct"}) }, `"ct" is listed twice`},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := cs2Profile(t)
			p.Env = map[string]string{"CS2_PORT": "27015"}
			c.edit(&p)
			if err := p.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err %v, want %q", err, c.want)
			}
		})
	}
}

func TestProfileValues(t *testing.T) {
	p := cs2Profile(t)
	v := p.Values(map[string]string{"max_players": "12"}, "My Server")
	want := map[string]string{"server_name": "My Server", "game_alias": "competitive", "map_group": "mg_active",
		"start_map": "de_inferno", "max_players": "12", "tv": "false"}
	for k, w := range want {
		if v[k] != w {
			t.Errorf("%s = %q, want %q", k, v[k], w)
		}
	}
	// A setting that conflicts with one set takes no default.
	v = p.Values(map[string]string{"game_type": "1", "game_mode": "2"}, "x")
	if _, set := v["game_alias"]; set {
		t.Error("game_alias defaulted beside game_type")
	}
	tv := p.Ports[2]
	if tv.Open(map[string]string{"tv": "false"}) || !tv.Open(map[string]string{"tv": "true"}) || !p.Ports[0].Open(nil) {
		t.Error("Port.Open")
	}
}
