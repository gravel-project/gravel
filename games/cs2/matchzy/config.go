package matchzy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The values of a config's map_sides entries.
const (
	MapSideTeam1CT = "team1_ct"
	MapSideTeam1T  = "team1_t"
	MapSideTeam2CT = "team2_ct"
	MapSideTeam2T  = "team2_t"
	MapSideKnife   = "knife"
)

// The values of a config's side_type: how the sides are chosen on a map with no map_sides entry.
const (
	SideTypeStandard    = "standard"
	SideTypeAlwaysKnife = "always_knife"
	SideTypeNeverKnife  = "never_knife"
	SideTypeRandom      = "random"
)

// The values of a config's veto_first.
const (
	VetoFirstTeam1  = "team1"
	VetoFirstTeam2  = "team2"
	VetoFirstRandom = "random"
)

// The steps of a config's veto_mode.
const (
	VetoTeam1Ban  = "team1_ban"
	VetoTeam2Ban  = "team2_ban"
	VetoTeam1Pick = "team1_pick"
	VetoTeam2Pick = "team2_pick"
)

// Config is a match config as matchzy_loadmatch reads it (LoadMatchFromJSON and
// ValidateMatchJsonStructure in MatchManagement.cs). A zero optional field is left out of the
// JSON, so MatchZy uses its own default for it.
type Config struct {
	// MatchID is the config's matchid, which every event carries back. MatchZy takes 0 to
	// 2147483647 (its MySQL tables keep it as an INT).
	MatchID int64
	Team1   ConfigTeam
	Team2   ConfigTeam
	// NumMaps is the series length.
	NumMaps int
	// Maplist is the map pool: played in order when it has NumMaps maps or SkipVeto is set,
	// vetoed by the teams when it has more.
	Maplist []string
	// MapSides is each map's starting sides (the MapSide constants); a map without one follows
	// SideType. When given, it has at least NumMaps entries.
	MapSides []string
	// SkipVeto plays the first NumMaps maps of a larger pool in order instead of vetoing them.
	SkipVeto bool
	// VetoFirst is the team that starts the default veto (the VetoFirst constants); empty is team1.
	VetoFirst string
	// VetoMode is the veto's steps, used as written instead of the default order.
	VetoMode []string
	// SideType is the SideType constants; empty is standard (a knife round).
	SideType string
	// PlayersPerTeam is empty at 0, which MatchZy reads as 5.
	PlayersPerTeam int
	// MinPlayersToReady is empty at 0, which leaves the server's matchzy_minimum_ready_required.
	MinPlayersToReady int
	// PlayAllMaps keeps playing a series after one team can no longer be caught (clinch_series
	// false). MatchZy stops at the clinch by default.
	PlayAllMaps bool
	// Wingman plays 2v2 wingman.
	Wingman bool
	// Spectators maps a SteamID64 to a name, as ConfigTeam.Players does: the players who may
	// join as spectators. A loaded match kicks anyone on no team and not a spectator.
	Spectators map[string]string
	// Cvars are set when the match loads: real convars or MatchZy settings, with plain values.
	Cvars map[string]string
}

// ConfigTeam is one of a config's two teams.
type ConfigTeam struct {
	// ID is echoed in the events' team objects; MatchZy needs none.
	ID string
	// Name is shown in game and in the events. MatchZy drops every character but letters, digits,
	// spaces, '_' and '-', so Validate refuses a name with any other, so the name the hub set is
	// the name the events carry.
	Name string
	// Players maps each player's SteamID64 to a name. A non-empty name is forced on the player in
	// game (sv_load_forced_client_names_file); an empty one leaves the player's own. MatchZy 0.9.1
	// kicks anyone who joins a loaded match and is on no team: an empty Players is valid JSON to
	// MatchZy, but nobody can then play until they are added with matchzy_addplayer.
	Players map[string]string
}

var (
	mapNameRE  = regexp.MustCompile(`^[a-z0-9_]+$`)
	steamIDRE  = regexp.MustCompile(`^[0-9]{17}$`)
	teamNameRE = regexp.MustCompile(`^[\p{L}0-9 _-]+$`)
	cvarNameRE = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
)

// blockedCvars are refused by MatchZy in a config's cvars (MatchZySecurity.BlockedMatchCvars).
var blockedCvars = []string{"rcon_password", "matchzy_everyone_is_admin"}

// Validate reports every way the config breaks what MatchZy accepts, or what the hub needs from
// it, joined into one error; nil when there is none. It is stricter than MatchZy where MatchZy
// would quietly change or drop a value: a team name it would strip, a cvar it would ignore.
// MatchZy also checks that a cvar names a real convar or one of its settings; that needs the
// server, so it is left to MatchZy, which logs and skips a cvar it refuses.
func (c Config) Validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if c.MatchID < 0 || c.MatchID > math.MaxInt32 {
		bad("matchid %d: must be 0 to %d", c.MatchID, math.MaxInt32)
	}
	if c.NumMaps < 1 || c.NumMaps%2 == 0 {
		bad("num_maps %d: must be odd (1, 3, 5, ...)", c.NumMaps)
	}
	if len(c.Maplist) == 0 {
		bad("maplist: must have at least one map")
	} else if len(c.Maplist) < c.NumMaps {
		bad("maplist: %d maps, fewer than num_maps %d", len(c.Maplist), c.NumMaps)
	}
	for i, m := range c.Maplist {
		if !mapNameRE.MatchString(m) {
			bad("maplist[%d] %q: must be lower-case letters, digits and '_'", i, m)
		}
	}
	if len(c.MapSides) > 0 && len(c.MapSides) < c.NumMaps {
		bad("map_sides: %d entries, fewer than num_maps %d", len(c.MapSides), c.NumMaps)
	}
	for i, s := range c.MapSides {
		if !slices.Contains([]string{MapSideTeam1CT, MapSideTeam1T, MapSideTeam2CT, MapSideTeam2T, MapSideKnife}, s) {
			bad("map_sides[%d] %q: unknown side", i, s)
		}
	}
	if c.VetoFirst != "" && !slices.Contains([]string{VetoFirstTeam1, VetoFirstTeam2, VetoFirstRandom}, c.VetoFirst) {
		bad("veto_first %q: must be team1, team2 or random", c.VetoFirst)
	}
	for i, s := range c.VetoMode {
		if !slices.Contains([]string{VetoTeam1Ban, VetoTeam2Ban, VetoTeam1Pick, VetoTeam2Pick}, s) {
			bad("veto_mode[%d] %q: unknown step", i, s)
		}
	}
	if c.SideType != "" && !slices.Contains([]string{SideTypeStandard, SideTypeAlwaysKnife, SideTypeNeverKnife, SideTypeRandom}, c.SideType) {
		bad("side_type %q: unknown", c.SideType)
	}
	if c.PlayersPerTeam < 0 {
		bad("players_per_team %d: must not be negative", c.PlayersPerTeam)
	}
	if c.MinPlayersToReady < 0 {
		bad("min_players_to_ready %d: must not be negative", c.MinPlayersToReady)
	}

	seen := map[string]string{}
	roster := func(where string, players map[string]string) {
		for _, id := range sortedKeys(players) {
			if !steamIDRE.MatchString(id) {
				bad("%s %q: not a SteamID64 (17 digits)", where, id)
			}
			if other, ok := seen[id]; ok {
				bad("%s %q: also in %s", where, id, other)
			}
			seen[id] = where
			if strings.ContainsAny(players[id], "\"\\\r\n") {
				bad("%s %q: name has a quote, a backslash or a line break", where, id)
			}
		}
	}
	for _, t := range []struct {
		key  string
		team ConfigTeam
	}{{"team1", c.Team1}, {"team2", c.Team2}} {
		if strings.TrimSpace(t.team.Name) == "" {
			bad("%s.name: must not be empty", t.key)
		} else if !teamNameRE.MatchString(t.team.Name) {
			bad("%s.name %q: only letters, digits, spaces, '_' and '-' (MatchZy drops the rest)", t.key, t.team.Name)
		}
		roster(t.key+".players", t.team.Players)
	}
	roster("spectators.players", c.Spectators)

	for _, k := range sortedKeys(c.Cvars) {
		switch {
		case !cvarNameRE.MatchString(k):
			bad("cvars %q: a name is letters, digits and '_'", k)
		case slices.ContainsFunc(blockedCvars, func(b string) bool { return strings.EqualFold(b, k) }):
			bad("cvars %q: MatchZy never sets it from a config", k)
		}
		if strings.ContainsAny(c.Cvars[k], "\";\r\n") {
			bad("cvars %q: value has a quote, ';' or a line break", k)
		}
	}
	return errors.Join(errs...)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

type configJSON struct {
	MatchID           int64             `json:"matchid"`
	Team1             teamJSON          `json:"team1"`
	Team2             teamJSON          `json:"team2"`
	NumMaps           int               `json:"num_maps"`
	Maplist           []string          `json:"maplist"`
	MapSides          []string          `json:"map_sides,omitempty"`
	SkipVeto          bool              `json:"skip_veto,omitempty"`
	VetoFirst         string            `json:"veto_first,omitempty"`
	VetoMode          []string          `json:"veto_mode,omitempty"`
	SideType          string            `json:"side_type,omitempty"`
	PlayersPerTeam    int               `json:"players_per_team,omitempty"`
	MinPlayersToReady int               `json:"min_players_to_ready,omitempty"`
	ClinchSeries      *bool             `json:"clinch_series,omitempty"`
	Wingman           bool              `json:"wingman,omitempty"`
	Spectators        *spectatorsJSON   `json:"spectators,omitempty"`
	Cvars             map[string]string `json:"cvars,omitempty"`
}

type teamJSON struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name"`
	// Players is always written, as an object even when empty: MatchZy refuses a team without
	// "players".
	Players map[string]string `json:"players"`
}

type spectatorsJSON struct {
	Players map[string]string `json:"players"`
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// MarshalJSON writes the config as MatchZy reads it: matchid as a number, players as an object of
// SteamID64 to name (empty when the team has no list), the optional fields only when set. It
// does not validate; call Validate first.
func (c Config) MarshalJSON() ([]byte, error) {
	j := configJSON{
		MatchID:           c.MatchID,
		Team1:             teamJSON{ID: c.Team1.ID, Name: c.Team1.Name, Players: nonNil(c.Team1.Players)},
		Team2:             teamJSON{ID: c.Team2.ID, Name: c.Team2.Name, Players: nonNil(c.Team2.Players)},
		NumMaps:           c.NumMaps,
		Maplist:           c.Maplist,
		MapSides:          c.MapSides,
		SkipVeto:          c.SkipVeto,
		VetoFirst:         c.VetoFirst,
		VetoMode:          c.VetoMode,
		SideType:          c.SideType,
		PlayersPerTeam:    c.PlayersPerTeam,
		MinPlayersToReady: c.MinPlayersToReady,
		Wingman:           c.Wingman,
		Cvars:             c.Cvars,
	}
	if j.Maplist == nil {
		j.Maplist = []string{}
	}
	if c.PlayAllMaps {
		clinch := false
		j.ClinchSeries = &clinch
	}
	if len(c.Spectators) > 0 {
		j.Spectators = &spectatorsJSON{Players: c.Spectators}
	}
	return json.Marshal(j)
}

// UnmarshalJSON reads a config the way MatchZy does, so a config written by hand or by Get5's
// tools reads too: matchid as a number or a string, players as an object or as an array of
// SteamID64s (strings or numbers, read without a name), the booleans as true/false, 1/0 or those
// as strings, and the cvars' values as any JSON scalar. Like MatchZy, it ignores fields it does
// not know.
func (c *Config) UnmarshalJSON(b []byte) error {
	var j struct {
		MatchID           json.RawMessage `json:"matchid"`
		Team1             rawTeam         `json:"team1"`
		Team2             rawTeam         `json:"team2"`
		NumMaps           json.RawMessage `json:"num_maps"`
		Maplist           []string        `json:"maplist"`
		MapSides          []string        `json:"map_sides"`
		SkipVeto          json.RawMessage `json:"skip_veto"`
		VetoFirst         string          `json:"veto_first"`
		VetoMode          []string        `json:"veto_mode"`
		SideType          string          `json:"side_type"`
		PlayersPerTeam    json.RawMessage `json:"players_per_team"`
		MinPlayersToReady json.RawMessage `json:"min_players_to_ready"`
		ClinchSeries      json.RawMessage `json:"clinch_series"`
		Wingman           json.RawMessage `json:"wingman"`
		Spectators        *struct {
			Players rawRoster `json:"players"`
		} `json:"spectators"`
		Cvars map[string]json.RawMessage `json:"cvars"`
	}
	if err := json.Unmarshal(b, &j); err != nil {
		return fmt.Errorf("matchzy: config: %w", err)
	}
	var out Config
	var err error
	field := func(name string, raw json.RawMessage, read func(json.RawMessage) error) {
		if err == nil && len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
			if e := read(raw); e != nil {
				err = fmt.Errorf("matchzy: config: %s: %w", name, e)
			}
		}
	}
	field("matchid", j.MatchID, func(r json.RawMessage) (e error) { out.MatchID, e = readInt(r); return })
	field("num_maps", j.NumMaps, func(r json.RawMessage) error { return readIntInto(r, &out.NumMaps) })
	field("players_per_team", j.PlayersPerTeam, func(r json.RawMessage) error { return readIntInto(r, &out.PlayersPerTeam) })
	field("min_players_to_ready", j.MinPlayersToReady, func(r json.RawMessage) error { return readIntInto(r, &out.MinPlayersToReady) })
	field("skip_veto", j.SkipVeto, func(r json.RawMessage) (e error) { out.SkipVeto, e = readBool(r); return })
	field("wingman", j.Wingman, func(r json.RawMessage) (e error) { out.Wingman, e = readBool(r); return })
	field("clinch_series", j.ClinchSeries, func(r json.RawMessage) error {
		clinch, e := readBool(r)
		out.PlayAllMaps = !clinch
		return e
	})
	if err != nil {
		return err
	}
	out.Team1 = ConfigTeam{ID: j.Team1.ID.String(), Name: j.Team1.Name, Players: nilIfEmpty(j.Team1.Players)}
	out.Team2 = ConfigTeam{ID: j.Team2.ID.String(), Name: j.Team2.Name, Players: nilIfEmpty(j.Team2.Players)}
	out.Maplist, out.MapSides, out.VetoMode = j.Maplist, j.MapSides, j.VetoMode
	out.VetoFirst = strings.ToLower(strings.TrimSpace(j.VetoFirst))
	out.SideType = strings.ToLower(strings.TrimSpace(j.SideType))
	if j.Spectators != nil {
		out.Spectators = nilIfEmpty(j.Spectators.Players)
	}
	for k, raw := range j.Cvars {
		v, e := scalarString(raw)
		if e != nil {
			return fmt.Errorf("matchzy: config: cvars %q: %w", k, e)
		}
		if out.Cvars == nil {
			out.Cvars = map[string]string{}
		}
		out.Cvars[k] = v
	}
	*c = out
	return nil
}

// nilIfEmpty reads an empty roster as no list, the way Config writes one.
func nilIfEmpty(r rawRoster) map[string]string {
	if len(r) == 0 {
		return nil
	}
	return r
}

// rawTeam reads a team's id, which MatchZy takes with ToString() and so may be any scalar.
type rawTeam struct {
	ID      scalar    `json:"id"`
	Name    string    `json:"name"`
	Players rawRoster `json:"players"`
}

type scalar string

func (s scalar) String() string { return string(s) }

func (s *scalar) UnmarshalJSON(b []byte) error {
	v, err := scalarString(b)
	*s = scalar(v)
	return err
}

// rawRoster reads a roster as MatchZy's MatchConfigJson.NormalizeRoster does: an object of
// SteamID64 to name, or an array of SteamID64s read with empty names.
type rawRoster map[string]string

func (r *rawRoster) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("null")) {
		return nil
	}
	out := map[string]string{}
	if len(b) > 0 && b[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(b, &items); err != nil {
			return err
		}
		for _, item := range items {
			id, err := scalarString(item)
			if err != nil {
				return fmt.Errorf("players: %w", err)
			}
			if id = strings.TrimSpace(id); id != "" {
				out[id] = ""
			}
		}
	} else {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			return fmt.Errorf("players: must be an object or an array: %w", err)
		}
		for id, raw := range m {
			name, err := scalarString(raw)
			if err != nil {
				return fmt.Errorf("players %q: %w", id, err)
			}
			out[id] = name
		}
	}
	*r = out
	return nil
}

// scalarString is a JSON string's value, or a number's or a boolean's text as written, so a
// SteamID64 written as a number keeps every digit.
func scalarString(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	switch {
	case len(raw) == 0 || bytes.Equal(raw, []byte("null")):
		return "", nil
	case raw[0] == '"':
		var s string
		err := json.Unmarshal(raw, &s)
		return s, err
	case raw[0] == '{' || raw[0] == '[':
		return "", errors.New("must be a string, a number or a boolean")
	default:
		var n json.Number
		if err := json.Unmarshal(raw, &n); err == nil {
			return n.String(), nil
		}
		var v bool
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", err
		}
		return strconv.FormatBool(v), nil
	}
}

func readInt(raw json.RawMessage) (int64, error) {
	s, err := scalarString(raw)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("not an integer: %q", s)
	}
	return n, nil
}

func readIntInto(raw json.RawMessage, dst *int) error {
	n, err := readInt(raw)
	if err != nil {
		return err
	}
	if n < math.MinInt32 || n > math.MaxInt32 {
		return fmt.Errorf("%d is out of range", n)
	}
	*dst = int(n)
	return nil
}

// readBool takes what MatchZy's MatchConfigJson.TryParseBool takes: true or false, 1 or 0, and
// those as strings in any case.
func readBool(raw json.RawMessage) (bool, error) {
	s, err := scalarString(raw)
	if err != nil {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1":
		return true, nil
	case "false", "0":
		return false, nil
	}
	return false, fmt.Errorf("not a boolean: %q", s)
}
