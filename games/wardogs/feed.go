package wardogs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// The event feed: the server posts its events as JSON to each endpoint it is given, with the
// endpoint's token as a bearer, and never retries. Up to CL-509546 the endpoint was the
// [WDServerFeed] section and the server appended FeedPath to its Url; from Season 02 (CL-516586)
// it is the RCON section's EventFeedEndpoints, set in the server's own file, and the server posts
// to the Url as given, so the Url carries the path.
//
// The shape below is the first capture (2026-10-18, CL-516586): a batch names the server and
// carries its events; every event has an id, a type and a time. Only match_state events have been
// seen so far (the server's start, with nobody on); a kill's fields are not known yet, so an event
// of any other type is kept as posted (Raw) for a reader that knows it.
const (
	// FeedPath is what CL-509546 appended to the feed's Url.
	FeedPath = "/api/ingest/events"
	// FeedMatchState is a match's state changing, the map with it.
	FeedMatchState = "match_state"
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
