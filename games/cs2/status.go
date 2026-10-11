package cs2

import (
	"bufio"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Status is what the server's "status" command reports. CS2's status lists no SteamIDs (CS:GO's
// did): who a player is comes from the log stream (Roster), keyed by the user id both carry.
type Status struct {
	Hostname string
	// Version is the build ("1.40.6.4/14064 10551"); a change is a game update.
	Version     string
	Map         string
	Humans      int
	Bots        int
	MaxPlayers  int
	Hibernating bool
	Players     []StatusPlayer
}

// StatusPlayer is one row of the players table.
type StatusPlayer struct {
	UserID  int
	Name    string
	TimeOn  time.Duration
	PingMs  int
	Loss    int
	State   string
	Address string
}

var (
	fieldRE     = regexp.MustCompile(`^([a-z/ ]+?)\s*:\s?(.*)$`)
	playersRE   = regexp.MustCompile(`(\d+) humans?, (\d+) bots? \((\d+) max\)`)
	statusMapRE = regexp.MustCompile(`\[1: ([A-Za-z0-9_/.-]+) \|`)
	rowRE       = regexp.MustCompile(`^\s*(\d+)\s+(.*?)\s*'(.*)'\s*$`)
	addrRE      = regexp.MustCompile(`(\d{1,3}(?:\.\d{1,3}){3}:\d+)`)
)

// noChannel is the placeholder row CS2 lists for a slot with no client (id 65535, "[NoChan]").
const noChannel = 65535

// ParseStatus parses a "status" answer. It is tolerant: a line it does not know is skipped, and a
// field it cannot read stays zero. It fails only on an answer that is not a status at all.
func ParseStatus(s string) (Status, error) {
	var st Status
	seenPlayers, inTable := false, false
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			continue
		case strings.HasPrefix(trimmed, "---------players"):
			inTable = true
			continue
		case trimmed == "#end":
			inTable = false
			continue
		}
		if inTable {
			if strings.HasPrefix(trimmed, "id ") {
				continue // the header row
			}
			if p, ok := parseRow(line); ok {
				st.Players = append(st.Players, p)
			}
			continue
		}
		if m := statusMapRE.FindStringSubmatch(line); m != nil && st.Map == "" {
			st.Map = m[1]
			continue
		}
		m := fieldRE.FindStringSubmatch(trimmed)
		if m == nil {
			continue
		}
		key, val := strings.TrimSpace(m[1]), strings.TrimSpace(m[2])
		switch key {
		case "hostname":
			st.Hostname = val
		case "version":
			if f := strings.Fields(val); len(f) >= 2 {
				st.Version = f[0] + " " + f[1]
			} else {
				st.Version = val
			}
		case "players":
			if pm := playersRE.FindStringSubmatch(val); pm != nil {
				st.Humans, _ = strconv.Atoi(pm[1])
				st.Bots, _ = strconv.Atoi(pm[2])
				st.MaxPlayers, _ = strconv.Atoi(pm[3])
				seenPlayers = true
			}
			st.Hibernating = strings.Contains(val, "(hibernating)")
		}
	}
	if err := sc.Err(); err != nil {
		return Status{}, err
	}
	if !seenPlayers {
		return Status{}, fmt.Errorf("cs2: not a status answer (no players line)")
	}
	return st, nil
}

// parseRow reads one players-table row: the id first, the name last in single quotes, and in
// between time, ping, loss, state, rate and address, which CS2 sometimes runs together.
func parseRow(line string) (StatusPlayer, bool) {
	m := rowRE.FindStringSubmatch(line)
	if m == nil {
		return StatusPlayer{}, false
	}
	id, err := strconv.Atoi(m[1])
	if err != nil || id == noChannel {
		return StatusPlayer{}, false
	}
	p := StatusPlayer{UserID: id, Name: m[3]}
	f := strings.Fields(m[2])
	if len(f) > 0 {
		p.TimeOn = parseTimeOn(f[0])
	}
	if len(f) > 1 {
		p.PingMs, _ = strconv.Atoi(f[1])
	}
	if len(f) > 2 {
		p.Loss, _ = strconv.Atoi(f[2])
	}
	if len(f) > 3 {
		p.State = f[3]
	}
	if a := addrRE.FindString(m[2]); a != "" {
		p.Address = a
	}
	return p, true
}

// parseTimeOn reads "mm:ss" or "hh:mm:ss"; anything else (a bot's "BOT") is zero.
func parseTimeOn(s string) time.Duration {
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0
	}
	var total int
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0
		}
		total = total*60 + n
	}
	return time.Duration(total) * time.Second
}
