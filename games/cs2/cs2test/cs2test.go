// Package cs2test is a fake Counter-Strike 2 server for tests: an RCON server (gorcon's rcontest)
// that answers "status" from a list of players and records every other command, and a log emitter
// that posts log lines the way logaddress_add_http does.
package cs2test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gorcon/rcon"
	"github.com/gorcon/rcon/rcontest"
)

// Password is the RCON password the fake server takes by default.
const Password = "cs2test-rcon"

// Player is one player the fake server lists in status.
type Player struct {
	UserID int
	Name   string
	PingMs int
}

// Server is a fake CS2 server's RCON side.
type Server struct {
	srv *rcontest.Server

	mu       sync.Mutex
	mapName  string
	max      int
	version  string
	players  []Player
	commands []string
	answers  map[string]string
}

// New starts a fake server on the loopback; it stops when the test ends.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{mapName: "de_inferno", max: 10, version: "1.40.6.4/14064 10551", answers: map[string]string{}}
	s.srv = rcontest.NewServer(
		rcontest.SetSettings(rcontest.Settings{Password: Password}),
		rcontest.SetAuthHandler(rcontest.AuthHandler),
		rcontest.SetCommandHandler(func(c *rcontest.Context) {
			body := s.handle(c.Request().Body())
			_, _ = rcon.NewPacket(rcon.SERVERDATA_RESPONSE_VALUE, c.Request().ID, body).WriteTo(c.Conn())
		}),
	)
	if s.srv == nil {
		t.Fatal("cs2test: could not start the rcon server")
	}
	t.Cleanup(s.srv.Close)
	return s
}

// Addr is the server's RCON address (host:port).
func (s *Server) Addr() string { return s.srv.Addr() }

// SetPlayers replaces who status lists.
func (s *Server) SetPlayers(p ...Player) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.players = slices.Clone(p)
}

// SetMap sets the map status reports.
func (s *Server) SetMap(m string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mapName = m
}

// SetVersion sets the build status reports.
func (s *Server) SetVersion(v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version = v
}

// Answer makes a command answer with text instead of "".
func (s *Server) Answer(command, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answers[command] = text
}

// Commands are the commands the server received other than status, in order.
func (s *Server) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.commands)
}

func (s *Server) handle(cmd string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cmd == "status" {
		return s.statusLocked()
	}
	s.commands = append(s.commands, cmd)
	if a, ok := s.answers[cmd]; ok {
		return a
	}
	if userID, ok := strings.CutPrefix(cmd, "kickid "); ok {
		id, _ := strconv.Atoi(strings.Fields(userID)[0])
		s.players = slices.DeleteFunc(s.players, func(p Player) bool { return p.UserID == id })
	}
	if m, ok := strings.CutPrefix(cmd, "changelevel "); ok {
		s.mapName = m
	}
	return ""
}

func (s *Server) statusLocked() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Server:  Running [0.0.0.0:27015]\nhostname : cs2test\nversion  : %s secure  public\n", s.version)
	fmt.Fprintf(&b, "players  : %d humans, 0 bots (%d max) (not hibernating) (unreserved)\n", len(s.players), s.max)
	fmt.Fprintf(&b, "loaded spawngroup(  1)  : SV:  [1: %s | main lump | mapload]\n\n", s.mapName)
	b.WriteString("---------players--------\n  id     time ping loss      state   rate adr name\n")
	b.WriteString("65535 [NoChan]    0    0 challenging      0unknown ''\n")
	for _, p := range s.players {
		fmt.Fprintf(&b, "%5d    00:10 %4d    0     active 786432 198.51.100.%d:27005 '%s'\n", p.UserID, p.PingMs, p.UserID, p.Name)
	}
	b.WriteString("#end\n")
	return b.String()
}

// Emitter posts log lines to a URL as logaddress_add_http does: text/plain, the instance token
// and the byte offsets in headers, no Authorization.
type Emitter struct {
	URL      string
	Instance string
	Client   *http.Client

	mu     sync.Mutex
	offset int
}

// Post sends one batch and returns the response's status code.
func (e *Emitter) Post(ctx context.Context, lines ...string) (int, error) {
	body := strings.Join(lines, "\n") + "\n"
	e.mu.Lock()
	begin := e.offset
	e.offset += len(body)
	end := e.offset
	e.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.URL, bytes.NewBufferString(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("User-Agent", "Valve/Steam HTTP Client 1.0 (730)")
	req.Header.Set("X-Server-Instance-Token", e.Instance)
	req.Header.Set("X-LogBytes-BeginOffset", strconv.Itoa(begin))
	req.Header.Set("X-LogBytes-EndOffset", strconv.Itoa(end))
	c := e.Client
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}
