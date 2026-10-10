package servers

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gravel-project/gravel/games/wardogs"
	"github.com/gravel-project/gravel/games/wardogs/wardogstest"
)

// Every shipped spec parses, the catalog entries have no driver, and each carries its
// publisher's rules (gravel#8).
func TestCatalogSpecs(t *testing.T) {
	specs := Specs()
	for _, c := range []struct {
		id, name, provider string
		noPaidPerks        bool
	}{
		{"cs2", "Counter-Strike 2", "steam", true},
		{"seaofthieves", "Sea of Thieves", "xbox", true},
		{"starcitizen", "Star Citizen", "rsi", true},
		{"pubg", "PUBG", "pubg", true},
		{"wardogs", "War Dogs", "steam", false},
	} {
		s, ok := specs[c.id]
		if !ok || s.Name != c.name || s.IdentityProvider != c.provider || s.NoPaidPerks != c.noPaidPerks {
			t.Errorf("%s spec = %+v", c.id, s)
		}
		if c.id != "wardogs" && len(s.Drivers) != 0 {
			t.Errorf("%s has drivers %v; it is a catalog entry", c.id, s.Drivers)
		}
	}
	if len(specs["cs2"].Plugins.Deny) == 0 {
		t.Error("cs2 ships no plugin denylist")
	}
}

// Each War Dogs band names a section and key the newest recorded build honours, so a band can't
// sit on a name the server doesn't use and silently check nothing.
func TestWarDogsBandsAreInTheServersSchema(t *testing.T) {
	builds := wardogstest.Builds(t, filepath.Join("..", "..", "games", "wardogs", "testdata"))
	if len(builds) == 0 {
		t.Fatal("no recorded War Dogs builds")
	}
	slices.Sort(builds)
	raw, err := os.ReadFile(filepath.Join(builds[len(builds)-1], "v1", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc wardogs.ConfigDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	spec := Specs()["wardogs"]
	if len(spec.Bands) < 5 {
		t.Fatalf("War Dogs bands = %+v", spec.Bands)
	}
	for _, b := range spec.Bands {
		if _, ok := doc.Key(b.Section, b.Key); !ok {
			t.Errorf("band %s %s: %s does not honour that key", b.Section, b.Key, filepath.Base(builds[len(builds)-1]))
		}
		if b.UnlessSet != "" {
			if _, ok := doc.Key(b.Section, b.UnlessSet); !ok {
				t.Errorf("band %s %s: unless_set %s is not a key of the section", b.Section, b.Key, b.UnlessSet)
			}
		}
	}
	kinds := map[string]bool{}
	for _, b := range spec.Bands {
		kinds[b.Kind()] = true
	}
	if !kinds[BandRange] || !kinds[BandLength] || !kinds[BandHosts] {
		t.Errorf("War Dogs band kinds = %v", kinds)
	}
}

func TestParseSpecRefusesMalformedRules(t *testing.T) {
	const head = "id: x\nname: X\nidentity_provider: steam\ndrivers: []\n"
	for name, c := range map[string]struct{ yaml, want string }{
		"no kind":       {"bands:\n  - {section: S, key: K}\n", "set exactly one of min/max, max_length or hosts"},
		"two kinds":     {"bands:\n  - {section: S, key: K, max: 3, max_length: 4}\n", "set exactly one of"},
		"crossed":       {"bands:\n  - {section: S, key: K, min: 4, max: 3}\n", "min is above max"},
		"zero length":   {"bands:\n  - {section: S, key: K, max_length: 0}\n", "max_length must be at least 1"},
		"bad host":      {"bands:\n  - {section: S, key: K, hosts: [Example.com]}\n", `host "Example.com" must be a lower-case domain name`},
		"url host":      {"bands:\n  - {section: S, key: K, hosts: [\"https://example.com\"]}\n", "must be a lower-case domain name"},
		"no section":    {"bands:\n  - {key: K, max: 3}\n", "section and key are required"},
		"twice":         {"bands:\n  - {section: S, key: K, max: 3}\n  - {section: S, key: K, max: 4}\n", "listed twice"},
		"deny reason":   {"plugins:\n  deny:\n    - {match: skin}\n", "plugins.deny[0]: match (with a letter or digit) and reason are required"},
		"deny match":    {"plugins:\n  deny:\n    - {match: \"--\", reason: r}\n", "plugins.deny[0]"},
		"allow symbols": {"plugins:\n  allow: [\"!!\"]\n", "plugins.allow[0]"},
		"unknown key":   {"policy: strict\n", "field policy not found"},
	} {
		_, err := ParseSpec([]byte(head + c.yaml))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
	if _, err := ParseSpec([]byte("id: x\nname: X\ndrivers: []\n")); err == nil {
		t.Error("a spec without identity_provider parsed")
	}
	// No drivers is a catalog entry, and well formed.
	if s, err := ParseSpec([]byte(head)); err != nil || len(s.Drivers) != 0 {
		t.Errorf("catalog entry = %+v, %v", s, err)
	}
}

func TestCheckPlugins(t *testing.T) {
	cs2 := Specs()["cs2"]
	if err := cs2.CheckPlugins([]string{"Metamod:Source", "CounterStrikeSharp", "MatchZy", "CS2-SimpleAdmin"}); err != nil {
		t.Errorf("the pinned plugin set was refused: %v", err)
	}
	err := cs2.CheckPlugins([]string{"MatchZy", "cs2-WeaponPaints", "CS2_Gloves", "fake-rank-display", "Music Kits"})
	var pe *PluginError
	if !errors.As(err, &pe) || !errors.Is(err, ErrPluginRefused) {
		t.Fatalf("err = %v", err)
	}
	var names []string
	for _, r := range pe.Refused {
		names = append(names, r.Name)
		if !strings.Contains(r.Reason, "Valve server guidelines") {
			t.Errorf("%s refused without the publisher's reason: %q", r.Name, r.Reason)
		}
	}
	if !slices.Equal(names, []string{"cs2-WeaponPaints", "CS2_Gloves", "fake-rank-display", "Music Kits"}) {
		t.Errorf("refused = %v", names)
	}
	if !strings.Contains(err.Error(), "Counter-Strike 2 does not allow these plugins: cs2-WeaponPaints (weapon skins") {
		t.Errorf("message = %v", err)
	}

	// An allow list permits only its names; the denylist still wins over it.
	s, err := ParseSpec([]byte("id: x\nname: X\nidentity_provider: steam\nplugins:\n  deny:\n    - {match: skin, reason: no skins}\n  allow: [MatchZy, SkinChanger]\n"))
	if err != nil {
		t.Fatal(err)
	}
	err = s.CheckPlugins([]string{"matchzy", "Retakes", "SkinChanger"})
	if !errors.As(err, &pe) || len(pe.Refused) != 2 || pe.Refused[0] != (RefusedPlugin{Name: "Retakes", Reason: "not on the allow list"}) ||
		pe.Refused[1] != (RefusedPlugin{Name: "SkinChanger", Reason: "no skins"}) {
		t.Errorf("allow list: %v", err)
	}
	// A game without a policy allows anything.
	if err := Specs()["wardogs"].CheckPlugins([]string{"anything"}); err != nil {
		t.Errorf("wardogs: %v", err)
	}
}

func TestTightenNewKinds(t *testing.T) {
	spec := Specs()["wardogs"]
	const gs = "/Script/WDGame.WDGameSession"
	got := spec.Tighten([]Band{{Section: gs, Key: "ServerName", MaxLength: i64(32)}, {Section: gs, Key: "ServerImageURL", Hosts: []string{"i.ibb.co"}}})
	for _, b := range got {
		switch b.Key {
		case "ServerName":
			if *b.MaxLength != 32 {
				t.Errorf("ServerName = %+v", b)
			}
		case "ServerImageURL":
			if !slices.Equal(b.Hosts, []string{"i.ibb.co"}) {
				t.Errorf("ServerImageURL = %+v", b)
			}
		case "MaxReservedSlots":
			if b.UnlessSet != "DefaultReservedPlayerIds" || *b.Max != 0 {
				t.Errorf("MaxReservedSlots = %+v", b)
			}
		}
	}
	if b, _ := spec.Band(gs, "ServerImageURL"); len(b.Hosts) != 4 {
		t.Error("Tighten changed the spec")
	}
	// A tightened subdomain is accepted by the manifest.
	m, err := ParseManifest([]byte(goodManifest))
	if err != nil {
		t.Fatal(err)
	}
	m.Games[0].Bands = append(m.Games[0].Bands, Band{Section: gs, Key: "ServerImageURL", Hosts: []string{"i.ibb.co"}}, Band{Section: gs, Key: "ServerName", MaxLength: i64(40)})
	if err := m.Validate(Specs(), []string{"wardogs"}); err != nil {
		t.Errorf("tightenings refused: %v", err)
	}
}
