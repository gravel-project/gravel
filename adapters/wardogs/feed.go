// Package wardogs is the War Dogs stats adapter: it reads the batches the game's event feed
// pushed, as the hub stored them (ADR-0011), into stats events. The wire format is games/wardogs';
// this package only says what each event means to gravel.
package wardogs

import (
	"regexp"

	"github.com/gravel-project/gravel/games/wardogs"
	"github.com/gravel-project/gravel/internal/ingest"
	"github.com/gravel-project/gravel/internal/store"
)

// Game is the game whose batches the parser reads: a batch's source is its server's game.
const Game = "wardogs"

// FeedVersion is the parser's version. Raise it with any change to what it reads, and the hub
// reads every stored batch again on its next start (a new kind, a fixed field).
//
//	1: match_state (2026-10-18, from the first capture).
//	2: player_joined and player_left, and a match_state's match_id (2026-10-19, from the
//	   first session with a player on).
const FeedVersion = 2

// The kinds. Each has, when the event had one, game_time (the game's clock in seconds; it is not
// a wall-clock time, so the event is observed when its batch was received).
const (
	// KindMatchState is a match's state changing: extensions map, state and match_id (from the
	// wait for players on).
	KindMatchState = "match_state"
	// KindPlayerJoined and KindPlayerLeft are one player, by SteamID, coming onto the server and
	// leaving it: extensions map, match_id and players (how many are on after). The player's name
	// and the game's own id for them are not kept: who they are is resolved at read time.
	KindPlayerJoined = "player_joined"
	KindPlayerLeft   = "player_left"
)

// steamProvider is the identity provider a SteamID belongs to (README principle 5).
const steamProvider = "steam"

// steamID is a SteamID64 as the feed sends it.
var steamID = regexp.MustCompile(`^\d{17}$`)

// Feed is the feed's parser.
type Feed struct{}

var _ ingest.Parser = Feed{}

// Version implements ingest.Parser.
func (Feed) Version() int { return FeedVersion }

// Parse implements ingest.Parser. A batch that is not a feed batch fails. Inside one, an event
// with no id (it cannot be deduplicated), no type, or fields of a known type that cannot be read
// is invalid; one of a type this version does not know is left for a later one.
func (Feed) Parse(b ingest.Batch) (ingest.Parsed, error) {
	batch, err := wardogs.ParseFeedBatch(b.Body)
	if err != nil {
		return ingest.Parsed{}, err
	}
	var out ingest.Parsed
	for _, e := range batch.Events {
		if e.EventID == "" || e.Type == "" {
			out.Invalid++
			continue
		}
		switch e.Type {
		case wardogs.FeedMatchState:
			m, err := e.MatchState()
			if err != nil || m.State == "" {
				out.Invalid++
				continue
			}
			ext := map[string]any{"map": m.MapName, "state": m.State}
			if m.MatchID != "" {
				ext["match_id"] = m.MatchID
			}
			out.Events = append(out.Events, event(e, KindMatchState, store.PlayerKey{}, ext))
		case wardogs.FeedPlayerJoined, wardogs.FeedPlayerLeft:
			p, err := e.PlayerChange()
			if err != nil || !steamID.MatchString(string(p.PlayerSteamID)) {
				out.Invalid++
				continue
			}
			ext := map[string]any{"map": p.MapName}
			if p.MatchID != "" {
				ext["match_id"] = p.MatchID
			}
			if p.Players != nil {
				ext["players"] = *p.Players
			}
			kind := KindPlayerJoined
			if e.Type == wardogs.FeedPlayerLeft {
				kind = KindPlayerLeft
			}
			out.Events = append(out.Events, event(e, kind, store.PlayerKey{Provider: steamProvider, Subject: string(p.PlayerSteamID)}, ext))
		default:
			out.Unknown = append(out.Unknown, e.Type)
		}
	}
	return out, nil
}

// event is one stats event of a feed event: keyed by its eventId, with its game time when it had one.
func event(e wardogs.FeedEvent, kind string, player store.PlayerKey, ext map[string]any) ingest.Event {
	if e.HasTime {
		ext["game_time"] = e.EventTime
	}
	return ingest.Event{Key: e.EventID, Kind: kind, Player: player, Extensions: ext}
}
