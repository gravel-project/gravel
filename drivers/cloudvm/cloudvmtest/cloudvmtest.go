// Package cloudvmtest holds test doubles for the cloud-VM driver: an SSH remote that pretends to
// be the hosts, an in-memory host store, and a game whose players a test sets.
package cloudvmtest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/gravel-project/gravel/cloud"
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
	// Env is the environment file on each address: the last one pushed, or one SetEnv put there.
	// A command that hashes it (cloudvm.EnvHashScript) answers from here.
	Env map[string]string
	// Files are the game's hooks on each address's disk, by their path on the host
	// ("~/cs2/pre.sh"): the last ones pushed, or ones SetFile put there.
	Files map[string]map[string]string
	// HashFail makes hashing the environment file fail on an address ("*": on every one).
	HashFail map[string]error
	// DiskOf, when set, names the disk an address's home is on, so the environment file follows
	// the data volume from one instance to the next as it does on a real host (HomeOnVolume).
	DiskOf func(address string) string
}

// HomeOnVolume is a DiskOf for a provider: an instance's home is its first attached volume.
func HomeOnVolume(p cloud.Provider) func(address string) string {
	return func(address string) string {
		insts, err := p.Instances(context.Background(), cloud.Filter{})
		if err != nil {
			return address
		}
		for _, i := range insts {
			if (i.PublicIPv4 == address || i.PrivateIPv4 == address) && len(i.VolumeIDs) > 0 {
				return "volume:" + i.VolumeIDs[0]
			}
		}
		return address
	}
}

// home is where an address's files live: its disk, or the address itself.
func (r *Remote) home(address string) string {
	if r.DiskOf != nil {
		return r.DiskOf(address)
	}
	return address
}

// NewRemote is a remote whose hosts are booted when they are first reached.
func NewRemote() *Remote {
	return &Remote{booted: map[string]bool{}, HostKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFakeHostKeyFakeHostKeyFakeHostKey000", HostKeys: map[string]string{}, Env: map[string]string{}, Files: map[string]map[string]string{}, HashFail: map[string]error{}}
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
	home := r.home(t.Address) // outside the lock: DiskOf may ask the provider
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
		r.Env[home] = string(in)
		for _, m := range pushedHook.FindAllStringSubmatch(command, -1) {
			b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(m[2], "\n", ""))
			if err != nil {
				return nil, key, fmt.Errorf("base64: %w", err)
			}
			if r.Files[home] == nil {
				r.Files[home] = map[string]string{}
			}
			r.Files[home][m[1]] = string(b)
		}
	}
	if strings.Contains(command, "sha256sum") {
		if fail, ok := r.HashFail[t.Address]; ok {
			return nil, key, fail
		}
		if fail, ok := r.HashFail["*"]; ok {
			return nil, key, fail
		}
		m := hashedFiles.FindStringSubmatch(command)
		if m == nil {
			return nil, key, errors.New("cloudvmtest: a hash command without its files")
		}
		var out strings.Builder
		for _, f := range strings.Fields(m[1]) {
			content, ok := r.Files[home][f]
			if strings.HasSuffix(f, "/env") {
				content, ok = r.Env[home]
			}
			if !ok {
				out.WriteString("missing " + f + "\n")
				continue
			}
			sum := sha256.Sum256([]byte(content))
			out.WriteString(hex.EncodeToString(sum[:]) + "  " + f + "\n")
		}
		return []byte(out.String()), key, nil
	}
	return nil, key, nil
}

var (
	// pushedHook is one hook in cloudvm.EnvScript: its path on the host and its base64.
	pushedHook = regexp.MustCompile(`base64 -d > (\S+)\.tmp <<'GRAVEL_HOOK'\n([A-Za-z0-9+/=\n]*)GRAVEL_HOOK\n`)
	// hashedFiles is the files cloudvm.EnvHashScript hashes.
	hashedFiles = regexp.MustCompile(`for f in (.+?); do`)
)

// SetFile changes a hook on a host behind the hub's back ("~/cs2/pre.sh"); a nil content deletes it.
func (r *Remote) SetFile(address, path string, content *string) {
	home := r.home(address)
	r.mu.Lock()
	defer r.mu.Unlock()
	if content == nil {
		delete(r.Files[home], path)
		return
	}
	if r.Files[home] == nil {
		r.Files[home] = map[string]string{}
	}
	r.Files[home][path] = *content
}

// FileOn is a hook on a host ("~/cs2/pre.sh"), and whether it is there.
func (r *Remote) FileOn(address, path string) (string, bool) {
	home := r.home(address)
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.Files[home][path]
	return f, ok
}

// SetEnv changes the environment file on a host behind the hub's back, as a hand edit would.
func (r *Remote) SetEnv(address, env string) {
	home := r.home(address)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Env[home] = env
}

// FailHash makes hashing the environment file fail on an address ("*": every one); nil clears it.
func (r *Remote) FailHash(address string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		delete(r.HashFail, address)
		return
	}
	r.HashFail[address] = err
}

// EnvOn is the environment file on a host, and whether it has one.
func (r *Remote) EnvOn(address string) (string, bool) {
	home := r.home(address)
	r.mu.Lock()
	defer r.mu.Unlock()
	env, ok := r.Env[home]
	return env, ok
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
	logs     map[string]cloudvm.LogResult // what the next Log returns, by body
	commands []string
	addrs    []string
	password string
}

// NewGame is a game that answers once Up is called.
func NewGame() *Game {
	return &Game{logs: map[string]cloudvm.LogResult{}, status: cloudvm.GameStatus{Build: "1.0", Map: "de_test", MaxPlayers: 10}}
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
	r := g.logs[body]
	r.Connected = connected
	g.logs[body] = r
}

// OnBoundary makes a log body announce a boundary (cloudvm.BoundaryMapChange, BoundaryMatchEnd).
func (g *Game) OnBoundary(body, boundary string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r := g.logs[body]
	r.Boundary = boundary
	g.logs[body] = r
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
func (g *Game) Log(body []byte) cloudvm.LogResult {
	g.mu.Lock()
	defer g.mu.Unlock()
	r := g.logs[string(body)]
	r.Connected = slices.Clone(r.Connected)
	return r
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
