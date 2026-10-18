// Package wardogs is the War Dogs stats adapter: it reads the batches the game's event feed
// pushed, as the hub stored them (ADR-0011), into stats events. The wire format is games/wardogs';
// this package only says what each event means to gravel.
package wardogs

import (
	"github.com/gravel-project/gravel/games/wardogs"
	"github.com/gravel-project/gravel/internal/ingest"
)

// Game is the game whose batches the parser reads: a batch's source is its server's game.
const Game = "wardogs"

// FeedVersion is the parser's version. Raise it with any change to what it reads, and the hub
// reads every stored batch again on its next start (a new kind, a fixed field).
//
//	1: match_state (2026-10-18, from the first capture).
const FeedVersion = 1

// KindMatchState is a match's state changing, with its map: extensions map, state and, when the
// event had one, game_time (the game's clock in seconds; it is not a wall-clock time, so the event
// is observed when its batch was received).
const KindMatchState = "match_state"

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
			if e.HasTime {
				ext["game_time"] = e.EventTime
			}
			out.Events = append(out.Events, ingest.Event{Key: e.EventID, Kind: KindMatchState, Extensions: ext})
		default:
			out.Unknown = append(out.Unknown, e.Type)
		}
	}
	return out, nil
}
