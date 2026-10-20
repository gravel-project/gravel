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
	"sync"
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
	c      *wardogs.Client
	logger *slog.Logger

	mu   sync.Mutex
	last *wardogs.Capabilities // the previous read, to log a route diff when it changes
	// rotation is the rotation's maps by index, read at most once a RotationTTL: the status names
	// only the positions, and the rotation changes at a match end or a config apply. Its own
	// lock, held across the read, so a slow read never holds up a capabilities read.
	rotMu       sync.Mutex
	rotation    map[int]string
	rotationAt  time.Time
	rotationErr bool // the last read failed; logged once until one succeeds
	// serverID is the host's identifier players join by, read at most once a JoinTTL.
	idMu  sync.Mutex
	id    string
	idAt  time.Time
	idErr bool
	// feeds is the server's feed delivery, read at most once a FeedTTL.
	feedMu  sync.Mutex
	feeds   []drivers.FeedDelivery
	feedAt  time.Time
	feedErr bool
	now     func() time.Time
}

// RotationTTL is how long the driver keeps the rotation's maps before reading them again.
const RotationTTL = time.Minute

// JoinTTL is how long the driver keeps the server's ID before reading it again: the host sets it,
// and it survives restarts and map changes (War Dogs patch 0.11).
const JoinTTL = 10 * time.Minute

// FeedTTL is how long the driver keeps the feed's delivery before reading it again: the gaps job
// compares it once a minute.
const FeedTTL = time.Minute

// joinHint says where a War Dogs player uses the ID (tested 2026-10-11, wardogs-server#8): no
// launch URI or steam://connect joins a server; the Community browser's Join by ID does.
const joinHint = "Paste it into Join by ID at the bottom left of the Community browser."

var _ drivers.ExternalReachable = (*Driver)(nil)

// Factory is New as a drivers.Factory (the registry's type).
func Factory(t drivers.Target) (drivers.Driver, error) { return New(t) }

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
	return &Driver{c: c, logger: logger, now: time.Now}, nil
}

// Build reads the capabilities, which name the build, and keeps them for the calls.
func (d *Driver) Build(ctx context.Context) (string, error) {
	caps, err := d.read(ctx)
	if err != nil {
		return "", err
	}
	return caps.Build, nil
}

// Capabilities re-reads what the server serves and maps it to the domain's set.
func (d *Driver) Capabilities(ctx context.Context) ([]string, error) {
	caps, err := d.read(ctx)
	if err != nil {
		return nil, err
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

// read fetches the capabilities (the client keeps them for its route lookups) and logs
// how the routes moved since the previous read: the detail behind a changed capability set, in
// the game's own terms, for whoever re-records the fixtures (games/wardogs/README.md).
func (d *Driver) read(ctx context.Context) (wardogs.Capabilities, error) {
	caps, err := d.c.Capabilities(ctx)
	if err != nil {
		return wardogs.Capabilities{}, mapError(err)
	}
	d.mu.Lock()
	prev := d.last
	d.last = &caps
	d.mu.Unlock()
	if prev != nil {
		if diff := wardogs.DiffRoutes(*prev, caps); !diff.Empty() || prev.Build != caps.Build {
			d.logger.Info("war dogs routes read again", "from_build", prev.Build, "to_build", caps.Build,
				"added", diff.Added, "removed", diff.Removed, "renamed", diff.Renamed, "unrecognised", caps.Unrecognised())
		}
	}
	return caps, nil
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
	maps := d.rotationMaps(ctx)
	out.RotationMap, out.NextRotationMap = maps[s.Rotation.NowIndex], maps[s.Rotation.NextIndex]
	if id := d.serverID(ctx); id != "" {
		out.Join = drivers.Join{Method: drivers.JoinByID, Value: id, Hint: joinHint}
	}
	out.Feeds = d.feedDelivery(ctx)
	return out, nil
}

// feedDelivery is the feed's delivery per endpoint (GET /v1/feed, Season 02), read again once
// FeedTTL has passed. A build without the route, or a feed that is off, says nothing; a failed
// read keeps the last good one and never fails the status.
func (d *Driver) feedDelivery(ctx context.Context) []drivers.FeedDelivery {
	d.feedMu.Lock()
	defer d.feedMu.Unlock()
	if !d.feedAt.IsZero() && d.now().Sub(d.feedAt) < FeedTTL {
		return d.feeds
	}
	d.feedAt = d.now()
	f, err := d.c.Feed(ctx)
	switch {
	case errors.Is(err, wardogs.ErrNotSupported):
		d.feeds = nil
		return nil
	case err != nil:
		if !d.feedErr {
			d.logger.Warn("war dogs feed delivery unreadable; the status goes without it", "error", err.Error())
		}
		d.feedErr = true
		return d.feeds
	}
	if d.feedErr {
		d.logger.Info("war dogs feed delivery readable again")
	}
	d.feedErr = false
	d.feeds = nil
	if f.Enabled {
		for _, e := range f.Endpoints {
			d.feeds = append(d.feeds, drivers.FeedDelivery{Host: e.Host, Healthy: e.Healthy, Queued: e.Queued + e.InFlight, Sent: e.SentTotal, Dropped: e.DroppedTotal})
		}
	}
	return d.feeds
}

// serverID is the host's identifier for the server, read again once JoinTTL has passed. Like the
// rotation's names, a build without the route or a failed read never fails the status: no route
// means no join, a failed read keeps the last good ID.
func (d *Driver) serverID(ctx context.Context) string {
	d.idMu.Lock()
	defer d.idMu.Unlock()
	if !d.idAt.IsZero() && d.now().Sub(d.idAt) < JoinTTL {
		return d.id
	}
	d.idAt = d.now()
	r, err := d.c.ServerID(ctx)
	switch {
	case errors.Is(err, wardogs.ErrNotSupported):
		d.id = ""
		return ""
	case err != nil:
		if !d.idErr {
			d.logger.Warn("war dogs server id unreadable; the status goes without its join id", "error", err.Error())
		}
		d.idErr = true
		return d.id
	}
	if d.idErr {
		d.logger.Info("war dogs server id readable again")
	}
	d.idErr = false
	d.id = strings.TrimSpace(r.ServerID)
	return d.id
}

// rotationMaps is the rotation's map names by index, read again once RotationTTL has passed. A
// build without the route, or a failed read, leaves the names out of the status and never fails
// it: the status is what matters, and the next read is a minute away.
func (d *Driver) rotationMaps(ctx context.Context) map[int]string {
	d.rotMu.Lock()
	defer d.rotMu.Unlock()
	if !d.rotationAt.IsZero() && d.now().Sub(d.rotationAt) < RotationTTL {
		return d.rotation
	}
	d.rotationAt = d.now()
	r, err := d.c.Rotation(ctx)
	switch {
	case errors.Is(err, wardogs.ErrNotSupported):
		d.rotation = nil
		return nil
	case err != nil:
		if !d.rotationErr {
			d.logger.Warn("war dogs rotation unreadable; the status goes without its map names", "error", err.Error())
		}
		d.rotationErr = true
		return d.rotation // the last good read, if any: a rotation rarely changes
	}
	if d.rotationErr {
		d.logger.Info("war dogs rotation readable again")
	}
	d.rotationErr = false
	d.rotation = make(map[int]string, len(r.Entries))
	for _, e := range r.Entries {
		d.rotation[e.Index] = e.Map
	}
	return d.rotation
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
