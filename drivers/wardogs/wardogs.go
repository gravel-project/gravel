// Package wardogs is the War Dogs driver: drivers.ExternalReachable over the games/wardogs RCON
// client. It translates; the client keeps the server's rules (one token, never retried after a
// 401; Retry-After; capabilities matched by route shape).
package wardogs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/gravel-project/gravel/drivers"
	"github.com/gravel-project/gravel/games/wardogs"
)

// Name is the driver's name in servers.yaml.
const Name = "wardogs"

// capabilities maps the client's capabilities to the domain's. A domain capability holds when
// every client capability it needs does.
var capabilities = map[string][]wardogs.Capability{
	drivers.CapStatus:      {wardogs.CapStatus},
	drivers.CapPlayers:     {wardogs.CapPlayers},
	drivers.CapKick:        {wardogs.CapKick},
	drivers.CapKill:        {wardogs.CapKill},
	drivers.CapMessage:     {wardogs.CapMessage},
	drivers.CapBroadcast:   {wardogs.CapBroadcast},
	drivers.CapMovePlayer:  {wardogs.CapMovePlayer},
	drivers.CapBans:        {wardogs.CapBans},
	drivers.CapBan:         {wardogs.CapBan},
	drivers.CapUnban:       {wardogs.CapUnban},
	drivers.CapRotation:    {wardogs.CapRotation},
	drivers.CapConfigRead:  {wardogs.CapConfigRead},
	drivers.CapConfigWrite: {wardogs.CapConfigRead, wardogs.CapConfigValidate, wardogs.CapConfigWrite},
}

// Driver is one War Dogs server.
type Driver struct {
	c *wardogs.Client
}

var _ drivers.ExternalReachable = (*Driver)(nil)

// New builds a driver; nothing is sent until the first call.
func New(t drivers.Target) (drivers.ExternalReachable, error) {
	logger := t.Logger
	if logger == nil {
		logger = slog.Default()
	}
	c, err := wardogs.New(t.Endpoint, wardogs.Options{
		Token:     t.Credential,
		UserAgent: t.UserAgent,
		OnMismatch: func(route string, err error) {
			logger.Warn("war dogs answer has a field of an unexpected type; kept the rest", "route", route, "error", err.Error())
		},
	})
	if err != nil {
		return nil, err
	}
	return &Driver{c: c}, nil
}

// Build reads the public capabilities, which name the build, and keeps them for the calls.
func (d *Driver) Build(ctx context.Context) (string, error) {
	caps, err := d.c.Capabilities(ctx)
	if err != nil {
		return "", mapError(err)
	}
	return caps.Build, nil
}

// Capabilities re-reads what the server serves and maps it to the domain's set.
func (d *Driver) Capabilities(ctx context.Context) ([]string, error) {
	caps, err := d.c.Capabilities(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := []string{} // empty, not nil: the server answered and grants nothing
	for domain, need := range capabilities {
		ok := true
		for _, c := range need {
			ok = ok && caps.Supports(c)
		}
		if ok {
			out = append(out, domain)
		}
	}
	slices.Sort(out)
	return out, nil
}

// Status is the match in progress.
func (d *Driver) Status(ctx context.Context) (drivers.Status, error) {
	s, err := d.c.Status(ctx)
	if err != nil {
		return drivers.Status{}, mapError(err)
	}
	out := drivers.Status{
		Name:              s.ServerName,
		Map:               s.Map,
		Experiences:       s.Experiences,
		Lighting:          s.Lighting,
		Players:           s.Players.Current,
		MaxPlayers:        s.Players.Max,
		ScoreTick:         s.ScoreTick.Current,
		RotationIndex:     s.Rotation.NowIndex,
		NextRotationIndex: s.Rotation.NextIndex,
	}
	for _, f := range s.Factions {
		out.Teams = append(out.Teams, drivers.TeamScore{Name: f.Name, Color: f.ColorHex, Score: f.Score})
	}
	return out, nil
}

// Players are the connected players, keyed by SteamID64.
func (d *Driver) Players(ctx context.Context) ([]drivers.Player, error) {
	p, err := d.c.Players(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]drivers.Player, 0, len(p.Players))
	for _, pl := range p.Players {
		out = append(out, drivers.Player{
			Name:     pl.Name,
			Identity: drivers.Identity{Provider: "steam", Subject: strings.TrimSpace(string(pl.SteamID))},
			Team:     pl.Faction,
			Kills:    pl.Kills,
			Deaths:   pl.Deaths,
			PingMs:   pl.PingMs,
		})
	}
	return out, nil
}

// SetCredential replaces the RCON password and lifts a refusal.
func (d *Driver) SetCredential(credential string) { d.c.SetToken(credential) }

func mapError(err error) error {
	var rl *wardogs.RateLimitedError
	switch {
	case errors.Is(err, wardogs.ErrTokenRefused):
		return fmt.Errorf("%w: %w", drivers.ErrCredentialRefused, err)
	case errors.Is(err, wardogs.ErrNoToken):
		return fmt.Errorf("%w: %w", drivers.ErrCredentialMissing, err)
	case errors.Is(err, wardogs.ErrNotSupported):
		return fmt.Errorf("%w: %w", drivers.ErrNotSupported, err)
	case errors.As(err, &rl):
		return fmt.Errorf("%w: %w", drivers.ErrRateLimited, err)
	}
	return err
}
