// Package drivers is the seam between the hub and the servers it controls (ADR-0010). A driver
// is written in domain terms; drivers/<game> adapts a game's client library (games/<game>) to it,
// and nothing outside that package imports the game's wire format.
//
// ExternalReachable is the driver for a server gravel can reach but does not run (RCON, SSH).
// Its capabilities are a set the server grants right now, never a constant: a game whose API
// changes between builds loses a capability instead of failing, and the pages and the bot show
// "not available". Moderation joined with its procedures (Moderator); configuration joins with
// its own.
package drivers

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"
)

// Capabilities a driver may report, in domain terms.
const (
	CapStatus      = "status"
	CapPlayers     = "players"
	CapKick        = "kick"
	CapKill        = "kill"
	CapMessage     = "message"
	CapBroadcast   = "broadcast"
	CapMovePlayer  = "move_player"
	CapBans        = "bans"
	CapBan         = "ban"
	CapUnban       = "unban"
	CapRotation    = "rotation"
	CapConfigRead  = "config_read"
	CapConfigWrite = "config_write"
)

// Errors a driver returns, wrapping the game client's own.
var (
	// ErrNotSupported is a capability the server does not grant now.
	ErrNotSupported = errors.New("drivers: the server does not offer this")
	// ErrCredentialRefused is the server refusing the credential. A driver sends nothing that
	// needs it again until SetCredential gives it another.
	ErrCredentialRefused = errors.New("drivers: the server refused the credential")
	// ErrCredentialMissing is a call that needs a credential on a driver without one.
	ErrCredentialMissing = errors.New("drivers: no credential")
	// ErrRateLimited is the server asking the driver to wait.
	ErrRateLimited = errors.New("drivers: rate limited by the server")
	// ErrPlayerNotFound is a moderation call naming a player who is not on the server.
	ErrPlayerNotFound = errors.New("drivers: the player is not on the server")
	// ErrBanNotFound is an unban of a player the server holds no ban for.
	ErrBanNotFound = errors.New("drivers: the server holds no ban for the player")
	// ErrRejected is a request the server, or the driver before sending, refused as invalid: a
	// message over the game's length cap, an identity the game does not key players by.
	ErrRejected = errors.New("drivers: the server rejected the request")
)

// Identity is a player's provider identity (principle 5: stats and moderation are keyed by it,
// never by a gravel user id; the hub resolves it to a member at read time).
type Identity struct {
	Provider string
	Subject  string
}

// Status is a server's state as one poll saw it.
type Status struct {
	Name        string
	Map         string
	Experiences []string
	Lighting    string
	Players     int
	MaxPlayers  int
	Teams       []TeamScore
	// ScoreTick is the score period in force, 0 when the game has none.
	ScoreTick int
	// RotationIndex and NextRotationIndex are the map rotation's position; -1 when unknown.
	RotationIndex     int
	NextRotationIndex int
}

// TeamScore is one team's (faction's) score.
type TeamScore struct {
	Name  string
	Color string
	Score int64
}

// Player is one connected player.
type Player struct {
	Name     string
	Identity Identity
	Team     string
	Kills    int
	Deaths   int
	PingMs   int
}

// RejectedError is ErrRejected with the game's own word for the refusal ("message_too_long"),
// which a caller may see; Err is the driver's error, which may name the server's address.
type RejectedError struct {
	Code string
	Err  error
}

func (e *RejectedError) Error() string {
	return "drivers: the server rejected the request: " + e.Err.Error()
}

// Unwrap makes the error both ErrRejected and the driver's.
func (e *RejectedError) Unwrap() []error { return []error{ErrRejected, e.Err} }

// Ban is one ban a server holds.
type Ban struct {
	Identity Identity
	// At is when the ban was made; zero when the server does not know (War Dogs: a ban from its
	// configuration document).
	At time.Time
	// By is who the server says made it (War Dogs: "config" for a configuration-document ban).
	By     string
	Reason string
}

// Moderator is what a moderator does to the players on a server (ADR-0010 §5). Each call needs
// its capability (CapKick, …) and fails with ErrNotSupported without it; a call naming a player
// who is not on fails with ErrPlayerNotFound. The hub audits every call; a driver only performs.
type Moderator interface {
	// Kick removes a connected player, who sees the reason.
	Kick(ctx context.Context, player Identity, reason string) error
	// Kill kills a connected player, who respawns.
	Kill(ctx context.Context, player Identity) error
	// Message whispers to one connected player.
	Message(ctx context.Context, player Identity, message string) error
	// Broadcast sends a message to everyone on the server.
	Broadcast(ctx context.Context, message string) error
	// MovePlayer moves a connected player to a team, by the team's name.
	MovePlayer(ctx context.Context, player Identity, team string) error
	// Ban bans a connected player.
	Ban(ctx context.Context, player Identity, reason string) error
	// Unban lifts a ban; ErrBanNotFound when there is none.
	Unban(ctx context.Context, player Identity) error
	// Bans are the bans the server holds.
	Bans(ctx context.Context) ([]Ban, error)
}

// ExternalReachable controls a server gravel reaches over the network and does not run.
type ExternalReachable interface {
	Moderator
	// Build is the server's build string, read from a route that needs no credential. A change
	// re-reads the capabilities.
	Build(ctx context.Context) (string, error)
	// Capabilities is the set the server grants, sorted; it re-reads what the server serves.
	Capabilities(ctx context.Context) ([]string, error)
	Status(ctx context.Context) (Status, error)
	Players(ctx context.Context) ([]Player, error)
	// SetCredential replaces the credential (a rotation) and lifts a refusal.
	SetCredential(credential string)
}

// Target is what a driver is built for.
type Target struct {
	// Endpoint is the server's control address (an RCON origin).
	Endpoint string
	// Credential is the secret the manifest's file held; never logged.
	Credential string
	// UserAgent names the hub in the server's logs.
	UserAgent string
	Logger    *slog.Logger
}

// Factory builds a driver.
type Factory func(Target) (ExternalReachable, error)

// Registry is the drivers a hub has, by name.
type Registry map[string]Factory

// Names are the registered drivers, sorted.
func (r Registry) Names() []string {
	out := make([]string, 0, len(r))
	for n := range r {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}
