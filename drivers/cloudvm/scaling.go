package cloudvm

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/gravel-project/gravel/cloud"
)

// Scaling is a server's vertical scaling policy (ADR-0003 §4, #37): the ladder its host walks one
// rung at a time as its population changes, and when. A decision is taken continuously from the
// players the status polls see and carried out only at a boundary the game announces (a map
// change, a match's end) or on an empty server, because a resize disconnects everyone for a few
// minutes (2½ for DigitalOcean's c-2 and c-4, measured on HTG's server).
type Scaling struct {
	// Ladder is the rungs, smallest first; the host's size is one of them.
	Ladder []Tier
	// UpAt and UpFor: a rung this full (players over its players, 0–1) for this long moves up.
	UpAt  float64
	UpFor time.Duration
	// DownAt and DownFor: a rung this empty for this long moves down.
	DownAt  float64
	DownFor time.Duration
	// Cooldown is the least time between two resizes.
	Cooldown time.Duration
}

// The policy's defaults (#37): up at 80 % for 5 minutes, down at 25 % for 30, no two resizes within
// 30 minutes, and a boundary counts for 2 minutes after the game announced it.
const (
	DefaultUpAt     = 0.8
	DefaultUpFor    = 5 * time.Minute
	DefaultDownAt   = 0.25
	DefaultDownFor  = 30 * time.Minute
	DefaultCooldown = 30 * time.Minute
	BoundaryWindow  = 2 * time.Minute
)

// Scale directions and the outcomes a decision ends in.
const (
	DirectionUp   = "up"
	DirectionDown = "down"

	// DetailScale is the event a decision records; DetailScaleCancelled one that no longer holds.
	DetailScale          = "scale"
	DetailScaleCancelled = "scale_cancelled"
)

// Boundaries a game announces in its log, where a resize may happen.
const (
	BoundaryMapChange = "map_change"
	BoundaryMatchEnd  = "match_end"
)

// WithDefaults fills a policy's zero values.
func (sc Scaling) WithDefaults() Scaling {
	if sc.UpAt == 0 {
		sc.UpAt = DefaultUpAt
	}
	if sc.UpFor == 0 {
		sc.UpFor = DefaultUpFor
	}
	if sc.DownAt == 0 {
		sc.DownAt = DefaultDownAt
	}
	if sc.DownFor == 0 {
		sc.DownFor = DefaultDownFor
	}
	if sc.Cooldown == 0 {
		sc.Cooldown = DefaultCooldown
	}
	return sc
}

// Rung is a size's place on a ladder, -1 when it is not on it.
func Rung(ladder []Tier, size string) int {
	return slices.IndexFunc(ladder, func(t Tier) bool { return t.Size == size })
}

// CheckLadder asks a provider whether a host made at start can walk a ladder both ways: every
// rung offered in the region, and every step up and down allowed from the root disk the host is
// made with (a resize never changes the disk, and a disk never shrinks: DigitalOcean's c-4 made
// as c-4 has 50 GB and can never come back to c-2's 25). The manifest's apply asks it, so a
// ladder that cannot be walked is refused when it is saved, never at a boundary.
func CheckLadder(sizes []cloud.Size, canResize func(from, to cloud.Size) error, region, start string, ladder []Tier) error {
	bySlug := map[string]cloud.Size{}
	for _, z := range sizes {
		bySlug[z.Slug] = z
	}
	var errs []error
	for _, t := range ladder {
		z, ok := bySlug[t.Size]
		if !ok {
			errs = append(errs, fmt.Errorf("%s is not a size this cloud offers", t.Size))
		} else if !slices.Contains(z.Regions, region) {
			errs = append(errs, fmt.Errorf("%s is not offered in %s", t.Size, region))
		}
	}
	first, ok := bySlug[start]
	if !ok || len(errs) > 0 {
		return errors.Join(errs...)
	}
	disk := first.DiskGiB
	for i := 0; i+1 < len(ladder); i++ {
		lo, hi := bySlug[ladder[i].Size], bySlug[ladder[i+1].Size]
		// The instance keeps the disk it was made with, whatever size it is now; the size it moves
		// to is the catalog's.
		from := lo
		from.DiskGiB = disk
		if err := canResize(from, hi); err != nil {
			errs = append(errs, fmt.Errorf("%s to %s: %w", lo.Slug, hi.Slug, err))
		}
		from = hi
		from.DiskGiB = disk
		if err := canResize(from, lo); err != nil {
			errs = append(errs, fmt.Errorf("%s back to %s with the %d GiB root disk a host made as %s has: %w", hi.Slug, lo.Slug, disk, start, err))
		}
	}
	return errors.Join(errs...)
}

// scale is the scaling policy's turn in a running host's tick: it keeps the time the room has
// been full or empty, takes a decision when one has lasted long enough, warns the players once,
// drops a decision the room no longer supports, and carries one out at a boundary by asking for
// the resize (the resizing state does the rest). It returns true when it asked for one.
func (t *tick) scale(ctx context.Context, h *Host) bool {
	sc := t.s.Scaling
	pending := h.PendingSize
	if sc == nil || h.PinnedSize != "" {
		if pending != "" {
			t.cancelScale(h, "the policy is off or the size is pinned")
		}
		t.m.met.setTier(t.s.ID, 0)
		return false
	}
	rung := Rung(sc.Ladder, h.Size)
	if rung < 0 {
		if pending != "" {
			t.cancelScale(h, "the host's size is not on the ladder")
		}
		t.rt.mu.Lock()
		warned := t.rt.offLadder == h.Size
		t.rt.offLadder = h.Size
		t.rt.mu.Unlock()
		if !warned {
			t.m.log.Warn("the host's size is not on its scaling ladder; the policy waits until it is", "server", h.ServerID, "size", h.Size)
		}
		t.m.met.setTier(t.s.ID, 0)
		return false
	}
	t.m.met.setTier(t.s.ID, sc.Ladder[rung].Players)
	// The status reads keep since when the rung has been full or empty enough (observe).
	t.rt.mu.Lock()
	players, known, boundaryAt, boundary := t.rt.players, t.rt.known, t.rt.boundaryAt, t.rt.boundary
	highSince, lowSince := t.rt.highSince, t.rt.lowSince
	t.rt.mu.Unlock()
	if !known {
		return false
	}
	capacity := sc.Ladder[rung].Players
	occupancy := float64(players) / float64(capacity)
	high, low := !highSince.IsZero(), !lowSince.IsZero()
	highFor, lowFor := t.now.Sub(highSince), t.now.Sub(lowSince)

	if pending != "" {
		up := Rung(sc.Ladder, pending) > rung
		switch {
		case Rung(sc.Ladder, pending) < 0:
			t.cancelScale(h, pending+" is no longer on the ladder")
		case up && occupancy <= sc.DownAt:
			t.cancelScale(h, fmt.Sprintf("the room emptied to %d of %d", players, capacity))
		case !up && occupancy >= sc.UpAt:
			t.cancelScale(h, fmt.Sprintf("the room filled to %d of %d", players, capacity))
		}
	}
	if h.PendingSize == "" && (h.LastResizeAt.IsZero() || t.now.Sub(h.LastResizeAt) >= sc.Cooldown) {
		var to, dir, why string
		switch {
		case high && highFor >= sc.UpFor:
			to, dir = sc.Ladder[rung+1].Size, DirectionUp
			why = fmt.Sprintf("%d of %d players for %s", players, capacity, highFor.Round(time.Second))
		case low && lowFor >= sc.DownFor:
			to, dir = sc.Ladder[rung-1].Size, DirectionDown
			why = fmt.Sprintf("%d of %d players for %s", players, capacity, lowFor.Round(time.Second))
		}
		if to != "" {
			h.PendingSize, h.PendingSince = to, t.now
			h.PendingReason = fmt.Sprintf("%s: %s, to %s (%d players) at the next boundary", dir, why, to, sc.Ladder[Rung(sc.Ladder, to)].Players)
			t.events = append(t.events, Event{ServerID: h.ServerID, At: t.now, From: h.State, To: h.State, Step: DetailScale, Detail: h.PendingReason, Size: h.Size})
			t.m.met.decisions.WithLabelValues(h.ServerID, dir, "decided").Inc()
			t.m.met.setPending(h.ServerID, dir)
			t.m.log.Info("scaling decision", "server", h.ServerID, "reason", h.PendingReason)
			if players > 0 {
				t.say(ctx, sayScale(dir, sc.Ladder[Rung(sc.Ladder, to)].Players))
			}
		}
	}
	if h.PendingSize == "" {
		return false
	}
	atBoundary := players == 0 || (boundary != "" && !boundaryAt.Before(h.PendingSince) && t.now.Sub(boundaryAt) <= BoundaryWindow)
	if !atBoundary {
		return false
	}
	dir := DirectionDown
	if Rung(sc.Ladder, h.PendingSize) > rung {
		dir = DirectionUp
	}
	at := "the server is empty"
	if players > 0 {
		at = "a " + map[string]string{BoundaryMapChange: "map change", BoundaryMatchEnd: "match's end"}[boundary]
	}
	t.m.log.Info("scaling at a boundary", "server", h.ServerID, "to", h.PendingSize, "at", at)
	t.m.met.decisions.WithLabelValues(h.ServerID, dir, "executed").Inc()
	t.m.met.setPending(h.ServerID, "")
	h.TargetSize = h.PendingSize
	h.PendingSize, h.PendingReason, h.PendingSince = "", "", time.Time{}
	return true
}

// cancelScale drops a pending decision, saying why in the host's events.
func (t *tick) cancelScale(h *Host, why string) {
	dir := DirectionDown
	if sc := t.s.Scaling; sc != nil && Rung(sc.Ladder, h.PendingSize) > Rung(sc.Ladder, h.Size) {
		dir = DirectionUp
	}
	t.events = append(t.events, Event{ServerID: h.ServerID, At: t.now, From: h.State, To: h.State, Step: DetailScaleCancelled,
		Detail: "to " + h.PendingSize + " dropped: " + why, Size: h.Size})
	t.m.met.decisions.WithLabelValues(h.ServerID, dir, "cancelled").Inc()
	t.m.met.setPending(h.ServerID, "")
	t.m.log.Info("scaling decision dropped", "server", h.ServerID, "to", h.PendingSize, "why", why)
	h.PendingSize, h.PendingReason, h.PendingSince = "", "", time.Time{}
}

// sayScale is the warning players get when a decision is taken: when it happens and what it costs
// them. It holds no quote or semicolon (RCON's say).
func sayScale(dir string, players int) string {
	what := "a bigger machine"
	if dir == DirectionDown {
		what = "a smaller machine"
	}
	return fmt.Sprintf("Heads up: this server moves to %s (%d players) at the next map change. Everyone is disconnected for about 3 minutes, then rejoin.", what, players)
}

// say sends a chat line to the players, logging a failure; nothing waits on it.
func (t *tick) say(ctx context.Context, message string) {
	t.rt.mu.Lock()
	g := t.rt.game
	t.rt.mu.Unlock()
	if g == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, gameCallTimeout)
	defer cancel()
	if err := g.Say(ctx, message); err != nil {
		t.m.log.Warn("the players could not be warned", "server", t.s.ID, "error", err.Error())
	}
}

// since keeps when a condition started holding: now when it starts, zero when it stops.
func since(was time.Time, holds bool, now time.Time) time.Time {
	switch {
	case !holds:
		return time.Time{}
	case was.IsZero():
		return now
	}
	return was
}
