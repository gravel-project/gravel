// Package cloudvmtest holds test doubles for the cloud-VM driver: an SSH remote that pretends to
// be the hosts, an in-memory host store, and a game whose players a test sets.
package cloudvmtest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/gravel-project/gravel/drivers"
	"github.com/gravel-project/gravel/drivers/cloudvm"
)

// Signer is a fresh ed25519 key, as the hub's SSH key.
func Signer(t testing.TB) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Run is one command a Remote ran.
type Run struct {
	Address, User, Command, Stdin string
}

// Remote is a fake SSH remote. A host is booted once Boot names its address; until then every
// command fails as a refused connection. Every host presents HostKey unless HostKeys overrides
// it.
type Remote struct {
	mu       sync.Mutex
	booted   map[string]bool
	runs     []Run
	HostKey  string
	HostKeys map[string]string
	// Fail makes every command fail (after the host key is seen).
	Fail error
	// Env is the last environment file pushed to each address.
	Env map[string]string
}

// NewRemote is a remote whose hosts are booted when they are first reached.
func NewRemote() *Remote {
	return &Remote{booted: map[string]bool{}, HostKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFakeHostKeyFakeHostKeyFakeHostKey000", HostKeys: map[string]string{}, Env: map[string]string{}}
}

// Boot marks a host booted.
func (r *Remote) Boot(address string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.booted[address] = true
}

// BootAll makes every host booted at its first command.
func (r *Remote) BootAll() { r.Boot("*") }

// Runs are the commands run, in order.
func (r *Remote) Runs() []Run {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.runs)
}

// Run implements cloudvm.Remote.
func (r *Remote) Run(_ context.Context, t cloudvm.RemoteTarget, stdin io.Reader, command string) ([]byte, string, error) {
	var in []byte
	if stdin != nil {
		in, _ = io.ReadAll(stdin)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := r.HostKey
	if k, ok := r.HostKeys[t.Address]; ok {
		key = k
	}
	if !r.booted[t.Address] && !r.booted["*"] {
		return nil, "", errors.New("dial tcp: connection refused")
	}
	if t.HostKey != "" && t.HostKey != key {
		return nil, key, cloudvm.ErrHostKeyChanged
	}
	r.runs = append(r.runs, Run{Address: t.Address, User: t.User, Command: command, Stdin: string(in)})
	if r.Fail != nil {
		return nil, key, r.Fail
	}
	if strings.Contains(command, "/env.tmp") {
		r.Env[t.Address] = string(in)
	}
	return nil, key, nil
}

// Store is an in-memory cloudvm.HostStore.
type Store struct {
	mu     sync.Mutex
	hosts  map[string]cloudvm.Host
	events []cloudvm.Event
	// Saves counts SaveHost calls.
	Saves int
}

// NewStore is an empty store.
func NewStore() *Store { return &Store{hosts: map[string]cloudvm.Host{}} }

// Hosts implements cloudvm.HostStore.
func (s *Store) Hosts(context.Context) ([]cloudvm.Host, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []cloudvm.Host
	for _, h := range s.hosts {
		out = append(out, h)
	}
	return out, nil
}

// SaveHost implements cloudvm.HostStore.
func (s *Store) SaveHost(_ context.Context, h cloudvm.Host) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Saves++
	s.hosts[h.ServerID] = h
	return nil
}

// AppendEvent implements cloudvm.HostStore.
func (s *Store) AppendEvent(_ context.Context, e cloudvm.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
	return nil
}

// Host is a stored host.
func (s *Store) Host(id string) (cloudvm.Host, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.hosts[id]
	return h, ok
}

// Events are the stored events, in order.
func (s *Store) Events() []cloudvm.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.events)
}

// Transitions are the stored events as "from>to" strings.
func (s *Store) Transitions() []string {
	var out []string
	for _, e := range s.Events() {
		out = append(out, e.From+">"+e.To)
	}
	return out
}

// Game is a fake game control shared by every host it is made for.
type Game struct {
	mu       sync.Mutex
	up       bool
	status   cloudvm.GameStatus
	connects map[string][]cloudvm.GamePlayer // what the next Log returns, by body
	commands []string
	addrs    []string
	password string
}

// NewGame is a game that answers once Up is called.
func NewGame() *Game {
	return &Game{connects: map[string][]cloudvm.GamePlayer{}, status: cloudvm.GameStatus{Build: "1.0", Map: "de_test", MaxPlayers: 10}}
}

// Factory is the game's GameFactory.
func (g *Game) Factory() cloudvm.GameFactory {
	return func(addr, password string, _ cloudvm.Profile, _ *slog.Logger) (cloudvm.Game, error) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.addrs = append(g.addrs, addr)
		g.password = password
		return g, nil
	}
}

// Up makes the game answer (installed and started) or not.
func (g *Game) Up(up bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.up = up
}

// SetPlayers sets who is on.
func (g *Game) SetPlayers(rows ...cloudvm.GamePlayer) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.status.Rows = slices.Clone(rows)
	g.status.Players = len(rows)
}

// OnLog makes a log body report players connecting.
func (g *Game) OnLog(body string, connected ...cloudvm.GamePlayer) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.connects[body] = connected
}

// Commands are the commands sent (say, kick, quit).
func (g *Game) Commands() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.commands)
}

// Addrs are the addresses controls were made for.
func (g *Game) Addrs() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.addrs)
}

// Password is the password the game was last given.
func (g *Game) Password() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.password
}

func (g *Game) record(c string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.commands = append(g.commands, c)
	if !g.up {
		return errors.New("connection refused")
	}
	return nil
}

// Status implements cloudvm.Game.
func (g *Game) Status(context.Context) (cloudvm.GameStatus, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.up {
		return cloudvm.GameStatus{}, errors.New("connection refused")
	}
	st := g.status
	st.Rows = slices.Clone(st.Rows)
	return st, nil
}

// Kick implements cloudvm.Game.
func (g *Game) Kick(_ context.Context, userID int, reason string) error {
	if err := g.record("kick " + strconv.Itoa(userID) + " " + reason); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.status.Rows = slices.DeleteFunc(g.status.Rows, func(p cloudvm.GamePlayer) bool { return p.UserID == userID })
	g.status.Players = len(g.status.Rows)
	return nil
}

// Say implements cloudvm.Game.
func (g *Game) Say(_ context.Context, m string) error { return g.record("say " + m) }

// Quit implements cloudvm.Game.
func (g *Game) Quit(context.Context) error { return g.record("quit") }

// SetPassword implements cloudvm.Game.
func (g *Game) SetPassword(p string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.password = p
}

// Log implements cloudvm.Game.
func (g *Game) Log(body []byte) []cloudvm.GamePlayer {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.connects[string(body)])
}

// Find implements cloudvm.Game.
func (g *Game) Find(id drivers.Identity) (cloudvm.GamePlayer, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range g.status.Rows {
		if p.Identity == id {
			return p, true
		}
	}
	return cloudvm.GamePlayer{}, false
}

// Forget implements cloudvm.Game.
func (g *Game) Forget() {}
