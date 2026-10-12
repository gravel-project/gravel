package cloudvm

import (
	"context"
	"errors"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gravel-project/gravel/drivers"
)

// Capabilities a cloud-VM server has while its host runs. Kill, message and moving a player
// are not offered: CS2's console has no command for the first two that names a player safely,
// and teams are the players' own.
var Capabilities = []string{drivers.CapBan, drivers.CapBans, drivers.CapBroadcast, drivers.CapChat, drivers.CapKick, drivers.CapPark,
	drivers.CapPlayers, drivers.CapResize, drivers.CapStatus, drivers.CapUnban}

// Factory is the driver the hub's monitor builds for a cloud-VM server: a thin view of the
// Manager, which owns the host and its game control (the monitor rebuilds drivers on every
// manifest change; the host's state must outlive that).
func (m *Manager) Factory() drivers.Factory {
	return func(t drivers.Target) (drivers.Driver, error) {
		if t.ServerID == "" {
			return nil, errors.New("cloudvm: a driver needs its server's id")
		}
		return &Driver{m: m, id: t.ServerID}, nil
	}
}

// Driver is one cloud-VM server, as the monitor and moderation see it.
type Driver struct {
	m  *Manager
	id string

	mu     sync.Mutex
	last   GameStatus
	lastAt time.Time
}

var (
	_ drivers.Driver  = (*Driver)(nil)
	_ drivers.BanList = (*Driver)(nil)
)

// status reads the game, reusing a read from the last few seconds (a poll reads Status and then
// Players).
func (d *Driver) status(ctx context.Context) (GameStatus, error) {
	d.mu.Lock()
	if now := d.m.opt.Now(); now.Sub(d.lastAt) < 3*time.Second && !now.Before(d.lastAt) {
		st := d.last
		d.mu.Unlock()
		return st, nil
	}
	d.mu.Unlock()
	st, err := d.m.Status(ctx, d.id)
	if err != nil {
		return GameStatus{}, err
	}
	d.mu.Lock()
	d.last, d.lastAt = st, d.m.opt.Now()
	d.mu.Unlock()
	return st, nil
}

// Build is the game's build, from its status.
func (d *Driver) Build(ctx context.Context) (string, error) {
	st, err := d.status(ctx)
	return st.Build, err
}

// Capabilities are the driver's, while the host runs.
func (d *Driver) Capabilities(ctx context.Context) ([]string, error) {
	if _, err := d.status(ctx); err != nil {
		return nil, err
	}
	return slices.Clone(Capabilities), nil
}

// Status is the game's status with the address players join.
func (d *Driver) Status(ctx context.Context) (drivers.Status, error) {
	st, err := d.status(ctx)
	if err != nil {
		return drivers.Status{}, err
	}
	out := drivers.Status{Name: st.Hostname, Map: st.Map, Players: st.Players, MaxPlayers: st.MaxPlayers,
		RotationIndex: -1, NextRotationIndex: -1}
	h, _ := d.m.Host(d.id)
	_, s, ok := d.m.host(d.id)
	if ok {
		for _, t := range s.Profile.Teams {
			out.Teams = append(out.Teams, drivers.TeamScore{Name: t.Name, Key: t.Key})
		}
	}
	if ok && h.Address != "" && s.Profile.JoinPort > 0 {
		addr := net.JoinHostPort(h.Address, strconv.Itoa(s.Profile.JoinPort))
		out.Join = drivers.Join{Method: drivers.JoinByAddress, Value: addr, Hint: strings.ReplaceAll(s.Profile.JoinHint, "<address>", addr)}
	}
	return out, nil
}

// Players are the players on, with the identities the log gave them.
func (d *Driver) Players(ctx context.Context) ([]drivers.Player, error) {
	st, err := d.status(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]drivers.Player, 0, len(st.Rows))
	for _, p := range st.Rows {
		out = append(out, drivers.Player{Name: p.Name, Identity: p.Identity, Team: p.Team, Kills: p.Kills, Deaths: p.Deaths, PingMs: p.PingMs})
	}
	return out, nil
}

// SetCredential does nothing: the Manager reads the server's secrets on its own reconcile and
// pushes a changed one to the host.
func (d *Driver) SetCredential(string) {}

func (d *Driver) game(ctx context.Context) (Game, error) {
	if _, err := d.status(ctx); err != nil {
		return nil, err
	}
	rt, _, ok := d.m.host(d.id)
	if !ok {
		return nil, ErrNoSuchHost
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.game == nil {
		return nil, ErrNotRunning
	}
	return rt.game, nil
}

// Kick removes a connected player, found by identity in the log's roster.
func (d *Driver) Kick(ctx context.Context, player drivers.Identity, reason string) error {
	g, err := d.game(ctx)
	if err != nil {
		return err
	}
	p, ok := g.Find(player)
	if !ok {
		return drivers.ErrPlayerNotFound
	}
	return g.Kick(ctx, p.UserID, reason)
}

// Broadcast is a chat line from the server.
func (d *Driver) Broadcast(ctx context.Context, message string) error {
	g, err := d.game(ctx)
	if err != nil {
		return err
	}
	return g.Say(ctx, message)
}

// Ban kicks the player if they are on; the hub's list (SetBans) keeps them out.
func (d *Driver) Ban(ctx context.Context, player drivers.Identity, reason string) error {
	return d.Kick(ctx, player, "Banned: "+reason)
}

// Unban has nothing to lift on the game: the hub's list is the only list.
func (d *Driver) Unban(context.Context, drivers.Identity) error { return drivers.ErrBanNotFound }

// Bans are the identities the server kicks as they connect.
func (d *Driver) Bans(context.Context) ([]drivers.Ban, error) {
	rt, _, ok := d.m.host(d.id)
	if !ok {
		return nil, ErrNoSuchHost
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]drivers.Ban, 0, len(rt.bans))
	for id := range rt.bans {
		out = append(out, drivers.Ban{Identity: id, By: "hub"})
	}
	slices.SortFunc(out, func(a, b drivers.Ban) int { return strings.Compare(a.Identity.Subject, b.Identity.Subject) })
	return out, nil
}

// SetBans replaces the identities the server kicks as they connect.
func (d *Driver) SetBans(_ context.Context, bans []drivers.Identity) error {
	d.m.setBans(d.id, bans)
	return nil
}

// Kill is not offered.
func (d *Driver) Kill(context.Context, drivers.Identity) error { return drivers.ErrNotSupported }

// Message is not offered.
func (d *Driver) Message(context.Context, drivers.Identity, string) error {
	return drivers.ErrNotSupported
}

// MovePlayer is not offered.
func (d *Driver) MovePlayer(context.Context, drivers.Identity, string) error {
	return drivers.ErrNotSupported
}
