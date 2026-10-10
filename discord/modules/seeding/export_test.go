package seeding

import "time"

// SetClock replaces the module's clock, for tests that walk through a week.
func (m *Module) SetClock(now func() time.Time) { m.now = now }
