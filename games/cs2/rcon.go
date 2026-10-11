package cs2

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorcon/rcon"
)

// Errors the client returns.
var (
	// ErrAuth is the server refusing the RCON password.
	ErrAuth = errors.New("cs2: the server refused the rcon password")
	// ErrTooLong is a message or reason over what the client sends (MaxText).
	ErrTooLong = errors.New("cs2: the text is too long")
	// ErrInvalid is an argument the client refuses before sending (a map name that is not one).
	ErrInvalid = errors.New("cs2: invalid argument")
)

// MaxText is the longest message or reason the client sends, in bytes: CS2's chat line is short,
// and a console command has a hard limit (rcon.MaxCommandLen).
const MaxText = 254

// Conn is one authenticated RCON connection; *rcon.Conn is one.
type Conn interface {
	Execute(command string) (string, error)
	Close() error
}

// Options tune the client. Zero values take the defaults.
type Options struct {
	// DialTimeout bounds connecting and authenticating; 5 s by default.
	DialTimeout time.Duration
	// Deadline bounds one command's write and read; 5 s by default.
	Deadline time.Duration
	// Dial replaces the connection (tests).
	Dial func(ctx context.Context, addr, password string) (Conn, error)
}

// Client sends RCON commands to one CS2 server. Each command uses its own connection: CS2 drops
// idle RCON connections, and a fresh one keeps a truncated answer from bleeding into the next.
type Client struct {
	addr string
	opt  Options

	mu       sync.Mutex
	password string
}

// New returns a client for the server's RCON address (host:port, the game port over TCP).
// Nothing is sent until the first command.
func New(addr, password string, opt Options) (*Client, error) {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return nil, fmt.Errorf("cs2: rcon address %q: %w", addr, err)
	}
	if opt.DialTimeout <= 0 {
		opt.DialTimeout = rcon.DefaultDialTimeout
	}
	if opt.Deadline <= 0 {
		opt.Deadline = rcon.DefaultDeadline
	}
	return &Client{addr: addr, opt: opt, password: password}, nil
}

// SetPassword replaces the RCON password (a rotation).
func (c *Client) SetPassword(p string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.password = p
}

func (c *Client) dial(ctx context.Context) (Conn, error) {
	c.mu.Lock()
	password := c.password
	c.mu.Unlock()
	if c.opt.Dial != nil {
		return c.opt.Dial(ctx, c.addr, password)
	}
	d := net.Dialer{Timeout: c.opt.DialTimeout}
	nc, err := d.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = nc.SetDeadline(dl)
	}
	conn, err := rcon.Open(nc, password, rcon.SetDialTimeout(c.opt.DialTimeout), rcon.SetDeadline(c.opt.Deadline))
	if err != nil {
		_ = nc.Close()
		if errors.Is(err, rcon.ErrAuthFailed) {
			return nil, ErrAuth
		}
		return nil, err
	}
	return conn, nil
}

// Exec runs one console command and returns the server's answer.
func (c *Client) Exec(ctx context.Context, command string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	conn, err := c.dial(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := conn.Execute(command)
		done <- result{out, err}
	}()
	select {
	case r := <-done:
		return r.out, r.err
	case <-ctx.Done():
		_ = conn.Close() // unblocks Execute
		return "", ctx.Err()
	}
}

// Status runs "status" and parses it.
func (c *Client) Status(ctx context.Context) (Status, error) {
	out, err := c.Exec(ctx, "status")
	if err != nil {
		return Status{}, err
	}
	return ParseStatus(out)
}

// Kick removes the player with the server's user id (status and the log carry it); the player
// sees the reason.
func (c *Client) Kick(ctx context.Context, userID int, reason string) error {
	r, err := quoted(reason)
	if err != nil {
		return err
	}
	if userID < 0 {
		return fmt.Errorf("%w: user id %d", ErrInvalid, userID)
	}
	_, err = c.Exec(ctx, "kickid "+strconv.Itoa(userID)+" "+r)
	return err
}

// Say sends a chat message from the server to everyone.
func (c *Client) Say(ctx context.Context, message string) error {
	m, err := quoted(message)
	if err != nil {
		return err
	}
	_, err = c.Exec(ctx, "say "+m)
	return err
}

// Quit shuts the server down; its supervisor (the VM's systemd unit) starts it again, which runs
// SteamCMD's update first. The connection usually drops before an answer, which is success.
func (c *Client) Quit(ctx context.Context) error {
	_, err := c.Exec(ctx, "quit")
	if err != nil && isClosed(err) {
		return nil
	}
	return err
}

var mapRE = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// ChangeLevel loads a map now.
func (c *Client) ChangeLevel(ctx context.Context, m string) error {
	if !mapRE.MatchString(m) {
		return fmt.Errorf("%w: map %q", ErrInvalid, m)
	}
	_, err := c.Exec(ctx, "changelevel "+m)
	return err
}

// quoted makes text one quoted console argument: a quote or a semicolon would end the argument or
// start another command, and a control character breaks the line, so they are replaced.
func quoted(s string) (string, error) {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '"':
			return '\''
		case r == ';':
			return ','
		case r < 0x20, r == 0x7f:
			return ' '
		}
		return r
	}, strings.TrimSpace(s))
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("%w: not UTF-8", ErrInvalid)
	}
	if len(s) > MaxText {
		return "", ErrTooLong
	}
	return `"` + s + `"`, nil
}

func isClosed(err error) bool {
	if errors.Is(err, net.ErrClosed) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "EOF") || strings.Contains(s, "connection reset")
}
