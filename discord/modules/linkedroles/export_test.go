package linkedroles

import "time"

// SetBackoff shortens the retry wait for tests.
func (m *Module) SetBackoff(d time.Duration) { m.backoff = d }
