// Package wardogs is the War Dogs driver: drivers.ExternalReachable over the games/wardogs RCON
// client. It translates; the client keeps the server's rules (one token, never retried after a
// 401; Retry-After; capabilities matched by route shape).
package wardogs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

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

// Kick removes a connected player.
func (d *Driver) Kick(ctx context.Context, player drivers.Identity, reason string) error {
	id, err := steamID(player)
	if err != nil {
		return err
	}
	return mapError(d.c.Kick(ctx, id, reason))
}

// Kill kills a connected player.
func (d *Driver) Kill(ctx context.Context, player drivers.Identity) error {
	id, err := steamID(player)
	if err != nil {
		return err
	}
	return mapError(d.c.Kill(ctx, id))
}

// Message whispers to a connected player.
func (d *Driver) Message(ctx context.Context, player drivers.Identity, message string) error {
	id, err := steamID(player)
	if err != nil {
		return err
	}
	return mapError(d.c.Message(ctx, id, message))
}

// Broadcast sends a message to everyone on the server.
func (d *Driver) Broadcast(ctx context.Context, message string) error {
	_, err := d.c.Broadcast(ctx, message)
	return mapError(err)
}

// MovePlayer moves a connected player to a faction.
func (d *Driver) MovePlayer(ctx context.Context, player drivers.Identity, team string) error {
	id, err := steamID(player)
	if err != nil {
		return err
	}
	return mapError(d.c.MovePlayer(ctx, id, team))
}

// Ban bans a connected player through POST /v1/bans; the server refuses one who is not on.
func (d *Driver) Ban(ctx context.Context, player drivers.Identity, reason string) error {
	id, err := steamID(player)
	if err != nil {
		return err
	}
	return mapError(d.c.Ban(ctx, id, reason))
}

// Unban lifts a ban.
func (d *Driver) Unban(ctx context.Context, player drivers.Identity) error {
	id, err := steamID(player)
	if err != nil {
		return err
	}
	return mapError(d.c.Unban(ctx, id))
}

// Bans are the server's bans, those from its configuration document included.
func (d *Driver) Bans(ctx context.Context) ([]drivers.Ban, error) {
	b, err := d.c.Bans(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]drivers.Ban, 0, len(b.Bans))
	for _, x := range b.Bans {
		ban := drivers.Ban{
			Identity: drivers.Identity{Provider: "steam", Subject: strings.TrimSpace(string(x.SteamID))},
			By:       x.BannedBy, Reason: x.Reason,
		}
		if at, ok := x.BannedAt(); ok {
			ban.At = at.UTC().Truncate(time.Second)
		}
		out = append(out, ban)
	}
	return out, nil
}

// steamID is a player's SteamID64; War Dogs keys players by nothing else.
func steamID(p drivers.Identity) (wardogs.SteamID, error) {
	if p.Provider != "steam" {
		return "", &drivers.RejectedError{Code: "invalid_player", Err: fmt.Errorf("war dogs keys players by steam, not %q", p.Provider)}
	}
	s := p.Subject
	if len(s) != 17 || strings.Trim(s, "0123456789") != "" {
		return "", &drivers.RejectedError{Code: "invalid_player", Err: fmt.Errorf("%q is not a SteamID64", s)}
	}
	return wardogs.SteamID(s), nil
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	var rl *wardogs.RateLimitedError
	var api *wardogs.APIError
	switch {
	case errors.Is(err, wardogs.ErrTokenRefused):
		return fmt.Errorf("%w: %w", drivers.ErrCredentialRefused, err)
	case errors.Is(err, wardogs.ErrNoToken):
		return fmt.Errorf("%w: %w", drivers.ErrCredentialMissing, err)
	case errors.Is(err, wardogs.ErrNotSupported):
		return fmt.Errorf("%w: %w", drivers.ErrNotSupported, err)
	case errors.As(err, &rl):
		return fmt.Errorf("%w: %w", drivers.ErrRateLimited, err)
	case wardogs.IsCode(err, "player_not_found"):
		return fmt.Errorf("%w: %w", drivers.ErrPlayerNotFound, err)
	case wardogs.IsCode(err, "ban_not_found"):
		return fmt.Errorf("%w: %w", drivers.ErrBanNotFound, err)
	case errors.Is(err, wardogs.ErrBodyTooLarge):
		return &drivers.RejectedError{Code: "body_too_large", Err: err}
	case errors.As(err, &api) && (api.StatusCode == http.StatusBadRequest || api.StatusCode == http.StatusUnprocessableEntity):
		return &drivers.RejectedError{Code: api.Code, Err: err}
	}
	return err
}
