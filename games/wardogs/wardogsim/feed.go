package wardogsim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"
)

// The kill feed: War Dogs' [WDServerFeed] posts kill batches to <Url>/api/ingest/events with
// `Authorization: Bearer <Token>` (ADR-0011), and the simulator does too.
//
// THE BATCH SHAPE IS A GUESS. No real batch is recorded yet (hidden-token-gaming/deploy#49
// captures them), and what is known comes from community captures, not a primary source: batches
// of 1 to 10 events about every 2 s, no retry, and per-event fields eventId, an optional
// killerSteamId and distance. The body below is a JSON array of such events with victimSteamId
// and timestampUtc added; everything about it lives in this file, so a captured batch replaces it
// here and nowhere else. The hub stores batches as received and parses none yet, so the guess
// cannot leak into the stats.
const (
	// FeedPath is what the game appends to the section's Url.
	FeedPath = "/api/ingest/events"
	// feedEvery is how often a batch goes out while there are kills to send.
	feedEvery = 2 * time.Second
	// feedBatch is the most events one batch carries.
	feedBatch = 10
	// feedBacklog is the most events kept while the sink is slow or away; older ones are dropped,
	// as a source that never retries loses them.
	feedBacklog = 1000
)

// KillEvent is one kill as the simulator posts it (the guessed shape above).
type KillEvent struct {
	EventID string `json:"eventId"`
	// TimestampUTC is when the kill happened.
	TimestampUTC string `json:"timestampUtc"`
	// KillerSteamID is empty for a death nobody caused (an admin kill, the world).
	KillerSteamID string  `json:"killerSteamId,omitempty"`
	VictimSteamID string  `json:"victimSteamId"`
	Distance      float64 `json:"distance"`
}

// queue keeps a kill for the feed when the feed is on (as read at the last restart). The caller
// holds mu.
func (s *Server) queue(t time.Time, killer, victim string) {
	if s.live.feedURL == "" {
		return
	}
	s.eventSeq++
	distance := math.Round(math.Exp(3.5+s.rng.NormFloat64()*0.9)*10) / 10
	s.events = append(s.events, KillEvent{
		EventID:      fmt.Sprintf("%s-%d-%d", s.build, s.booted.Unix(), s.eventSeq),
		TimestampUTC: t.UTC().Format("2006-01-02T15:04:05.000Z"), KillerSteamID: killer, VictimSteamID: victim, Distance: distance,
	})
	if over := len(s.events) - feedBacklog; over > 0 {
		s.events = s.events[over:]
		s.dropped += over
	}
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
		url, token := s.live.feedURL, s.live.feedToken
		var batch []KillEvent
		if url != "" && now.Sub(lastPost) >= feedEvery && len(s.events) > 0 {
			n := min(feedBatch, len(s.events))
			batch = append(batch, s.events[:n]...)
			s.events = s.events[n:]
		}
		s.mu.Unlock()
		if batch == nil {
			continue
		}
		lastPost = now
		if err := s.post(ctx, url, token, batch); err != nil {
			if !failing {
				s.logger.Warn("wardogsim: feed batch not delivered; the feed does not retry", "events", len(batch), "error", err.Error())
			}
			failing = true
		} else if failing {
			s.logger.Info("wardogsim: feed delivering again")
			failing = false
		}
	}
}

// post sends one batch. The sink's answer is not read beyond its status: the game does not retry.
func (s *Server) post(ctx context.Context, url, token string, batch []KillEvent) error {
	body, err := json.Marshal(batch)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+FeedPath, bytes.NewReader(body))
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

// Pending is how many kills wait for the feed, and how many were dropped because the backlog was
// full.
func (s *Server) Pending() (waiting, dropped int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events), s.dropped
}
