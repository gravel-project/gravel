package fake

import (
	"sync"
	"time"
)

// Clock is the time the fake's operations complete on. The real clock is the default; a test
// that wants to see an instance pending, or a Wait that has not returned yet, passes a
// ManualClock and moves it with Advance.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// realClock is the wall clock.
type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// ManualClock is a Clock that only moves when the test says so, so an operation's completion is
// a step the test takes rather than a race it waits out. It is safe for concurrent use.
type ManualClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []manualTimer
}

type manualTimer struct {
	at time.Time
	ch chan time.Time
}

// NewManualClock is a clock standing at start.
func NewManualClock(start time.Time) *ManualClock {
	return &ManualClock{now: start}
}

// Now is the clock's current time.
func (c *ManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// After fires once the clock has been advanced by d; a d of zero or less fires at once.
func (c *ManualClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1) // buffered: Advance never blocks on a reader that went away
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.timers = append(c.timers, manualTimer{at: c.now.Add(d), ch: ch})
	return ch
}

// Advance moves the clock forward by d and fires every timer that is now due.
func (c *ManualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.timers[:0]
	for _, t := range c.timers {
		if t.at.After(c.now) {
			kept = append(kept, t)
			continue
		}
		t.ch <- c.now
	}
	clear(c.timers[len(kept):]) // drop the fired channels' references
	c.timers = kept
}

// Timers is how many timers are waiting for the clock to reach them: a test that started a Wait
// in a goroutine can poll it to know the Wait is blocked before it advances the clock.
func (c *ManualClock) Timers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}
