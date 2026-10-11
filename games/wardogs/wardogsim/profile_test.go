package wardogsim

import (
	"math"
	"strings"
	"testing"
	"time"
)

const exampleProfile = `version: 1
timezone: America/Chicago
population: [4, 3, 2, 2, 1, 1, 2, 3, 5, 6, 8, 10, 12, 14, 16, 18, 22, 28, 34, 38, 36, 30, 20, 10]
regulars: 250
session_minutes: {min: 15, max: 90}
match_minutes: {min: 22, max: 30}
kills_per_minute: 0.5
skill_spread: 0.6
rejoin_rate: 0.1
restart_hour: 9
`

func TestDefaultProfileIsValid(t *testing.T) {
	if err := DefaultProfile().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestParseProfile(t *testing.T) {
	p, err := ParseProfileBytes([]byte(exampleProfile))
	if err != nil {
		t.Fatal(err)
	}
	if p.Regulars != 250 || p.MatchMinutes.Max != 30 || *p.RestartHour != 9 || p.Location().String() != "America/Chicago" {
		t.Errorf("parsed %+v", p)
	}
	noRestart, err := ParseProfileBytes([]byte(strings.Replace(exampleProfile, "restart_hour: 9\n", "", 1)))
	if err != nil || noRestart.RestartHour != nil || !noRestart.NextRestart(time.Now()).IsZero() {
		t.Errorf("a profile without restart_hour: %+v %v", noRestart, err)
	}
	for name, tc := range map[string]struct{ yaml, want string }{
		"unknown key":      {exampleProfile + "names: [Ana]\n", "field names not found"},
		"empty":            {"", "empty"},
		"version":          {strings.Replace(exampleProfile, "version: 1", "version: 2", 1), "version"},
		"short curve":      {strings.Replace(exampleProfile, "[4, 3, ", "[", 1), "population: 22 values"},
		"negative hour":    {strings.Replace(exampleProfile, "[4, 3,", "[-4, 3,", 1), "population[0]"},
		"too many players": {strings.Replace(exampleProfile, "regulars: 250", "regulars: 5000", 1), "regulars"},
		"backwards range":  {strings.Replace(exampleProfile, "{min: 22, max: 30}", "{min: 30, max: 22}", 1), "match_minutes"},
		"rejoin above one": {strings.Replace(exampleProfile, "rejoin_rate: 0.1", "rejoin_rate: 1.5", 1), "rejoin_rate"},
		"no such zone":     {strings.Replace(exampleProfile, "America/Chicago", "Mars/Olympus", 1), "timezone"},
		"bad hour":         {strings.Replace(exampleProfile, "restart_hour: 9", "restart_hour: 24", 1), "restart_hour"},
		"two documents":    {exampleProfile + "---\nversion: 1\n", "one YAML document"},
		"a name":           {strings.Replace(exampleProfile, "regulars: 250", "regulars: Ana", 1), "cannot unmarshal"},
	} {
		if _, err := ParseProfileBytes([]byte(tc.yaml)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

func TestPopulationAt(t *testing.T) {
	p := DefaultProfile()
	p.Timezone = "UTC"
	at := func(h, m int) float64 { return p.PopulationAt(time.Date(2026, 10, 11, h, m, 0, 0, time.UTC)) }
	if at(20, 0) != p.Population[20] || math.Abs(at(20, 30)-(p.Population[20]+p.Population[21])/2) > 1e-9 {
		t.Errorf("20:00 %v, 20:30 %v", at(20, 0), at(20, 30))
	}
	if math.Abs(at(23, 30)-(p.Population[23]+p.Population[0])/2) > 1e-9 {
		t.Errorf("23:30 wraps to midnight: %v", at(23, 30))
	}
	chicago := DefaultProfile()
	// 01:00 UTC is 20:00 in Chicago in October (CDT).
	if got := chicago.PopulationAt(time.Date(2026, 10, 12, 1, 0, 0, 0, time.UTC)); got != chicago.Population[20] {
		t.Errorf("the profile's hours are local: %v", got)
	}
}

func TestNextRestart(t *testing.T) {
	p := DefaultProfile() // 09:00 Chicago
	now := time.Date(2026, 10, 11, 13, 59, 0, 0, time.UTC)
	if got, want := p.NextRestart(now), time.Date(2026, 10, 11, 14, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("next restart %v, want %v", got, want)
	}
	if got, want := p.NextRestart(time.Date(2026, 10, 11, 14, 0, 0, 0, time.UTC)), time.Date(2026, 10, 12, 14, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("at the restart, the next is tomorrow's: %v", got)
	}
}

func TestStandIns(t *testing.T) {
	names, ids := map[string]bool{}, map[string]bool{}
	for n := 1; n <= MaxRegulars; n++ {
		id, name := SteamID(n), Name(n)
		if len(id) != 17 || id >= "76561197960265728" || names[name] || ids[id] {
			t.Fatalf("player %d: %s %q", n, id, name)
		}
		names[name], ids[id] = true, true
	}
	if Name(1) == Name(2) || SteamID(7) != "76561190000000007" {
		t.Errorf("Name(1) %q, SteamID(7) %s", Name(1), SteamID(7))
	}
}
