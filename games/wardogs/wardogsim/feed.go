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
// What is captured (Hidden Token Gaming's server, Season 02): the envelope, two match_state events
// at the server's start (MatchState.PreMatch, then MatchState.PreMatch.WaitingForWorldLoad, with
// the map), a player_joined and a player_left for each player (playerSteamId, playerName,
// playerId, the count after, matchId, mapName), a match_ended (the winner, the factions' scores,
// randomWinner when nobody scored) with MatchState.PostMatch after it, that it posts only when it
// has events (nothing for hours on an empty server), and that it queues what it could not post and
// retries, reporting its counts at GET /v1/feed (deploy#49). A kill (killed) is the community's
// capture's fields (games/wardogs/feed.go); its items here are made up but for the AK-74M, and a
// death by an admin's kill posted as the world's is a guess (a lone player's, waiting for a
// match, posts nothing).
const (
	// feedEvery is how often a batch goes out while there are events to send.
	feedEvery = 2 * time.Second
	// feedBatch is the most events one batch carries.
	feedBatch = 10
	// feedBacklog is the most events kept while the sink is slow or away; older ones are dropped
	// and counted (droppedTotal). How many the real server holds is not known.
	feedBacklog = 1000
	// feedRetryMax is the longest wait between two tries of a failing sink: the real server's, once
	// a few quick tries failed, was about a minute (deploy#49).
	feedRetryMax = time.Minute
)

// killItems are what a simulated kill is by; only the AK-74M's id is the game's.
var killItems = []string{"Id.Item.AK74M", "Id.Item.M4A1", "Id.Item.SVD", "Id.Item.PKM"}

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
	// A killed's: the killer's fields are empty for a death nobody caused (an admin kill, the world).
	KillerSteamID string   `json:"killerSteamId,omitempty"`
	KillerName    string   `json:"killerName,omitempty"`
	KillerID      string   `json:"killerId,omitempty"`
	VictimSteamID string   `json:"victimSteamId,omitempty"`
	VictimName    string   `json:"victimName,omitempty"`
	VictimID      string   `json:"victimId,omitempty"`
	Cause         string   `json:"cause,omitempty"`
	Distance      *float64 `json:"distance,omitempty"`
	ContextTags   []string `json:"contextTags,omitempty"`
	// A match_ended's.
	WinningFaction string                     `json:"winningFaction,omitempty"`
	RandomWinner   bool                       `json:"randomWinner,omitempty"`
	FactionScores  []wardogs.FeedFactionScore `json:"factionScores,omitempty"`
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

// queue keeps a kill for the feed: by killer, or by nobody (nil). The caller holds mu.
func (s *Server) queue(t time.Time, killer, victim *player) {
	e := FeedEvent{Type: wardogs.FeedKilled, MapName: s.match.entry.level(), MatchID: s.match.id,
		VictimSteamID: victim.SteamID, VictimName: victim.Name, VictimID: playerID(victim.Player), ContextTags: []string{"Meta.PlayerKillFlag.Player.Local.Death"}}
	if killer != nil {
		distance := math.Round(math.Exp(3.5+s.rng.NormFloat64()*0.9)*1000) / 10 // centimetres
		e.KillerSteamID, e.KillerName, e.KillerID = killer.SteamID, killer.Name, playerID(killer.Player)
		e.Cause, e.Distance = killItems[s.rng.IntN(len(killItems))], &distance
		e.ContextTags = append(e.ContextTags, "Meta.PlayerKillFlag.Player.Local.Kill")
		if s.rng.Float64() < 0.2 {
			e.ContextTags = append([]string{wardogs.TagHeadshot}, e.ContextTags...)
		}
	}
	s.event(t, e)
}

// ended queues a match's end: its winner by score (drawn when nobody scored), the scores, and the
// post-match state. The caller holds mu.
func (s *Server) ended(t time.Time) {
	e := FeedEvent{Type: wardogs.FeedMatchEnded, MapName: s.match.entry.level(), MatchID: s.match.id}
	best := -1
	for i, f := range s.factions {
		score := s.match.scores[i]
		e.FactionScores = append(e.FactionScores, wardogs.FeedFactionScore{Faction: f.Name, Score: score})
		if score > 0 && (best < 0 || score > s.match.scores[best]) {
			best = i
		}
	}
	if best < 0 && len(s.factions) > 0 {
		best, e.RandomWinner = s.rng.IntN(len(s.factions)), true
	}
	if best >= 0 {
		e.WinningFaction = s.factions[best].Name
	}
	n := len(s.online)
	e.Players = &n
	s.event(t, e)
	s.event(t, FeedEvent{Type: wardogs.FeedMatchState, MapName: e.MapName, MatchID: e.MatchID, State: "MatchState.PostMatch"})
}

// playerID is the game's own id for a player, as the recorder's stand-ins look.
func playerID(p *Player) string { return fmt.Sprintf("00000000-0000-4000-8001-%012d", p.N) }

// playerEvent queues a player's joining or leaving, with how many are on after. The caller holds mu.
func (s *Server) playerEvent(t time.Time, kind string, p *Player) {
	n := len(s.online)
	s.event(t, FeedEvent{Type: kind, MapName: s.match.entry.level(), MatchID: s.match.id, PlayerSteamID: p.SteamID,
		PlayerName: p.Name, PlayerID: playerID(p), Players: &n})
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
	var lastPost, retryAt time.Time
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
		if target != "" && len(s.events) > 0 && now.Sub(lastPost) >= feedEvery && !now.Before(retryAt) {
			n := min(feedBatch, len(s.events))
			batch = &FeedBatch{ServerID: s.serverID, ServerName: s.live.serverName, Events: append([]FeedEvent{}, s.events[:n]...)}
			s.events = s.events[n:]
		}
		s.mu.Unlock()
		if batch == nil {
			continue
		}
		lastPost = now
		code, err := s.post(ctx, target, token, *batch)
		s.mu.Lock()
		s.delivery.lastStatus = code
		if err != nil {
			// Back to the front of the queue, to be tried again: quickly at first, then about once
			// a minute, as the real server did.
			s.events = append(batch.Events, s.events...)
			if over := len(s.events) - feedBacklog; over > 0 {
				s.events = s.events[over:]
				s.dropped += over
			}
			s.delivery.failures++
			retryAt = now.Add(min(feedEvery<<min(s.delivery.failures, 6), feedRetryMax))
			if s.delivery.failures == 1 {
				s.logger.Warn("wardogsim: feed batch not delivered; kept to try again", "events", len(batch.Events), "error", err.Error())
			}
		} else {
			if s.delivery.failures > 0 {
				s.logger.Info("wardogsim: feed delivering again", "after_failures", s.delivery.failures)
			}
			s.delivery.failures, s.delivery.lastSuccess, retryAt = 0, now, time.Time{}
			s.delivery.sent += len(batch.Events)
		}
		s.mu.Unlock()
	}
}

// post sends one batch, and says the sink's status (0 when none came back).
func (s *Server) post(ctx context.Context, target, token string, batch FeedBatch) (int, error) {
	body, err := json.Marshal(batch)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := s.feed.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("the sink answered %s", resp.Status)
	}
	return resp.StatusCode, nil
}

// Pending is how many events wait for the feed, and how many were dropped because the backlog was
// full.
func (s *Server) Pending() (waiting, dropped int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events), s.dropped
}
