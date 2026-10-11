package wardogsim

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/gravel-project/gravel/games/wardogs"
)

// The configuration document's sections and keys the simulator acts on, as CL-509546 names them.
const (
	secRCON       = "/Script/WDRCON.WDRCONSettings"
	secSession    = "/Script/WDGame.WDGameSession"
	secEngine     = "/Script/Engine.GameSession"
	secTeams      = "/Script/WDGame.WDGameStateSession"
	secRotation   = "/Script/WDGame.WDServerMapRotationSettings"
	secKOTH       = "MatchState.Playing.KOTH"
	secPreMatch   = "MatchState.PreMatch.WaitingForPlayers.PlayerCount"
	secFeed       = "WDServerFeed"
	keyBanned     = "DefaultBannedPlayerIds"
	keyReserved   = "DefaultReservedPlayerIds"
	keyRotation   = "RotationEntries"
	defaultMaxPls = 100
)

// The score period's band, which the server enforces itself (game.yaml).
const (
	scorePeriodMin = 18
	scorePeriodMax = 30
)

// imageHosts are the hosts a sponsor banner may come from (Bulkhead's allow-list, game.yaml).
var imageHosts = []string{"catbox.moe", "imgbb.com", "ibb.co", "postimg.cc"}

// settings are the values the simulation reads from a configuration document.
type settings struct {
	serverName     string
	imageURL       string
	maxPlayers     int
	reservedSlots  int
	reserved       []string
	banned         []string
	scorePeriod    int
	minPlayers     int
	rotationOn     bool
	rotationRandom bool
	rotation       []rotationEntry
	feedURL        string
	feedToken      string
}

// rotationEntry is one RotationEntries line.
type rotationEntry struct {
	Map, Lighting, Alternator string
	Experiences               []string
}

// revisionOf is a document's revision: the first twelve hex digits of its content's SHA-256, the
// length the server's revisions have.
func revisionOf(text string) string {
	sum := sha256.Sum256([]byte(wardogs.NormalizeEOL(text)))
	return hex.EncodeToString(sum[:])[:12]
}

// read takes the settings out of a document; a value it cannot read keeps the default.
func read(d wardogs.Doc) settings {
	s := settings{maxPlayers: defaultMaxPls, scorePeriod: 24, minPlayers: 20, rotationOn: true}
	str := func(sec, key string) string {
		if x := d.Section(sec); x != nil {
			v, _ := x.Value(key)
			return unquote(v)
		}
		return ""
	}
	num := func(sec, key string, def int) int {
		if n, err := strconv.Atoi(str(sec, key)); err == nil {
			return n
		}
		return def
	}
	s.serverName = str(secSession, "ServerName")
	s.imageURL = str(secSession, "ServerImageURL")
	s.maxPlayers = num(secEngine, "MaxPlayers", defaultMaxPls)
	s.reservedSlots = num(secSession, "MaxReservedSlots", 0)
	s.scorePeriod = num(secKOTH, "ScorePeriod", 24)
	s.minPlayers = num(secPreMatch, "MinimumRequiredPlayers", 20)
	if x := d.Section(secSession); x != nil {
		s.banned, s.reserved = x.Array(keyBanned), x.Array(keyReserved)
	}
	if x := d.Section(secRotation); x != nil {
		if v, ok := x.Value("bEnabled"); ok {
			s.rotationOn = strings.EqualFold(v, "true")
		}
		v, _ := x.Value("RotationMode")
		s.rotationRandom = strings.EqualFold(v, "random")
		for _, raw := range x.Array(keyRotation) {
			if e, err := parseRotationEntry(raw); err == nil {
				s.rotation = append(s.rotation, e)
			}
		}
	}
	s.feedURL, s.feedToken = str(secFeed, "Url"), str(secFeed, "Token")
	return s
}

// parseRotationEntry reads `(Map="Europe", Experiences="Madrid_KOTH_01", Lighting="DayClear",
// ZoneAlternator="ZoneAlternator.Ozeti.Default.Circle")`; Experiences may also be a list in
// parentheses.
func parseRotationEntry(raw string) (rotationEntry, error) {
	s := strings.TrimSpace(raw)
	if len(s) < 2 || s[0] != '(' || s[len(s)-1] != ')' {
		return rotationEntry{}, fmt.Errorf("%q is not (Key=Value, …)", raw)
	}
	var e rotationEntry
	for _, field := range splitTop(s[1 : len(s)-1]) {
		k, v, ok := strings.Cut(field, "=")
		if !ok {
			return rotationEntry{}, fmt.Errorf("%q: %q has no value", raw, field)
		}
		v = strings.TrimSpace(v)
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "map":
			e.Map = unquote(v)
		case "lighting":
			e.Lighting = unquote(v)
		case "zonealternator":
			e.Alternator = unquote(v)
		case "experiences":
			if strings.HasPrefix(v, "(") && strings.HasSuffix(v, ")") {
				for _, x := range splitTop(v[1 : len(v)-1]) {
					if x = unquote(strings.TrimSpace(x)); x != "" {
						e.Experiences = append(e.Experiences, x)
					}
				}
			} else if x := unquote(v); x != "" {
				e.Experiences = []string{x}
			}
		}
	}
	if e.Map == "" {
		return rotationEntry{}, fmt.Errorf("%q names no Map", raw)
	}
	return e, nil
}

// splitTop splits on the commas outside quotes and parentheses.
func splitTop(s string) []string {
	var out []string
	depth, quoted, start := 0, false, 0
	for i, c := range s {
		switch {
		case c == '"':
			quoted = !quoted
		case quoted:
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			out = append(out, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	if rest := strings.TrimSpace(s[start:]); rest != "" {
		out = append(out, rest)
	}
	return out
}

func unquote(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		return v[1 : len(v)-1]
	}
	return v
}

// level is the map's name in play, which the status reports: the zone alternator's second part
// ("ZoneAlternator.Ozeti.Default.Circle" is Ozeti), or the catalog's id when there is none.
func (e rotationEntry) level() string {
	parts := strings.Split(e.Alternator, ".")
	if len(parts) >= 2 && parts[1] != "" {
		return parts[1]
	}
	return e.Map
}

// checked is what checking a submitted document found: the document as the server would keep it
// and the answer's lists.
type checked struct {
	doc      wardogs.Doc
	errors   []wardogs.KeyError
	stripped []wardogs.Stripped
	warnings []string
}

// check reads a submitted document against the schema and the catalog, as the server does: a
// section or key the schema does not honour is stripped; a value the server cannot take is an
// error; a key that something else pins keeps the current value, with a warning.
func (s *Server) check(text string, cur wardogs.Doc) checked {
	var c checked
	in := wardogs.ParseDoc(text)
	c.doc.Preamble = in.Preamble
	for _, sec := range in.Sections {
		schema, ok := s.schema(sec.Name)
		if !ok {
			c.stripped = append(c.stripped, wardogs.Stripped{Section: sec.Name, Reason: "unknown_section"})
			continue
		}
		out := wardogs.DocSection{Name: sec.Name}
		for _, l := range sec.Lines {
			if l.Key == "" {
				out.Lines = append(out.Lines, l)
				continue
			}
			if !slices.ContainsFunc(schema.AllowedKeys, func(k string) bool { return strings.EqualFold(k, l.Key) }) {
				c.stripped = append(c.stripped, wardogs.Stripped{Section: sec.Name, Key: l.Key, Reason: "unknown_key"})
				continue
			}
			out.Lines = append(out.Lines, l)
		}
		c.doc.Sections = append(c.doc.Sections, out)
	}
	// Pinned keys keep what the server has.
	for _, sec := range s.sections {
		for _, o := range sec.KeyOverrides {
			if o.Writable == nil || *o.Writable {
				continue
			}
			want, have := "", ""
			if x := c.doc.Section(sec.Section); x != nil {
				want, _ = x.Value(o.Key)
			}
			if x := cur.Section(sec.Section); x != nil {
				have, _ = x.Value(o.Key)
			}
			if want == have {
				continue
			}
			c.warnings = append(c.warnings, fmt.Sprintf("%s %s is pinned by %s; the value in force is kept", sec.Section, o.Key, o.LockedBy))
			if x := c.doc.Section(sec.Section); x != nil {
				setValue(x, o.Key, have)
			}
		}
	}
	bad := func(sec, key, code, format string, a ...any) {
		c.errors = append(c.errors, wardogs.KeyError{Section: sec, Key: key, Code: code, Message: fmt.Sprintf(format, a...)})
	}
	for _, sec := range c.doc.Sections {
		for _, l := range sec.Lines {
			if l.Key == "" {
				continue
			}
			v := unquote(l.Value)
			switch {
			case strings.EqualFold(l.Key, "ScorePeriod"):
				if n, err := strconv.Atoi(v); err != nil || n < scorePeriodMin || n > scorePeriodMax {
					bad(sec.Name, l.Key, "out_of_range", "%q is not a whole number from %d to %d", v, scorePeriodMin, scorePeriodMax)
				}
			case slices.ContainsFunc([]string{"MinimumRequiredPlayers", "MaxReservedSlots", "ServerMinPlayerCash", "ServerMaxPlayerCash",
				"ServerMinPlayerLevel", "ServerMaxPlayerLevel", "OverpopulatedTeamThresholdConfig", "Port"}, func(k string) bool { return strings.EqualFold(k, l.Key) }):
				if n, err := strconv.Atoi(v); err != nil || n < 0 {
					bad(sec.Name, l.Key, "invalid_value", "%q is not a whole number of at least 0", v)
				}
			case strings.EqualFold(l.Key, "MaxPlayers"):
				if n, err := strconv.Atoi(v); err != nil || n < 1 || n > 200 {
					bad(sec.Name, l.Key, "out_of_range", "%q is not a whole number from 1 to 200", v)
				}
			case strings.EqualFold(l.Key, "ServerName"):
				if len([]rune(v)) > 64 {
					bad(sec.Name, l.Key, "too_long", "the name is %d characters; at most 64", len([]rune(v)))
				}
			case strings.EqualFold(l.Key, "ServerImageURL"):
				if v != "" && !allowedImage(v) {
					bad(sec.Name, l.Key, "host_not_allowed", "the banner must be https on %s", strings.Join(imageHosts, ", "))
				}
			case strings.EqualFold(l.Key, "RotationMode"):
				if !strings.EqualFold(v, "ordered") && !strings.EqualFold(v, "random") {
					bad(sec.Name, l.Key, "invalid_value", "%q is not Ordered or Random", v)
				}
			case strings.HasPrefix(strings.ToLower(l.Key), "b") && (strings.EqualFold(sec.Name, secRotation) || strings.EqualFold(sec.Name, secRCON) || strings.EqualFold(sec.Name, secTeams)):
				if !strings.EqualFold(v, "true") && !strings.EqualFold(v, "false") {
					bad(sec.Name, l.Key, "invalid_value", "%q is not True or False", v)
				}
			case strings.EqualFold(l.Key, keyRotation) && l.Op != "!":
				if _, err := parseRotationEntry(v); err != nil && l.Op != "-" {
					bad(sec.Name, l.Key, "invalid_value", "%v", err)
				}
			case (strings.EqualFold(l.Key, keyBanned) || strings.EqualFold(l.Key, keyReserved)) && l.Op != "!":
				if len(v) != 17 || strings.Trim(v, "0123456789") != "" {
					bad(sec.Name, l.Key, "invalid_value", "%q is not a SteamID64", v)
				}
			}
		}
	}
	dropEmptyArrays(&c.doc)
	return c
}

// schema is a section the server honours.
func (s *Server) schema(name string) (wardogs.Section, bool) {
	for _, sec := range s.sections {
		if sec.Section == name {
			return sec, true
		}
	}
	return wardogs.Section{}, false
}

func allowedImage(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return slices.ContainsFunc(imageHosts, func(h string) bool { return host == h || strings.HasSuffix(host, "."+h) })
}

// setValue replaces a plain key's value, or adds the key at the end of the section.
func setValue(sec *wardogs.DocSection, key, value string) {
	for i, l := range sec.Lines {
		if l.Op == "" && strings.EqualFold(l.Key, key) {
			sec.Lines[i] = wardogs.DocLine{Key: l.Key, Value: value}
			return
		}
	}
	sec.Lines = append(sec.Lines, wardogs.DocLine{Key: key, Value: value})
}

// dropEmptyArrays removes the lines of an array key that ends up empty: the server drops an
// empty array when it saves the document.
func dropEmptyArrays(d *wardogs.Doc) {
	for i := range d.Sections {
		sec := &d.Sections[i]
		empty := map[string]bool{}
		for _, l := range sec.Lines {
			if l.Op != "" && l.Key != "" {
				empty[strings.ToLower(l.Key)] = len(sec.Array(l.Key)) == 0
			}
		}
		sec.Lines = slices.DeleteFunc(sec.Lines, func(l wardogs.DocLine) bool { return l.Op != "" && empty[strings.ToLower(l.Key)] })
	}
}

// changes groups what differs between two documents by section, for the answer's changed list.
func changes(older, newer wardogs.Doc) []wardogs.SectionChange {
	var out []wardogs.SectionChange
	index := map[string]int{}
	for _, k := range wardogs.DiffDocs(older, newer) {
		i, ok := index[k.Section]
		if !ok {
			i = len(out)
			index[k.Section] = i
			out = append(out, wardogs.SectionChange{Section: k.Section, Added: older.Section(k.Section) == nil, Removed: newer.Section(k.Section) == nil})
		}
		if !slices.Contains(out[i].Keys, k.Key) {
			out[i].Keys = append(out[i].Keys, k.Key)
		}
	}
	return out
}

// outcomes say when each changed section takes effect: the section's own time, or a key's when
// every key that changed shares an override.
func (s *Server) outcomes(changed []wardogs.SectionChange) []wardogs.Outcome {
	out := make([]wardogs.Outcome, 0, len(changed))
	for _, c := range changed {
		schema, _ := s.schema(c.Section)
		state, detail := schema.AppliesWhen, schema.Description
		override := ""
		for i, k := range c.Keys {
			when := ""
			for _, o := range schema.KeyOverrides {
				if strings.EqualFold(o.Key, k) {
					when = o.AppliesWhen
				}
			}
			if i == 0 {
				override = when
			} else if when != override {
				override = ""
			}
		}
		if override != "" {
			state = override
		}
		out = append(out, wardogs.Outcome{Section: c.Section, State: state, Detail: detail})
	}
	return out
}
