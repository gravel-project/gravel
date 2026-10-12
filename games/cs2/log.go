package cs2

import (
	"bufio"
	"bytes"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yungwood/cs2log"
)

// The headers CS2 sends with each log batch (logaddress_add_http), as observed from a CS2 server
// on 2026-07-17 (github.com/arieladasme/cs2-watch, testdata/log-samples.txt): the batch's place in
// the server's log (byte offsets, for duplicates and gaps), the server's instance (a new token is
// a restart), and the game state as of the batch. The server sends no Authorization header.
var LogHeaders = []string{
	"X-Server-Instance-Token",
	"X-LogBytes-BeginOffset",
	"X-LogBytes-EndOffset",
	"X-Tick-Start",
	"X-Tick-End",
	"X-Timestamp",
	"X-Game-Map",
	"X-Game-State",
	"X-Game-TeamCT",
	"X-Game-ScoreCT",
	"X-Game-TeamT",
	"X-Game-ScoreT",
}

// SteamID64 converts the SteamID a log line carries ("[U:1:N]" or "STEAM_X:Y:Z") to the 64-bit
// form gravel keys players by; ok is false for a bot ("BOT") or anything else.
func SteamID64(steamID string) (uint64, bool) {
	id, err := cs2log.DecodeSteamID(strings.TrimSpace(steamID))
	if err != nil {
		return 0, false
	}
	return id.SteamID64(), true
}

// RosterPlayer is a connected player as the log stream knows them.
type RosterPlayer struct {
	UserID    int
	Name      string
	SteamID64 uint64
	// Side is "CT", "TERRORIST", "Spectator", or "" before the player picks one.
	Side        string
	Kills       int
	Deaths      int
	ConnectedAt time.Time
	// Entered is the player being in the game (past loading), not only connected.
	Entered bool
}

// Change is what one log line did to the roster.
type Change struct {
	// Connected is a player who just connected (with their SteamID): the moment to enforce a ban.
	Connected *RosterPlayer
	// Disconnected is a player who left.
	Disconnected *RosterPlayer
	// Reset is the server restarting (a new log file): everyone is gone.
	Reset bool
	// MatchOver is a match ending (game over); the counters start again.
	MatchOver bool
	// MapChange is the map the server started loading ("Loading map"): a new map, after a
	// match's end or a changelevel.
	MapChange string
	// Said is a chat line from a player with a SteamID, on any team or none.
	Said *Chat
}

// Chat is one chat line, with the account that typed it: the log ties it to the player's SteamID,
// which is what lets a code typed in chat prove the account (gravel#5).
type Chat struct {
	UserID    int
	Name      string
	SteamID64 uint64
	Text      string
	// Team is team chat (say_team), not all chat.
	Team bool
}

// sayAnySide is a chat line from a player on any side: cs2log reads one only from a player on CT
// or T (the others are its Unknown), and a code is as often typed from the team-select screen
// (Unassigned) or as a Spectator.
var sayAnySide = regexp.MustCompile(`^"(.*)<(\d+)><([^<>]*)><([^<>]*)>" say(_team)? "(.*)"$`)

// Roster is who is on the server, kept from the log stream: CS2's status lists no SteamIDs, so
// the log's connect lines are where an identity comes from (principle 6: logs are authoritative).
// It is safe for concurrent use.
type Roster struct {
	parser *cs2log.Parser

	mu      sync.Mutex
	players map[int]*RosterPlayer // by user id
}

// NewRoster returns an empty roster that reads timestamps in the server's timezone (IANA name;
// "" is UTC).
func NewRoster(logTimezone string) (*Roster, error) {
	p, err := cs2log.NewParser(cs2log.Config{LogTimezone: logTimezone})
	if err != nil {
		return nil, err
	}
	return &Roster{parser: p, players: map[int]*RosterPlayer{}}, nil
}

// Push reads one log line. A line that is not a log line, or one the roster does not need, changes
// nothing and is no error; err is only a line cs2log cannot read at all.
func (r *Roster) Push(line string) (Change, error) {
	line = strings.TrimRight(line, "\r")
	ev, err := r.parser.ParseLine(line)
	if err != nil {
		if errors.Is(err, cs2log.ErrNoMatch) {
			return Change{}, nil
		}
		return Change{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch e := ev.(type) {
	case cs2log.PlayerConnected:
		id, ok := userID(e.Player)
		sid, human := SteamID64(e.Player.SteamID)
		if !ok || !human {
			return Change{}, nil
		}
		p := &RosterPlayer{UserID: id, Name: e.Player.Name, SteamID64: sid, ConnectedAt: e.Timestamp()}
		r.players[id] = p
		cp := *p
		return Change{Connected: &cp}, nil
	case cs2log.PlayerValidated:
		r.touch(e.Player)
	case cs2log.PlayerEntered:
		if p := r.touch(e.Player); p != nil {
			p.Entered = true
		}
	case cs2log.PlayerSwitched:
		if p := r.touch(e.Player); p != nil {
			p.Side = e.To
		}
	case cs2log.PlayerKill:
		if p := r.touch(e.Attacker); p != nil {
			p.Kills++
		}
		if p := r.touch(e.Victim); p != nil {
			p.Deaths++
		}
	case cs2log.PlayerKilledSuicide:
		if p := r.touch(e.Player); p != nil {
			p.Deaths++
		}
	case cs2log.PlayerKilledBomb:
		if p := r.touch(e.Player); p != nil {
			p.Deaths++
		}
	case cs2log.PlayerDisconnected:
		id, ok := userID(e.Player)
		if !ok {
			return Change{}, nil
		}
		p, on := r.players[id]
		if !on {
			return Change{}, nil
		}
		delete(r.players, id)
		cp := *p
		return Change{Disconnected: &cp}, nil
	case cs2log.GameOver:
		for _, p := range r.players {
			p.Kills, p.Deaths = 0, 0
		}
		return Change{MatchOver: true}, nil
	case cs2log.LogFileStarted:
		clear(r.players)
		return Change{Reset: true}, nil
	case cs2log.MapLoadingStarted:
		return Change{MapChange: e.Map}, nil
	case cs2log.PlayerSay:
		return chat(e.Player.UserID, e.Player.Name, e.Player.SteamID, e.Text, e.Team), nil
	case cs2log.Unknown: // cs2log reads chat only from CT or T
		if m := sayAnySide.FindStringSubmatch(e.Payload); m != nil {
			return chat(m[2], m[1], m[3], m[6], m[5] != ""), nil
		}
	}
	return Change{}, nil
}

// chat is a chat line's change: nothing for a bot's or a console's line, which has no SteamID.
func chat(userID, name, steamID, text string, team bool) Change {
	id, err := strconv.Atoi(userID)
	sid, human := SteamID64(steamID)
	if err != nil || id < 0 || !human {
		return Change{}
	}
	return Change{Said: &Chat{UserID: id, Name: name, SteamID64: sid, Text: text, Team: team}}
}

// PushBatch reads every line of a batch (a log push's body) and returns what changed, in order.
func (r *Roster) PushBatch(body []byte) []Change {
	var out []Change
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		c, err := r.Push(sc.Text())
		if err != nil {
			continue
		}
		if c.Connected != nil || c.Disconnected != nil || c.Reset || c.MatchOver || c.MapChange != "" || c.Said != nil {
			out = append(out, c)
		}
	}
	return out
}

// touch is the roster's player for a log token, refreshing the name and the side the line shows.
// A player the roster missed (the hub started mid-session) is added when the line carries a human
// SteamID. Called with r.mu held.
func (r *Roster) touch(lp cs2log.Player) *RosterPlayer {
	id, ok := userID(lp)
	if !ok {
		return nil
	}
	p, on := r.players[id]
	if !on {
		sid, human := SteamID64(lp.SteamID)
		if !human {
			return nil
		}
		p = &RosterPlayer{UserID: id, SteamID64: sid}
		r.players[id] = p
	}
	if lp.Name != "" {
		p.Name = lp.Name
	}
	if s := string(lp.Side); s != "" && s != string(cs2log.SideUnassigned) {
		p.Side = s
	}
	return p
}

func userID(p cs2log.Player) (int, bool) {
	id, err := strconv.Atoi(p.UserID)
	return id, err == nil && id >= 0
}

// Players are the connected players, by user id.
func (r *Roster) Players() []RosterPlayer {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RosterPlayer, 0, len(r.players))
	for _, p := range r.players {
		out = append(out, *p)
	}
	slices.SortFunc(out, func(a, b RosterPlayer) int { return a.UserID - b.UserID })
	return out
}

// ByUserID is a connected player by the server's user id.
func (r *Roster) ByUserID(id int) (RosterPlayer, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.players[id]
	if !ok {
		return RosterPlayer{}, false
	}
	return *p, true
}

// BySteamID64 is a connected player by SteamID64.
func (r *Roster) BySteamID64(id uint64) (RosterPlayer, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.players {
		if p.SteamID64 == id {
			return *p, true
		}
	}
	return RosterPlayer{}, false
}

// Clear empties the roster (the server's host went away).
func (r *Roster) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	clear(r.players)
}

var cvarRE = regexp.MustCompile(`^(.*?)"([^"]+)" = "(.*)"\s*$`)

// sensitiveCvar is a cvar whose value is a secret (the join and RCON passwords, a token, a key),
// by the same rule cs2log redacts by.
func sensitiveCvar(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, marker := range []string{"password", "passwd", "secret", "token", "apikey", "api_key", "key", "auth", "credential"} {
		if strings.Contains(n, marker) {
			return true
		}
	}
	return false
}

// linkCode is a proof-of-control code typed in chat ("!link 7K3M-9QXA").
var linkCode = regexp.MustCompile(`(?i)([!.]link\s+)[0-9a-z]{4}-?[0-9a-z]{4}`)

// RedactLog returns a log batch with every secret it may carry replaced: the value of a secret
// cvar in the server's startup dump ("sv_password" = "…"), and every occurrence of the given
// strings (the feed token the batch was posted with, which a logged console command could echo).
// The redacted batch is what is kept; the offsets in the headers still describe the original.
func RedactLog(body []byte, secrets ...string) []byte {
	lines := bytes.SplitAfter(body, []byte("\n"))
	var out bytes.Buffer
	out.Grow(len(body))
	for _, l := range lines {
		s := string(l)
		nl := ""
		if strings.HasSuffix(s, "\n") {
			s, nl = strings.TrimSuffix(s, "\n"), "\n"
		}
		if m := cvarRE.FindStringSubmatch(s); m != nil && sensitiveCvar(m[2]) && m[3] != "" {
			s = m[1] + `"` + m[2] + `" = "[REDACTED]"`
		}
		s = linkCode.ReplaceAllString(s, "${1}[REDACTED]") // a proof code typed in chat (gravel#5)
		for _, sec := range secrets {
			if sec != "" {
				s = strings.ReplaceAll(s, sec, "[REDACTED]")
			}
		}
		out.WriteString(s + nl)
	}
	return out.Bytes()
}
