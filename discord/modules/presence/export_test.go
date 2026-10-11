package presence

import "time"

// SetNow fixes the module's clock.
func SetNow(m *Module, now func() time.Time) { m.now = now }
