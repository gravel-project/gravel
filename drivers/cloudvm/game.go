package cloudvm

import (
	"context"
	"log/slog"

	"github.com/gravel-project/gravel/drivers"
)

// Game is the game-side control of a server on a VM the hub made, in domain terms: the game's own
// channels (RCON and the log it pushes). drivers/cloudvm/cs2 adapts games/cs2 to it; another game
// is another adapter, registered by the Game spec's id.
type Game interface {
	// Status reads the game's state; its rows carry the identities the log stream gave them.
	Status(ctx context.Context) (GameStatus, error)
	// Kick removes the player with the game's user id, who sees the reason.
	Kick(ctx context.Context, userID int, reason string) error
	// Say sends a chat line to everyone.
	Say(ctx context.Context, message string) error
	// Quit stops the game; its unit on the VM starts it again (updating it first).
	Quit(ctx context.Context) error
	// SetPassword replaces the control password (a rotation).
	SetPassword(password string)
	// Log reads a batch the game pushed: who connected in it, and the last boundary it announced.
	Log(body []byte) LogResult
	// Find is a connected player by identity, as the log knows them.
	Find(id drivers.Identity) (GamePlayer, bool)
	// Forget clears what the log told it: the game's host went away.
	Forget()
}

// LogResult is what a pushed log batch told the hub.
type LogResult struct {
	// Connected are the players who connected, in order: the moment a ban is enforced.
	Connected []GamePlayer
	// Boundary is the last point in the batch where a resize disconnects nobody mid-game
	// (BoundaryMapChange, BoundaryMatchEnd); "" for none.
	Boundary string
}

// GameStatus is one status read.
type GameStatus struct {
	Build      string
	Map        string
	Hostname   string
	Players    int
	Bots       int
	MaxPlayers int
	Rows       []GamePlayer
}

// GamePlayer is a player on the game.
type GamePlayer struct {
	UserID int
	Name   string
	// Identity is the player's identity at the game's provider; empty when the log has not
	// named them yet.
	Identity drivers.Identity
	Team     string
	Kills    int
	Deaths   int
	PingMs   int
}

// GameFactory builds a game's control for a server: its control address (host:port), password,
// the game's profile, and a logger.
type GameFactory func(addr, password string, p Profile, logger *slog.Logger) (Game, error)
