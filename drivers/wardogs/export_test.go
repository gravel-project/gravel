package wardogs

import (
	"time"

	"github.com/gravel-project/gravel/drivers"
)

// SetNow replaces the driver's clock, for the rotation's read interval.
func SetNow(d drivers.ExternalReachable, now func() time.Time) { d.(*Driver).now = now }
