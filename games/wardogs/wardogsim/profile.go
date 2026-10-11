package wardogsim

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"time"
	_ "time/tzdata" // a profile's timezone resolves in a container with no zoneinfo

	"gopkg.in/yaml.v3"
)

// ProfileVersion is the calibration file format this package reads.
const ProfileVersion = 1

// MaxRegulars is the most distinct players a profile may draw from: the stand-in SteamIDs are
// three digits (SteamID).
const MaxRegulars = 999

// Profile is a calibration: the shapes a simulated server, and a seeded history, take from
// measurements of real War Dogs servers (gravel#112). It holds distributions only, never a
// player's name or SteamID, so a fresh measurement updates the demo without code and without
// carrying anyone's identity into it.
type Profile struct {
	Version int `yaml:"version"`
	// Timezone is where the hours of Population and RestartHour are read (IANA); "" is UTC.
	Timezone string `yaml:"timezone"`
	// Population is the players on, on average, in each hour of the day: 24 values, midnight
	// first. Between two hours the count moves linearly.
	Population []float64 `yaml:"population"`
	// Regulars is how many distinct players come and go (1–999); a few play much more than the rest.
	Regulars int `yaml:"regulars"`
	// SessionMinutes is how long a player stays once they join.
	SessionMinutes Range `yaml:"session_minutes"`
	// MatchMinutes is how long a match lasts before the rotation moves.
	MatchMinutes Range `yaml:"match_minutes"`
	// KillsPerMinute is a typical player's kills a minute while a match is on.
	KillsPerMinute float64 `yaml:"kills_per_minute"`
	// SkillSpread is how far players' kill rates spread around it: the sigma of a log-normal, so
	// 0 makes everyone alike and 0.6 puts the best players at several times the worst.
	SkillSpread float64 `yaml:"skill_spread"`
	// RejoinRate is the share of players who leave and come back to the same match a few
	// minutes later (a crash, a reconnect), with the game's counters restarted.
	RejoinRate float64 `yaml:"rejoin_rate"`
	// RestartHour is the hour of the day the host restarts the server (0–23), when it reads the
	// sections that apply at the next restart; nil never restarts.
	RestartHour *int `yaml:"restart_hour"`
}

// Range is a uniform range, Min to Max.
type Range struct {
	Min float64 `yaml:"min"`
	Max float64 `yaml:"max"`
}

func (r Range) draw(u float64) float64 { return r.Min + (r.Max-r.Min)*u }

// DefaultProfile is the calibration without a --profile: a North American community server that
// is quiet overnight and busy in the evening, Central time. The numbers are placeholders shaped
// like the CL-509546 lobby until HTG's own match_stats measure better ones.
func DefaultProfile() Profile {
	restart := 9
	return Profile{
		Version:        ProfileVersion,
		Timezone:       "America/Chicago",
		Population:     []float64{18, 12, 8, 5, 4, 3, 3, 4, 6, 8, 10, 12, 14, 16, 18, 22, 26, 32, 40, 48, 52, 46, 36, 26},
		Regulars:       400,
		SessionMinutes: Range{Min: 20, Max: 120},
		MatchMinutes:   Range{Min: 20, Max: 35},
		KillsPerMinute: 0.6,
		SkillSpread:    0.55,
		RejoinRate:     0.08,
		RestartHour:    &restart,
	}
}

// ParseProfile reads a calibration file strictly: an unknown key is an error, and so is a
// missing or out-of-range value.
func ParseProfile(r io.Reader) (Profile, error) {
	var p Profile
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		if errors.Is(err, io.EOF) {
			return Profile{}, errors.New("wardogsim: the profile is empty")
		}
		return Profile{}, fmt.Errorf("wardogsim: profile: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return Profile{}, errors.New("wardogsim: profile: one YAML document only")
	}
	if err := p.Validate(); err != nil {
		return Profile{}, err
	}
	return p, nil
}

// ParseProfileBytes is ParseProfile over a byte slice.
func ParseProfileBytes(b []byte) (Profile, error) { return ParseProfile(bytes.NewReader(b)) }

// Validate reports every problem at once.
func (p Profile) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if p.Version != ProfileVersion {
		bad("version: got %d, this build reads version %d", p.Version, ProfileVersion)
	}
	if _, err := time.LoadLocation(p.Timezone); err != nil {
		bad("timezone: %q is not an IANA zone", p.Timezone)
	}
	if len(p.Population) != 24 {
		bad("population: %d values, want 24 (one an hour, midnight first)", len(p.Population))
	}
	for i, v := range p.Population {
		if v < 0 || v > 1000 || math.IsNaN(v) {
			bad("population[%d]: %v is not 0–1000", i, v)
		}
	}
	if p.Regulars < 1 || p.Regulars > MaxRegulars {
		bad("regulars: %d is not 1–%d", p.Regulars, MaxRegulars)
	}
	checkRange := func(name string, r Range, lo, hi float64) {
		if r.Min < lo || r.Max > hi || r.Min > r.Max || math.IsNaN(r.Min) || math.IsNaN(r.Max) {
			bad("%s: min %v, max %v: want %v ≤ min ≤ max ≤ %v", name, r.Min, r.Max, lo, hi)
		}
	}
	checkRange("session_minutes", p.SessionMinutes, 0.05, 24*60)
	checkRange("match_minutes", p.MatchMinutes, 0.05, 6*60)
	if p.KillsPerMinute < 0 || p.KillsPerMinute > 20 || math.IsNaN(p.KillsPerMinute) {
		bad("kills_per_minute: %v is not 0–20", p.KillsPerMinute)
	}
	if p.SkillSpread < 0 || p.SkillSpread > 2 || math.IsNaN(p.SkillSpread) {
		bad("skill_spread: %v is not 0–2", p.SkillSpread)
	}
	if p.RejoinRate < 0 || p.RejoinRate > 1 || math.IsNaN(p.RejoinRate) {
		bad("rejoin_rate: %v is not 0–1", p.RejoinRate)
	}
	if h := p.RestartHour; h != nil && (*h < 0 || *h > 23) {
		bad("restart_hour: %d is not 0–23", *h)
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("wardogsim: invalid profile:\n%w", errors.Join(errs...))
}

// Location is the profile's timezone (UTC when unset or unknown).
func (p Profile) Location() *time.Location {
	loc, err := time.LoadLocation(p.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// PopulationAt is the average number of players on at t: the profile's hourly values, moving
// linearly from one hour to the next.
func (p Profile) PopulationAt(t time.Time) float64 {
	if len(p.Population) != 24 {
		return 0
	}
	l := t.In(p.Location())
	h := float64(l.Hour()) + float64(l.Minute())/60 + float64(l.Second())/3600
	i := int(h) % 24
	f := h - math.Floor(h)
	return p.Population[i]*(1-f) + p.Population[(i+1)%24]*f
}

// NextRestart is the first restart after t, or the zero time when the server never restarts.
func (p Profile) NextRestart(t time.Time) time.Time {
	if p.RestartHour == nil {
		return time.Time{}
	}
	l := t.In(p.Location())
	r := time.Date(l.Year(), l.Month(), l.Day(), *p.RestartHour, 0, 0, 0, l.Location())
	for !r.After(t) {
		r = r.AddDate(0, 0, 1)
	}
	return r
}
