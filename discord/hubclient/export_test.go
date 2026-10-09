package hubclient

import "time"

// SetClock replaces the clock; tests use it to reach the refresh margin.
func SetClock(c *Client, now func() time.Time) { c.now = now }
