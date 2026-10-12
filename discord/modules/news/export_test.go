package news

import "time"

// SetClock replaces the module's clock.
func (m *Module) SetClock(now func() time.Time) { m.now = now }
