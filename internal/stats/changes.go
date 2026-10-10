package stats

import "sync/atomic"

// Changes counts writes to the stats store in this hub, so the boards' cache can keep an answer
// for as long as nothing it counts has changed. Stats change only while someone plays: an idle
// hub serves every board from memory, and a live match refreshes a board at most every
// BoardLiveTTL however many people watch it.
type Changes struct{ n atomic.Uint64 }

// Bump records a write.
func (c *Changes) Bump() {
	if c != nil {
		c.n.Add(1)
	}
}

// Seen is the number of writes so far.
func (c *Changes) Seen() uint64 {
	if c == nil {
		return 0
	}
	return c.n.Load()
}
