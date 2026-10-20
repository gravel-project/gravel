package wardogs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// The event feed: the server posts its events as JSON to each endpoint it is given, with the
// endpoint's token as a bearer. Up to CL-509546 the endpoint was the [WDServerFeed] section and
// the server appended FeedPath to its Url; from Season 02 (CL-516586) it is the RCON section's
// EventFeedEndpoints, set in the server's own file, and the server posts to the Url as given, so
// the Url carries the path.
//
// It queues and retries: with its endpoint answering 503 for nine minutes (2026-10-20, CL-516586)
// the server held a join and a leave, retried about once a minute, and posted both in one batch
// 19 seconds after the endpoint came back, each with its own eventTime; it dropped nothing
// (GET /v1/feed's droppedTotal, FeedEndpointStatus). How much it holds is not known.
//
// The shape below is Hidden Token Gaming's captures (from 2026-10-18, CL-516586): a batch names
// the server and carries its events; every event has an id, a type and a time. The feed posts only
// when it has events (nothing for hours on an empty server). Captured: match_state (the server's
// start, then waiting for players, with the match's id from then on), player_joined/player_left
// (one player, with the count after) and match_ended (the winner and the factions' scores). A
// kill (killed) has not been captured here: its fields are the War Dogs community's capture of
// CL-501228 (Warcon's docs/wardogs-api.md and its parser, 2026-09-16, unchanged by its Season 02
// update of 2026-10-15). An event of any other type is kept as posted (Raw) for a reader that
// knows it.
const (
	// FeedPath is what CL-509546 appended to the feed's Url.
	FeedPath = "/api/ingest/events"
	// FeedMatchState is a match's state changing, the map with it.
	FeedMatchState = "match_state"
	// FeedPlayerJoined and FeedPlayerLeft are one player coming onto the server and leaving it.
	FeedPlayerJoined = "player_joined"
	FeedPlayerLeft   = "player_left"
	// FeedKilled is one player killed, by another, by themself or by the world.
	FeedKilled = "killed"
	// FeedMatchEnded is a match's end, with its winner and the factions' scores.
	FeedMatchEnded = "match_ended"
)

// Kill context tags (a killed event's contextTags) gravel reads; the event carries others.
const (
	// TagHeadshot is a kill by a shot to the head.
	TagHeadshot = "Meta.Progression.Context.Player.KillContext.Headshot"
	// TagSuicide is a player killing themself.
	TagSuicide = "Meta.PlayerKillFlag.Player.Suicide"
)

// FeedBatch is one request the feed posts.
type FeedBatch struct {
	// ServerID is the host's identifier for the server (GET /v1/server-id; the Join-by-ID).
	ServerID string `json:"serverId"`
	// ServerName is EventFeedServerName from the server's file: a label of the host's choosing.
	ServerName string      `json:"serverName"`
	Events     []FeedEvent `json:"events"`
}

// FeedEvent is one event of a batch: the fields every event has, and the event as posted.
type FeedEvent struct {
	EventID string
	Type    string
	// EventTime is the game's clock in seconds (0.132 at the server's start), not a wall-clock
	// time; HasTime is false when the event carried none the reader could read.
	EventTime float64
	HasTime   bool
	// Raw is the event as posted, from which a type's own fields are read (MatchState).
	Raw json.RawMessage
}

// UnmarshalJSON reads the common fields and keeps the rest. A field it cannot read (a time that
// is not a number) is left unset rather than failing the batch: the feed changes with the game.
func (e *FeedEvent) UnmarshalJSON(b []byte) error {
	var head struct {
		EventID   json.RawMessage `json:"eventId"`
		Type      json.RawMessage `json:"type"`
		EventTime json.RawMessage `json:"eventTime"`
	}
	if err := json.Unmarshal(b, &head); err != nil {
		return err
	}
	*e = FeedEvent{Raw: append(json.RawMessage(nil), b...)}
	_ = json.Unmarshal(head.EventID, &e.EventID)
	_ = json.Unmarshal(head.Type, &e.Type)
	if json.Unmarshal(head.EventTime, &e.EventTime) == nil && len(head.EventTime) > 0 && !bytes.Equal(head.EventTime, []byte("null")) {
		e.HasTime = true
	}
	return nil
}

// MarshalJSON writes the event as posted.
func (e FeedEvent) MarshalJSON() ([]byte, error) {
	if len(e.Raw) == 0 {
		return nil, errors.New("wardogs: a feed event with nothing posted")
	}
	return e.Raw, nil
}

// MatchState is a match_state event's own fields.
type MatchState struct {
	// MapName is the level ("Kavkazi"), as the status's map is.
	MapName string `json:"mapName"`
	// State is the engine's match state: "MatchState.PreMatch", "MatchState.PreMatch.WaitingForWorldLoad".
	State string `json:"state"`
	// MatchID is the match's id, from the wait for players on; "" before it (the server's start).
	MatchID string `json:"matchId"`
}

// MatchState reads a match_state event's fields.
func (e FeedEvent) MatchState() (MatchState, error) {
	if e.Type != FeedMatchState {
		return MatchState{}, fmt.Errorf("wardogs: a %q event is not a %s", e.Type, FeedMatchState)
	}
	var m MatchState
	if err := json.Unmarshal(e.Raw, &m); err != nil {
		return MatchState{}, fmt.Errorf("wardogs: %s %s: %w", FeedMatchState, e.EventID, err)
	}
	return m, nil
}

// PlayerChange is a player_joined or player_left event's own fields.
type PlayerChange struct {
	MatchID string `json:"matchId"`
	MapName string `json:"mapName"`
	// PlayerSteamID is who; PlayerName is their name in the game then, and PlayerID the game's own
	// id for them, which gravel does not read (players are known by SteamID).
	PlayerSteamID SteamID `json:"playerSteamId"`
	PlayerName    string  `json:"playerName"`
	PlayerID      string  `json:"playerId"`
	// Players is how many are on after the change; nil when the event carried no count.
	Players *int `json:"players"`
}

// PlayerChange reads a player_joined or player_left event's fields.
func (e FeedEvent) PlayerChange() (PlayerChange, error) {
	if e.Type != FeedPlayerJoined && e.Type != FeedPlayerLeft {
		return PlayerChange{}, fmt.Errorf("wardogs: a %q event is not a %s or a %s", e.Type, FeedPlayerJoined, FeedPlayerLeft)
	}
	var p PlayerChange
	if err := json.Unmarshal(e.Raw, &p); err != nil {
		return PlayerChange{}, fmt.Errorf("wardogs: %s %s: %w", e.Type, e.EventID, err)
	}
	return p, nil
}

// Kill is a killed event's own fields. The killer's are absent when the world killed (a fall, a
// vehicle's explosion), Cause on a fall, Distance on an explosion and most suicides. Neither side's
// faction is sent: a team kill is the poll's to tell.
type Kill struct {
	MatchID string `json:"matchId"`
	MapName string `json:"mapName"`
	// KillerSteamID is who killed, "" for the world; KillerName and KillerID are their name in
	// the game then and the game's own id for them, which gravel does not read.
	KillerSteamID SteamID `json:"killerSteamId"`
	KillerName    string  `json:"killerName"`
	KillerID      string  `json:"killerId"`
	// VictimSteamID is who died; VictimName and VictimID as the killer's.
	VictimSteamID SteamID `json:"victimSteamId"`
	VictimName    string  `json:"victimName"`
	VictimID      string  `json:"victimId"`
	// Cause is the item that killed ("Id.Item.AK74M"); "" when the event carried none.
	Cause string `json:"cause"`
	// Distance is how far, in the engine's centimetres; nil when the event carried none.
	Distance *float64 `json:"distance"`
	// ContextTags are the kill's gameplay tags (TagHeadshot, TagSuicide, …).
	ContextTags []string `json:"contextTags"`
}

// Suicide is whether the victim killed themself: the tag, or the killer the victim.
func (k Kill) Suicide() bool {
	return slices.Contains(k.ContextTags, TagSuicide) || (k.KillerSteamID != "" && k.KillerSteamID == k.VictimSteamID)
}

// Headshot is whether the kill was a shot to the head.
func (k Kill) Headshot() bool { return slices.Contains(k.ContextTags, TagHeadshot) }

// Kill reads a killed event's fields.
func (e FeedEvent) Kill() (Kill, error) {
	if e.Type != FeedKilled {
		return Kill{}, fmt.Errorf("wardogs: a %q event is not a %s", e.Type, FeedKilled)
	}
	var k Kill
	if err := json.Unmarshal(e.Raw, &k); err != nil {
		return Kill{}, fmt.Errorf("wardogs: %s %s: %w", FeedKilled, e.EventID, err)
	}
	return k, nil
}

// MatchEnded is a match_ended event's own fields.
type MatchEnded struct {
	MatchID string `json:"matchId"`
	MapName string `json:"mapName"`
	// WinningFaction is the faction that won ("Valkyra").
	WinningFaction string `json:"winningFaction"`
	// RandomWinner is true when nobody scored and the server drew the winner (an empty match
	// running out, 2026-10-19).
	RandomWinner bool `json:"randomWinner"`
	// FactionScores are each faction's score at the end.
	FactionScores []FeedFactionScore `json:"factionScores"`
	// Players is how many were on at the end; nil when the event carried no count.
	Players *int `json:"players"`
}

// FeedFactionScore is one faction's score in a feed event: the faction by name, where the
// status's FactionScore says name.
type FeedFactionScore struct {
	Faction string `json:"faction"`
	Score   int64  `json:"score"`
}

// MatchEnded reads a match_ended event's fields.
func (e FeedEvent) MatchEnded() (MatchEnded, error) {
	if e.Type != FeedMatchEnded {
		return MatchEnded{}, fmt.Errorf("wardogs: a %q event is not a %s", e.Type, FeedMatchEnded)
	}
	var m MatchEnded
	if err := json.Unmarshal(e.Raw, &m); err != nil {
		return MatchEnded{}, fmt.Errorf("wardogs: %s %s: %w", FeedMatchEnded, e.EventID, err)
	}
	return m, nil
}

// ParseFeedBatch reads a batch. It fails only when the body is not a JSON object or its events
// are not a list of objects; unknown fields and types are kept for the reader to judge.
func ParseFeedBatch(body []byte) (FeedBatch, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 || body[0] != '{' {
		return FeedBatch{}, errors.New("wardogs: a feed batch is a JSON object")
	}
	var b FeedBatch
	if err := json.Unmarshal(body, &b); err != nil {
		return FeedBatch{}, fmt.Errorf("wardogs: feed batch: %w", err)
	}
	return b, nil
}
