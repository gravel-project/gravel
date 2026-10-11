// Package cs2 adapts games/cs2 (RCON and the log roster) to the cloud-VM driver's Game: status
// with each player's identity from the log, kicks by the game's user id, chat, quit.
package cs2

import (
	"context"
	"log/slog"
	"strconv"

	"github.com/gravel-project/gravel/drivers"
	"github.com/gravel-project/gravel/drivers/cloudvm"
	cs2 "github.com/gravel-project/gravel/games/cs2"
)

// Provider is the identity provider CS2's players are keyed by.
const Provider = "steam"

// Game is one CS2 server's control.
type Game struct {
	c      *cs2.Client
	roster *cs2.Roster
}

var _ cloudvm.Game = (*Game)(nil)

// New is a cloudvm.GameFactory.
func New(addr, password string, p cloudvm.Profile, _ *slog.Logger) (cloudvm.Game, error) {
	c, err := cs2.New(addr, password, cs2.Options{})
	if err != nil {
		return nil, err
	}
	r, err := cs2.NewRoster(p.LogTimezone)
	if err != nil {
		return nil, err
	}
	return &Game{c: c, roster: r}, nil
}

func identity(steamID64 uint64) drivers.Identity {
	if steamID64 == 0 {
		return drivers.Identity{}
	}
	return drivers.Identity{Provider: Provider, Subject: strconv.FormatUint(steamID64, 10)}
}

// Status reads status and puts the log's identities on its rows.
func (g *Game) Status(ctx context.Context) (cloudvm.GameStatus, error) {
	st, err := g.c.Status(ctx)
	if err != nil {
		return cloudvm.GameStatus{}, err
	}
	out := cloudvm.GameStatus{Build: st.Version, Map: st.Map, Hostname: st.Hostname, Players: st.Humans, Bots: st.Bots, MaxPlayers: st.MaxPlayers}
	for _, p := range st.Players {
		row := cloudvm.GamePlayer{UserID: p.UserID, Name: p.Name, PingMs: p.PingMs}
		if rp, ok := g.roster.ByUserID(p.UserID); ok {
			row.Identity, row.Team, row.Kills, row.Deaths = identity(rp.SteamID64), rp.Side, rp.Kills, rp.Deaths
		} else if p.Address == "" {
			continue // a bot
		}
		out.Rows = append(out.Rows, row)
	}
	return out, nil
}

// Kick implements cloudvm.Game.
func (g *Game) Kick(ctx context.Context, userID int, reason string) error {
	return g.c.Kick(ctx, userID, reason)
}

// Say implements cloudvm.Game.
func (g *Game) Say(ctx context.Context, m string) error { return g.c.Say(ctx, m) }

// Quit implements cloudvm.Game.
func (g *Game) Quit(ctx context.Context) error { return g.c.Quit(ctx) }

// SetPassword implements cloudvm.Game.
func (g *Game) SetPassword(p string) { g.c.SetPassword(p) }

// Log reads a pushed batch into the roster and returns who connected.
func (g *Game) Log(body []byte) []cloudvm.GamePlayer {
	var out []cloudvm.GamePlayer
	for _, c := range g.roster.PushBatch(body) {
		if p := c.Connected; p != nil {
			out = append(out, cloudvm.GamePlayer{UserID: p.UserID, Name: p.Name, Identity: identity(p.SteamID64)})
		}
	}
	return out
}

// Find implements cloudvm.Game.
func (g *Game) Find(id drivers.Identity) (cloudvm.GamePlayer, bool) {
	if id.Provider != Provider {
		return cloudvm.GamePlayer{}, false
	}
	n, err := strconv.ParseUint(id.Subject, 10, 64)
	if err != nil {
		return cloudvm.GamePlayer{}, false
	}
	p, ok := g.roster.BySteamID64(n)
	if !ok {
		return cloudvm.GamePlayer{}, false
	}
	return cloudvm.GamePlayer{UserID: p.UserID, Name: p.Name, Identity: id, Team: p.Side, Kills: p.Kills, Deaths: p.Deaths}, true
}

// Forget implements cloudvm.Game.
func (g *Game) Forget() { g.roster.Clear() }
