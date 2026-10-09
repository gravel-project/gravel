package record

import (
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"regexp"
	"slices"
	"strings"

	"github.com/gravel-project/gravel/games/wardogs"
)

// The fixtures land in a public repository, so a recording carries no real person and no real
// address: SteamIDs, player names and IP addresses are replaced with stand-ins, consistently
// across every file of one recording, and every secret is redacted.
var (
	steamIDPattern = regexp.MustCompile(`7656119\d{10}`)
	ipv4Pattern    = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)
	ipv6Pattern    = regexp.MustCompile(`[0-9A-Fa-f]{0,4}(?::[0-9A-Fa-f]{0,4}){2,7}`)
)

// Stand-ins. The SteamIDs sit below the first individual account (76561197960265728), so none
// is a real person; the addresses are from the documentation ranges (RFC 5737, RFC 3849).
const (
	fakeSteamIDBase = "76561190000000" // + three digits
	fakeIPv4Prefix  = "192.0.2."
	fakeIPv6Prefix  = "2001:db8::"
	// fakeServerIDFormat is a version-4-shaped UUID that names no server.
	fakeServerIDPrefix = "00000000-0000-4000-8000-"
	fakeServerIDFormat = fakeServerIDPrefix + "%012d"
)

// Counts are what a recording scrubbed.
type Counts struct {
	SteamIDs  int `json:"steamIds"`
	Names     int `json:"names"`
	Addresses int `json:"addresses"`
	// ServerIDs are the host's identifiers for the server (GET /v1/server-id).
	ServerIDs int `json:"serverIds"`
	Secrets   int `json:"secrets"`
}

// standInName, and the prefixes above, recognise a value that is already a stand-in (a
// recording of a fake, or of a server that was recorded before): it is kept, never mapped
// again, so a re-recording is stable and the final check never mistakes it for an original.
var standInName = regexp.MustCompile(`^Player \d+$`)

// scrubber holds one recording's replacements, so the same person or address gets the same
// stand-in in every file.
type scrubber struct {
	token     string
	steamIDs  map[string]string
	names     map[string]string
	addresses map[string]string
	serverIDs map[string]string
	secrets   int
}

func newScrubber(token string) *scrubber {
	return &scrubber{token: token, steamIDs: map[string]string{}, names: map[string]string{}, addresses: map[string]string{}, serverIDs: map[string]string{}}
}

func (s *scrubber) counts() Counts {
	return Counts{SteamIDs: len(s.steamIDs), Names: len(s.names), Addresses: len(s.addresses), ServerIDs: len(s.serverIDs), Secrets: s.secrets}
}

// originals are the values a written file must never contain.
func (s *scrubber) originals() []string {
	var out []string
	if s.token != "" {
		out = append(out, s.token)
	}
	for _, m := range []map[string]string{s.steamIDs, s.names, s.addresses, s.serverIDs} {
		for k := range m {
			out = append(out, k)
		}
	}
	return out
}

// isNameKey and isSteamIDKey recognise a player's object: a name beside a SteamID.
func isNameKey(k string) bool {
	switch strings.ToLower(k) {
	case "name", "playername", "displayname", "nickname", "killername", "victimname":
		return true
	}
	return false
}

func isSteamIDKey(k string) bool { return strings.Contains(strings.ToLower(k), "steamid") }

func isServerIDKey(k string) bool { return strings.EqualFold(k, "serverId") }

// collect is the first pass: it learns every player name (a name-like key in an object that also
// has a SteamID-like key), so the second pass can replace the name wherever it appears, in free
// text too.
func (s *scrubber) collect(v any) {
	switch x := v.(type) {
	case map[string]any:
		hasID := false
		for k := range x {
			if isSteamIDKey(k) {
				hasID = true
			}
		}
		for _, k := range slices.Sorted(maps.Keys(x)) {
			child := x[k]
			if name, ok := child.(string); ok && hasID && isNameKey(k) && strings.TrimSpace(name) != "" && !standInName.MatchString(name) {
				if _, seen := s.names[name]; !seen {
					s.names[name] = fmt.Sprintf("Player %d", len(s.names)+1)
				}
			}
			s.collect(child)
		}
	case []any:
		for _, child := range x {
			s.collect(child)
		}
	}
}

// scrub is the second pass: it rewrites every string and number. Keys are walked in order, so
// the stand-ins are numbered the same way on every recording of the same answers.
func (s *scrubber) scrub(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(x)) {
			if id, ok := x[k].(string); ok && isServerIDKey(k) && id != "" && !strings.HasPrefix(id, fakeServerIDPrefix) {
				fake, seen := s.serverIDs[id]
				if !seen {
					fake = fmt.Sprintf(fakeServerIDFormat, len(s.serverIDs)+1)
					s.serverIDs[id] = fake
				}
				x[k] = fake
				continue
			}
			x[k] = s.scrub(x[k])
		}
		return x
	case []any:
		for i, child := range x {
			x[i] = s.scrub(child)
		}
		return x
	case string:
		return s.text(x)
	case json.Number:
		if t := s.text(string(x)); t != string(x) {
			return json.Number(t)
		}
		return x
	}
	return v
}

func (s *scrubber) text(in string) string {
	out := in
	if s.token != "" && strings.Contains(out, s.token) {
		out = strings.ReplaceAll(out, s.token, wardogs.Redacted)
		s.secrets++
	}
	if r := wardogs.RedactConfig(out); r != out {
		s.secrets += strings.Count(r, wardogs.Redacted) - strings.Count(out, wardogs.Redacted)
		out = r
	}
	out = steamIDPattern.ReplaceAllStringFunc(out, func(id string) string {
		if strings.HasPrefix(id, fakeSteamIDBase) {
			return id
		}
		fake, ok := s.steamIDs[id]
		if !ok {
			fake = fmt.Sprintf("%s%03d", fakeSteamIDBase, len(s.steamIDs)+1)
			s.steamIDs[id] = fake
		}
		return fake
	})
	out = ipv4Pattern.ReplaceAllStringFunc(out, func(a string) string {
		if ip := net.ParseIP(a); ip == nil || ip.To4() == nil {
			return a
		}
		return s.address(a, fakeIPv4Prefix)
	})
	out = ipv6Pattern.ReplaceAllStringFunc(out, func(a string) string {
		if ip := net.ParseIP(a); ip == nil || ip.To4() != nil || ip.IsLoopback() || ip.IsUnspecified() {
			return a
		}
		return s.address(a, fakeIPv6Prefix)
	})
	// Longest first, so a name inside another name does not split it.
	names := slices.SortedFunc(maps.Keys(s.names), func(a, b string) int {
		if d := len(b) - len(a); d != 0 {
			return d
		}
		return strings.Compare(a, b)
	})
	for _, name := range names {
		if out == name || (len(name) >= 3 && strings.Contains(out, name)) {
			out = strings.ReplaceAll(out, name, s.names[name])
		}
	}
	return out
}

func (s *scrubber) address(a, prefix string) string {
	if strings.HasPrefix(a, prefix) {
		return a
	}
	fake, ok := s.addresses[a]
	if !ok {
		fake = fmt.Sprintf("%s%d", prefix, len(s.addresses)+1)
		s.addresses[a] = fake
	}
	return fake
}
