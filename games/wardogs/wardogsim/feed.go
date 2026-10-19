package wardogsim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gravel-project/gravel/games/wardogs"
)

// The event feed: the server posts batches of events to the feed's Url with `Authorization:
// Bearer <Token>` (ADR-0011), and the simulator does too, in the shape games/wardogs/feed.go reads:
// `{serverId, serverName, events[]}`, each event with eventId, type and eventTime (the game's
// clock in seconds). A build with [WDServerFeed] (CL-509546) appends wardogs.FeedPath to its Url;
// Season 02 posts to the Url as given.
//
// What is captured (2026-10-18, Season 02): the envelope, two match_state events at the server's
// start (MatchState.PreMatch, then MatchState.PreMatch.WaitingForWorldLoad, with the map), a
// player_joined and a player_left for each player (playerSteamId, playerName, playerId, the count
// after, matchId, mapName), and that it posts only when it has events (nothing for hours on an
// empty server). What is NOT, and is a guess here: a kill (type "kill", killerSteamId,
// victimSteamId and distance, as community captures name them) and the states after the
// pre-match.
// A kill's fields come from the first session with players; until then the hub's parser keeps
// these as a kind it does not know.
const (
	// feedEvery is how often a batch goes out while there are events to send.
	feedEvery = 2 * time.Second
	// feedBatch is the most events one batch carries.
	feedBatch = 10
	// feedBacklog is the most events kept while the sink is slow or away; older ones are dropped,
	// as a source that never retries loses them.
	feedBacklog = 1000
)

// FeedEvent is one event as the simulator posts it.
type FeedEvent struct {
	EventID   string  `json:"eventId"`
	Type      string  `json:"type"`
	EventTime float64 `json:"eventTime"`
	// A match_state's (MapName and MatchID a player event's too).
	MapName string `json:"mapName,omitempty"`
	State   string `json:"state,omitempty"`
	MatchID string `json:"matchId,omitempty"`
	// A player_joined's or a player_left's: who, and how many are on after.
	PlayerSteamID string `json:"playerSteamId,omitempty"`
	PlayerName    string `json:"playerName,omitempty"`
	PlayerID      string `json:"playerId,omitempty"`
	Players       *int   `json:"players,omitempty"`
	// A kill's (guessed). KillerSteamID is empty for a death nobody caused (an admin kill, the
	// world).
	KillerSteamID string   `json:"killerSteamId,omitempty"`
	VictimSteamID string   `json:"victimSteamId,omitempty"`
	Distance      *float64 `json:"distance,omitempty"`
}

// FeedBatch is one batch as the simulator posts it.
type FeedBatch struct {
	ServerID string `json:"serverId"`
	// ServerName is the server's own name here; a real server sends its file's
	// EventFeedServerName, a label of the host's choosing.
	ServerName string      `json:"serverName"`
	Events     []FeedEvent `json:"events"`
}

// event appends an event for the feed when the feed is on (as read at the last restart). The
// caller holds mu.
func (s *Server) event(t time.Time, e FeedEvent) {
	if s.live.feedURL == "" {
		return
	}
	s.eventSeq++
	e.EventID = fmt.Sprintf("%s-%d-%d", s.build, s.booted.Unix(), s.eventSeq)
	e.EventTime = math.Round(t.Sub(s.booted).Seconds()*1000) / 1000
	s.events = append(s.events, e)
	if over := len(s.events) - feedBacklog; over > 0 {
		s.events = s.events[over:]
		s.dropped += over
	}
}

// queue keeps a kill for the feed. The caller holds mu.
func (s *Server) queue(t time.Time, killer, victim string) {
	distance := math.Round(math.Exp(3.5+s.rng.NormFloat64()*0.9)*10) / 10
	s.event(t, FeedEvent{Type: "kill", KillerSteamID: killer, VictimSteamID: victim, Distance: &distance})
}

// playerEvent queues a player's joining or leaving, with how many are on after. The caller holds mu.
func (s *Server) playerEvent(t time.Time, kind string, p *Player) {
	n := len(s.online)
	s.event(t, FeedEvent{Type: kind, MapName: s.match.entry.level(), MatchID: s.match.id, PlayerSteamID: p.SteamID,
		PlayerName: p.Name, PlayerID: fmt.Sprintf("00000000-0000-4000-8001-%012d", p.N), Players: &n})
}

// started queues the match states the server posts at its start, and gives the match the id its
// players' events carry. The caller holds mu.
func (s *Server) started(t time.Time) {
	s.match.id = fmt.Sprintf("%08x-0000-4000-8000-%012x", uint32(t.Unix()), s.match.index)
	level := s.match.entry.level()
	for _, state := range []string{"MatchState.PreMatch", "MatchState.PreMatch.WaitingForWorldLoad"} {
		s.event(t, FeedEvent{Type: wardogs.FeedMatchState, MapName: level, State: state})
	}
}

// feedTarget is where a batch goes: the Url as given, or with wardogs.FeedPath on a build whose
// feed is [WDServerFeed]. "" when the feed is off. The caller holds mu.
func (s *Server) feedTarget() string {
	if s.live.feedURL == "" {
		return ""
	}
	if _, section := s.schema(secFeed); section {
		return strings.TrimRight(s.live.feedURL, "/") + wardogs.FeedPath
	}
	return s.live.feedURL
}

// FeedTarget is where the feed posts now; "" when it is off.
func (s *Server) FeedTarget() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.feedTarget()
}

// Run moves the world on every second, whether or not anyone reads it, and posts the feed's
// batches; it returns when ctx is done.
func (s *Server) Run(ctx context.Context) error {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var lastPost time.Time
	failing := false
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		now := s.now()
		s.mu.Lock()
		s.advance(now)
		s.prunePeers(now)
		target, token := s.feedTarget(), s.live.feedToken
		var batch *FeedBatch
		if target != "" && len(s.events) > 0 && now.Sub(lastPost) >= feedEvery {
			n := min(feedBatch, len(s.events))
			batch = &FeedBatch{ServerID: s.serverID, ServerName: s.live.serverName, Events: append([]FeedEvent{}, s.events[:n]...)}
			s.events = s.events[n:]
		}
		s.mu.Unlock()
		if batch == nil {
			continue
		}
		lastPost = now
		if err := s.post(ctx, target, token, *batch); err != nil {
			if !failing {
				s.logger.Warn("wardogsim: feed batch not delivered; the feed does not retry", "events", len(batch.Events), "error", err.Error())
			}
			failing = true
		} else if failing {
			s.logger.Info("wardogsim: feed delivering again")
			failing = false
		}
	}
}

// post sends one batch. The sink's answer is not read beyond its status: the game does not retry.
func (s *Server) post(ctx context.Context, target, token string, batch FeedBatch) error {
	body, err := json.Marshal(batch)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := s.feed.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("the sink answered %s", resp.Status)
	}
	return nil
}

// Pending is how many events wait for the feed, and how many were dropped because the backlog was
// full.
func (s *Server) Pending() (waiting, dropped int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events), s.dropped
}
