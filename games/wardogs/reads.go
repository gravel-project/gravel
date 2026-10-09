package wardogs

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"time"
)

// SteamID is a player's SteamID64 as the server writes it. It reads a JSON string or number,
// and an object with a steamId field (a reserved-slot entry that grew fields), so a build that
// changes the encoding does not break the list.
type SteamID string

// UnmarshalJSON reads "765…", 765… or {"steamId": …}.
func (s *SteamID) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case bytes.Equal(b, []byte("null")):
		return nil
	case len(b) > 0 && b[0] == '"':
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*s = SteamID(v)
		return nil
	case len(b) > 0 && b[0] == '{':
		var v struct {
			SteamID SteamID `json:"steamId"`
		}
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*s = v.SteamID
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*s = SteamID(n.String())
	return nil
}

// Health is GET /v1/health (public).
type Health struct {
	Status string `json:"status"`
	// UptimeSeconds resets when the host restarts the server (daily).
	UptimeSeconds int64 `json:"uptimeSeconds"`
	Connections   struct {
		Active int `json:"active"`
	} `json:"connections"`
	GameThreadQueue struct {
		InFlight int `json:"inFlight"`
		Depth    int `json:"depth"`
		// RejectedTotal rising means the server is shedding API work.
		RejectedTotal int64 `json:"rejectedTotal"`
	} `json:"gameThreadQueue"`
}

// Status is GET /v1/status: the match in progress.
type Status struct {
	ServerName string `json:"serverName"`
	// Map is the level's name ("Ozeti"), not the catalog's map id ("Europe") that the rotation
	// and the catalog use.
	Map         string         `json:"map"`
	Experiences []string       `json:"experiences"`
	Lighting    string         `json:"lighting"`
	Alternator  string         `json:"alternator"`
	ScoreTick   ScoreTick      `json:"scoreTick"`
	Players     PlayerCount    `json:"players"`
	Factions    []FactionScore `json:"factionScores"`
	Rotation    struct {
		NowIndex  int `json:"nowIndex"`
		NextIndex int `json:"nextIndex"`
	} `json:"rotation"`
}

// ScoreTick is the score period in force and its band.
type ScoreTick struct {
	Current int `json:"current"`
	Min     int `json:"min"`
	Max     int `json:"max"`
}

// PlayerCount is who is on; Max is MaxPlayers less MaxReservedSlots.
type PlayerCount struct {
	Current int `json:"current"`
	Max     int `json:"max"`
}

// FactionScore is one faction's score.
type FactionScore struct {
	Name     string `json:"name"`
	ColorHex string `json:"colorHex"`
	Score    int64  `json:"score"`
}

// Players is GET /v1/players.
type Players struct {
	Players []Player `json:"players"`
	Count   int      `json:"count"`
}

// Player is one connected player.
type Player struct {
	Name    string  `json:"name"`
	SteamID SteamID `json:"steamId"`
	Faction string  `json:"faction"`
	Kills   int     `json:"kills"`
	Deaths  int     `json:"deaths"`
	Cash    int64   `json:"cash"`
	PingMs  int     `json:"pingMs"`
}

// Rotation is GET /v1/rotation: the map rotation as the server sees it.
type Rotation struct {
	Enabled bool            `json:"enabled"`
	Mode    string          `json:"mode"` // "ordered" or "random"
	Entries []RotationEntry `json:"entries"`
	Count   int             `json:"count"`
}

// RotationEntry is one map in the rotation.
type RotationEntry struct {
	Index          int      `json:"index"`
	Map            string   `json:"map"`
	Experiences    []string `json:"experiences"`
	Lighting       string   `json:"lighting"`
	ZoneAlternator string   `json:"zoneAlternator"`
	// Status is "now", "next" or empty.
	Status string `json:"status"`
	// Denied is an entry the server skips (it names something the catalog lacks).
	Denied bool `json:"denied"`
}

// Bans is GET /v1/bans.
type Bans struct {
	Bans  []Ban `json:"bans"`
	Count int   `json:"count"`
}

// Ban is one ban. A ban from the configuration document reads back with BannedBy "config", no
// reason and a zero time.
type Ban struct {
	SteamID     SteamID `json:"steamId"`
	BannedAtUTC string  `json:"bannedAtUtc"`
	BannedBy    string  `json:"bannedBy"`
	Reason      string  `json:"reason"`
}

// BannedAt parses BannedAtUTC; ok is false for the zero time of a config-sourced ban or a
// format this client does not know.
func (b Ban) BannedAt() (time.Time, bool) { return parseTime(b.BannedAtUTC) }

// ReservedSlots is GET /v1/reserved-slots: the SteamIDs that may take a held-back slot.
type ReservedSlots struct {
	ReservedSlots []SteamID `json:"reservedSlots"`
	Count         int       `json:"count"`
}

// ServerID is GET /v1/server-id: the host's identifier for the server, "" when unset.
type ServerID struct {
	ServerID string `json:"serverId"`
}

// Sponsor is GET /v1/sponsor: the banner the server last accepted.
type Sponsor struct {
	ImageURL string `json:"imageUrl"`
}

// Audit is GET /v1/audit: the newest entries of the server's admin log, a ring buffer of 500
// that the hosts' own pollers fill in minutes, so it cannot answer what happened last week.
type Audit struct {
	Limit   int          `json:"limit"`
	Entries []AuditEntry `json:"entries"`
	Count   int          `json:"count"`
}

// AuditEntry is one admin-log line.
type AuditEntry struct {
	TimestampUTC string `json:"timestampUtc"`
	Peer         string `json:"peer"`
	SessionID    string `json:"sessionId"`
	Event        string `json:"event"` // ACCEPT, AUTH_OK, HTTP, …
	Detail       string `json:"detail"`
}

// Time parses TimestampUTC.
func (e AuditEntry) Time() (time.Time, bool) { return parseTime(e.TimestampUTC) }

func parseTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.Year() <= 1 {
		return time.Time{}, false
	}
	return t, true
}

// Health is the server's health (public; no token needed).
func (c *Client) Health(ctx context.Context) (Health, error) {
	var out Health
	return out, c.call(ctx, CapHealth, nil, &out)
}

// Status is the match in progress.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var out Status
	return out, c.call(ctx, CapStatus, nil, &out)
}

// Players are the connected players.
func (c *Client) Players(ctx context.Context) (Players, error) {
	var out Players
	return out, c.call(ctx, CapPlayers, nil, &out)
}

// Rotation is the map rotation.
func (c *Client) Rotation(ctx context.Context) (Rotation, error) {
	var out Rotation
	return out, c.call(ctx, CapRotation, nil, &out)
}

// Bans are the server's bans.
func (c *Client) Bans(ctx context.Context) (Bans, error) {
	var out Bans
	return out, c.call(ctx, CapBans, nil, &out)
}

// ReservedSlots are the SteamIDs on the reserved list.
func (c *Client) ReservedSlots(ctx context.Context) (ReservedSlots, error) {
	var out ReservedSlots
	return out, c.call(ctx, CapReservedSlots, nil, &out)
}

// ServerID is the host's identifier for the server.
func (c *Client) ServerID(ctx context.Context) (ServerID, error) {
	var out ServerID
	return out, c.call(ctx, CapServerID, nil, &out)
}

// Sponsor is the banner in force.
func (c *Client) Sponsor(ctx context.Context) (Sponsor, error) {
	var out Sponsor
	return out, c.call(ctx, CapSponsor, nil, &out)
}

// Audit is the newest limit entries of the admin log (the server allows 1–500; 0 asks for its
// default).
func (c *Client) Audit(ctx context.Context, limit int) (Audit, error) {
	r, err := c.route(ctx, CapAudit)
	if err != nil {
		return Audit{}, err
	}
	var q url.Values
	if limit > 0 {
		q = url.Values{"limit": {strconv.Itoa(limit)}}
	}
	var out Audit
	return out, c.getJSON(ctx, r, q, &out)
}
