package bot

import (
	"time"

	"github.com/disgoorg/disgo/gateway"
)

// SetGatewayState stands in for disgo's gateway so a test can drop and restore the session.
func SetGatewayState(rt *Runtime, f func() (gateway.Status, time.Duration)) { rt.gatewayState = f }

// MarkReady stands in for Start's command registration, which a gateway test can't run offline.
func MarkReady(rt *Runtime) { rt.ready.Store(true) }
