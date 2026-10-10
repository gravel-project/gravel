package servers

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

const goodManifest = `
version: 1
games:
  - id: wardogs
    bands:
      - section: MatchState.Playing.KOTH
        key: ScorePeriod
        min: 21
servers:
  - id: htg-wardogs-1
    name: "HTG WARDOGS | NA WEST | #1"
    game: wardogs
    driver: wardogs
    location: qonzer-slc
    endpoint: http://203.0.113.10:7789
    credential_file: /run/secrets/htg-wardogs-rcon-password
    trust: official
    seeding:
      threshold: 20
      hours: "17:00-23:00"
      quiet: "23:30-09:00"
      timezone: America/Chicago
    feed:
      url: https://ingest.example.com
      token_file: /run/secrets/htg-wardogs-feed-token
`

func i64(v int64) *int64 { return &v }

func TestShippedSpecs(t *testing.T) {
	specs := Specs()
	wd, ok := specs["wardogs"]
	if !ok || wd.Name != "War Dogs" || wd.IdentityProvider != "steam" || len(wd.Drivers) != 1 || wd.Drivers[0] != "wardogs" {
		t.Fatalf("wardogs spec = %+v", wd)
	}
	sp, ok := wd.Band("MatchState.Playing.KOTH", "ScorePeriod")
	if !ok || *sp.Min != 18 || *sp.Max != 30 {
		t.Errorf("ScorePeriod band = %+v", sp)
	}
	mrp, ok := wd.Band("MatchState.PreMatch.WaitingForPlayers.PlayerCount", "MinimumRequiredPlayers")
	if !ok || *mrp.Min != 20 || mrp.Max != nil {
		t.Errorf("MinimumRequiredPlayers band = %+v", mrp)
	}
	if _, err := ParseSpec([]byte("id: x\nname: X\nidentity_provider: steam\ndrivers: [x]\nextra: 1\n")); err == nil {
		t.Error("a spec with an unknown key parsed")
	}
	if _, err := ParseSpec([]byte("id: x\n")); err == nil {
		t.Error("a spec without its required fields parsed")
	}
}

func TestParseManifestAndDefaults(t *testing.T) {
	m, err := ParseManifest([]byte(goodManifest))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(Specs(), []string{"wardogs"}); err != nil {
		t.Fatal(err)
	}
	s := m.Servers[0]
	if time.Duration(s.PollInterval) != DefaultPollInterval || time.Duration(s.Seeding.Cooldown) != DefaultCooldown {
		t.Errorf("defaults = %s, %s", s.PollInterval, s.Seeding.Cooldown)
	}
	// Marshal and parse again: the same manifest.
	out, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	again, err := ParseManifest(out)
	if err != nil {
		t.Fatal(err)
	}
	out2, _ := again.Marshal()
	if string(out) != string(out2) {
		t.Errorf("round trip changed the manifest:\n%s\n---\n%s", out, out2)
	}
	if !strings.Contains(string(out), "poll_interval: 15s") || !strings.Contains(string(out), "cooldown: 1h0m0s") {
		t.Errorf("durations are not written as strings:\n%s", out)
	}
	if _, err := ParseManifest([]byte(goodManifest + "    extra: 1\n")); err == nil {
		t.Error("an unknown key parsed")
	}
	if _, err := ParseManifest([]byte("servers:\n  - poll_interval: soon\n")); err == nil || !strings.Contains(err.Error(), "not a duration") {
		t.Errorf("a bad duration = %v", err)
	}
}

func TestNormalizedSortsAndCompares(t *testing.T) {
	a := Manifest{Servers: []Server{{ID: "b"}, {ID: "a"}}, Games: []GameRef{{ID: "z"}, {ID: "y", Bands: []Band{{Section: "S", Key: "b"}, {Section: "S", Key: "a"}}}}}.Normalized()
	if a.Servers[0].ID != "a" || a.Games[0].ID != "y" || a.Games[0].Bands[0].Key != "a" || a.Version != ManifestVersion {
		t.Errorf("normalized = %+v", a)
	}
}

// Every rule names its field, and every problem is reported at once.
func TestValidate(t *testing.T) {
	base := func() Manifest {
		m, err := ParseManifest([]byte(goodManifest))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	for _, c := range []struct {
		name string
		edit func(*Manifest)
		want string
	}{
		{"version", func(m *Manifest) { m.Version = 2 }, "version: got 2"},
		{"unknown game", func(m *Manifest) { m.Games = append(m.Games, GameRef{ID: "quake"}) }, `games[1].id: "quake" is not a game this hub ships (cs2, pubg, seaofthieves, starcitizen, wardogs)`},
		{"catalog game", func(m *Manifest) {
			m.Games = append(m.Games, GameRef{ID: "cs2"})
			m.Servers[0].Game = "cs2"
		}, "servers[0].game: Counter-Strike 2 is a catalog entry; gravel has no driver for its servers yet"},
		{"length loosened", func(m *Manifest) {
			m.Games[0].Bands = append(m.Games[0].Bands, Band{Section: "/Script/WDGame.WDGameSession", Key: "ServerName", MaxLength: i64(65)})
		}, "max_length: 65 must be from 1 to the spec's 64"},
		{"hosts loosened", func(m *Manifest) {
			m.Games[0].Bands = append(m.Games[0].Bands, Band{Section: "/Script/WDGame.WDGameSession", Key: "ServerImageURL", Hosts: []string{"ibb.co", "example.com"}})
		}, `hosts: "example.com" loosens the spec's hosts`},
		{"hosts emptied", func(m *Manifest) {
			m.Games[0].Bands = append(m.Games[0].Bands, Band{Section: "/Script/WDGame.WDGameSession", Key: "ServerImageURL", Hosts: []string{}})
		}, "hosts: an empty list allows no URL"},
		{"kind changed", func(m *Manifest) {
			m.Games[0].Bands = append(m.Games[0].Bands, Band{Section: "/Script/WDGame.WDGameSession", Key: "ServerName", Max: i64(10)})
		}, "the spec's band for ServerName is a length band, not a range band"},
		{"two kinds", func(m *Manifest) { m.Games[0].Bands[0].MaxLength = i64(5) }, "set only one of min/max, max_length or hosts"},
		{"condition changed", func(m *Manifest) {
			m.Games[0].Bands = append(m.Games[0].Bands, Band{Section: "/Script/WDGame.WDGameSession", Key: "MaxReservedSlots", Max: i64(0), UnlessSet: "ServerPassword"})
		}, "unless_set: the spec's condition can't be changed by a manifest"},
		{"game twice", func(m *Manifest) { m.Games = append(m.Games, GameRef{ID: "wardogs"}) }, `"wardogs" is listed twice`},
		{"band loosened", func(m *Manifest) { m.Games[0].Bands[0].Min = i64(10) }, "min: 10 loosens the spec's 18"},
		{"band loosened max", func(m *Manifest) { m.Games[0].Bands[0].Max = i64(31) }, "max: 31 loosens the spec's 30"},
		{"band unknown", func(m *Manifest) {
			m.Games[0].Bands = append(m.Games[0].Bands, Band{Section: "X", Key: "TickRate", Max: i64(1)})
		}, "has no band for X TickRate"},
		{"band empty", func(m *Manifest) { m.Games[0].Bands[0].Min = nil }, "neither min nor max is set"},
		{"band crossed", func(m *Manifest) {
			m.Games[0].Bands[0].Min, m.Games[0].Bands[0].Max = i64(27), i64(24)
		}, "min 27 is above max 24"},
		{"id", func(m *Manifest) { m.Servers[0].ID = "HTG 1" }, `servers[0].id: "HTG 1"`},
		{"id twice", func(m *Manifest) { m.Servers = append(m.Servers, m.Servers[0]) }, `servers[1].id: "htg-wardogs-1" is listed twice`},
		{"name", func(m *Manifest) { m.Servers[0].Name = strings.Repeat("x", 65) }, "servers[0].name: required, at most 64"},
		{"game not enabled", func(m *Manifest) { m.Servers[0].Game = "cs2" }, `servers[0].game: "cs2" is not in games`},
		{"driver unknown", func(m *Manifest) { m.Servers[0].Driver = "rcon" }, `servers[0].driver: "rcon" is not a driver this hub has`},
		{"location", func(m *Manifest) { m.Servers[0].Location = "" }, "servers[0].location"},
		{"endpoint path", func(m *Manifest) { m.Servers[0].Endpoint = "http://203.0.113.10:7789/v1" }, "servers[0].endpoint"},
		{"endpoint creds", func(m *Manifest) { m.Servers[0].Endpoint = "http://u:p@203.0.113.10:7789" }, "servers[0].endpoint"},
		{"endpoint scheme", func(m *Manifest) { m.Servers[0].Endpoint = "ftp://203.0.113.10" }, "servers[0].endpoint"},
		{"credential relative", func(m *Manifest) { m.Servers[0].CredentialFile = "rcon.txt" }, "servers[0].credential_file"},
		{"credential unclean", func(m *Manifest) { m.Servers[0].CredentialFile = "/run/secrets/../etc/x" }, "servers[0].credential_file"},
		{"poll too fast", func(m *Manifest) { m.Servers[0].PollInterval = Duration(time.Second) }, "servers[0].poll_interval: 1s is outside"},
		{"trust", func(m *Manifest) { m.Servers[0].Trust = "" }, "servers[0].trust"},
		{"seeding threshold", func(m *Manifest) { m.Servers[0].Seeding.Threshold = 0 }, "seeding.threshold"},
		{"seeding hours", func(m *Manifest) { m.Servers[0].Seeding.Hours = "5pm-11pm" }, "seeding.hours"},
		{"seeding quiet", func(m *Manifest) { m.Servers[0].Seeding.Quiet = "24:00-01:00" }, "seeding.quiet"},
		{"seeding zone", func(m *Manifest) { m.Servers[0].Seeding.Timezone = "Central" }, `seeding.timezone: "Central"`},
		{"seeding cooldown", func(m *Manifest) { m.Servers[0].Seeding.Cooldown = Duration(time.Second) }, "seeding.cooldown"},
		{"feed url path", func(m *Manifest) { m.Servers[0].Feed.URL = "https://ingest.example.com/api/ingest/events" }, "servers[0].feed.url"},
		{"feed url scheme", func(m *Manifest) { m.Servers[0].Feed.URL = "ingest.example.com" }, "servers[0].feed.url"},
		{"feed token relative", func(m *Manifest) { m.Servers[0].Feed.TokenFile = "feed.txt" }, "servers[0].feed.token_file"},
		{"feed token is the credential", func(m *Manifest) { m.Servers[0].Feed.TokenFile = m.Servers[0].CredentialFile }, "must not be the RCON credential"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := base()
			c.edit(&m)
			err := m.Validate(Specs(), []string{"wardogs"})
			if err == nil || !errors.Is(err, ErrInvalidManifest) || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
	m := base()
	m.Servers[0].Trust, m.Servers[0].Location = "", ""
	if err := m.Validate(Specs(), []string{"wardogs"}); err == nil || strings.Count(err.Error(), ErrInvalidManifest.Error()) != 2 {
		t.Errorf("two problems reported as %v", err)
	}
	m = base()
	if err := m.Validate(Specs(), nil); err == nil || !strings.Contains(err.Error(), "is not a driver this hub has") {
		t.Errorf("a hub without the driver = %v", err)
	}
}

func TestTighten(t *testing.T) {
	spec := Specs()["wardogs"]
	got := spec.Tighten([]Band{{Section: "MatchState.Playing.KOTH", Key: "ScorePeriod", Min: i64(21), Max: i64(27)}})
	sp := got[0]
	if *sp.Min != 21 || *sp.Max != 27 {
		t.Errorf("tightened = %+v", sp)
	}
	if b, _ := spec.Band("MatchState.Playing.KOTH", "ScorePeriod"); *b.Min != 18 {
		t.Error("Tighten changed the spec")
	}
}

// A feed survives the manifest's round trip, and a server without one has none.
func TestFeedRoundTrip(t *testing.T) {
	m, err := ParseManifest([]byte(goodManifest))
	if err != nil {
		t.Fatal(err)
	}
	if f := m.Servers[0].Feed; f == nil || f.URL != "https://ingest.example.com" || f.TokenFile != "/run/secrets/htg-wardogs-feed-token" {
		t.Fatalf("feed = %+v", f)
	}
	out, err := m.Normalized().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	again, err := ParseManifest(out)
	if err != nil || !reflect.DeepEqual(again.Normalized(), m.Normalized()) {
		t.Errorf("round trip: %v\n%s", err, out)
	}
	row, err := toRow(m.Servers[0])
	if err != nil {
		t.Fatal(err)
	}
	back, err := fromRow(row)
	if err != nil || !reflect.DeepEqual(back.Feed, m.Servers[0].Feed) {
		t.Errorf("row round trip: %+v %v", back.Feed, err)
	}
	m.Servers[0].Feed = nil
	if row, _ := toRow(m.Servers[0]); row.Feed != nil {
		t.Errorf("no feed stores %s", row.Feed)
	}
}
