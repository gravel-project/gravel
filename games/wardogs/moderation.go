package wardogs

import "context"

// BroadcastResult is what POST /v1/broadcast answers.
type BroadcastResult struct {
	OK      bool   `json:"ok"`
	Pending bool   `json:"pending"`
	Message string `json:"message"`
}

// Kick removes a connected player with a reason they see.
func (c *Client) Kick(ctx context.Context, id SteamID, reason string) error {
	return c.call(ctx, CapKick, map[string]string{"reason": reason}, nil, string(id))
}

// Kill kills a connected player, who respawns.
func (c *Client) Kill(ctx context.Context, id SteamID) error {
	return c.call(ctx, CapKill, nil, nil, string(id))
}

// Message whispers to one connected player. The server caps the length (256 characters on
// CL-507060) and answers 400 message_too_long over it.
func (c *Client) Message(ctx context.Context, id SteamID, message string) error {
	return c.call(ctx, CapMessage, map[string]string{"message": message}, nil, string(id))
}

// Broadcast sends a message to everyone on the server, under the same cap as Message.
func (c *Client) Broadcast(ctx context.Context, message string) (BroadcastResult, error) {
	var out BroadcastResult
	return out, c.call(ctx, CapBroadcast, map[string]string{"message": message}, &out)
}

// MovePlayer moves a connected player to a faction, by the faction's name. The official console
// kills the player after a move so they respawn on the new side; that is the caller's choice.
func (c *Client) MovePlayer(ctx context.Context, id SteamID, faction string) error {
	return c.call(ctx, CapMovePlayer, map[string]string{"faction": faction}, nil, string(id))
}

// Ban bans a player who is on the server; the server answers player_not_found otherwise, and a
// ban of a player who is not on goes in the configuration document's DefaultBannedPlayerIds.
// An empty reason is omitted.
func (c *Client) Ban(ctx context.Context, id SteamID, reason string) error {
	body := map[string]string{"steamId": string(id)}
	if reason != "" {
		body["reason"] = reason
	}
	return c.call(ctx, CapBan, body, nil)
}

// Unban lifts a ban (ban_not_found when there is none).
func (c *Client) Unban(ctx context.Context, id SteamID) error {
	return c.call(ctx, CapUnban, nil, nil, string(id))
}
