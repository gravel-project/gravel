// Package drivers is the seam between the hub and the servers it controls (ADR-0010). A driver
// is written in domain terms; drivers/<game> adapts a game's client library (games/<game>) to it,
// and nothing outside that package imports the game's wire format.
//
// Driver is what the hub's monitor and moderation need from any server: its build, the
// capabilities it grants right now, its status and players, and moderation (Moderator). The
// capabilities are a set, never a constant: a game whose API changes between builds loses a
// capability instead of failing, and the pages and the bot show "not available".
//
// ExternalReachable is the driver for a server gravel can reach but does not run (RCON, SSH):
// a Driver that also reads and writes the server's configuration document (Configurer). The
// cloud-VM driver (ADR-0003, drivers/cloudvm) runs the server on a VM the hub made; it has no
// configuration document and enforces the hub's ban list itself (BanList).
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
	// CapPark and CapResize are a server whose host the hub owns (ADR-0003): it can park the
	// host when nobody plays and resize it between sizes.
	CapPark   = "park"
	CapResize = "resize"
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
	// ErrHostNotRunning is a server whose host is not up: parked, being made, or failing. A
	// HostStateError carries the phase; the monitor records it and polls nothing.
	ErrHostNotRunning = errors.New("drivers: the server's host is not running")
)

// Host phases a HostStateError names.
const (
	HostParked       = "parked"
	HostProvisioning = "provisioning"
	HostError        = "error"
)

// HostStateError is ErrHostNotRunning with the host's phase (HostParked, …).
type HostStateError struct{ Phase string }

func (e *HostStateError) Error() string { return "drivers: the server's host is " + e.Phase }

// Unwrap makes the error ErrHostNotRunning.
func (e *HostStateError) Unwrap() error { return ErrHostNotRunning }

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
	// RotationMap and NextRotationMap are the rotation's current and next map as the game names
	// them in its rotation (War Dogs: the theatre, where Map is the level); empty when unknown.
	RotationMap     string
	NextRotationMap string
	// Join is how a player gets onto the server from the game; zero when the driver knows no way.
	Join Join
}

// Join is how a player reaches the server from the game: Method says what Value is, Hint says
// where to use it, in the game's own terms. It is public: it is what a community shares to bring
// players in.
type Join struct {
	// Method is JoinByID (paste Value in the game's own server browser), JoinByAddress (Value is
	// host:port) or JoinByURI (Value is a link that launches the game onto the server); "" is none.
	Method string
	Value  string
	// Hint is one line saying where Value goes: "Paste it into Join by ID in the Community browser."
	Hint string
}

// The join methods.
const (
	JoinByID      = "id"
	JoinByAddress = "address"
	JoinByURI     = "uri"
)

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

// Driver controls one server: what the monitor polls and moderation calls.
type Driver interface {
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

// ExternalReachable controls a server gravel reaches over the network and does not run: a
// Driver with the server's configuration document.
type ExternalReachable interface {
	Driver
	Configurer
}

// BanList is a driver that enforces the hub's ban list itself (the cloud-VM driver kicks a banned
// identity when it connects). The hub owns the list and hands it over whole, after every change
// and to every new driver.
type BanList interface {
	SetBans(ctx context.Context, bans []Identity) error
}

// Target is what a driver is built for.
type Target struct {
	// ServerID and Game name the server, as the manifest does.
	ServerID string
	Game     string
	// Endpoint is the server's control address (an RCON origin); "" for a driver that learns the
	// address itself (the cloud-VM driver, which made the host).
	Endpoint string
	// Credential is the secret the manifest's file held; never logged.
	Credential string
	// UserAgent names the hub in the server's logs.
	UserAgent string
	Logger    *slog.Logger
}

// Factory builds a driver.
type Factory func(Target) (Driver, error)

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
