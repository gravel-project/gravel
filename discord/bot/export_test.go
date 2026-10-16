package bot

import (
	"context"
	"time"

	"github.com/disgoorg/disgo/gateway"
)

// SetGatewayState stands in for disgo's gateway so a test can drop and restore the session.
func SetGatewayState(rt *Runtime, f func() (gateway.Status, time.Duration)) { rt.gatewayState = f }

// MarkReady stands in for Start's command registration, which a gateway test can't run offline.
func MarkReady(rt *Runtime) { rt.ready.Store(true) }

// SyncAvatar runs the avatar job once, as Run would after Start.
func SyncAvatar(rt *Runtime, ctx context.Context) error { return rt.syncAvatar(ctx) }

// JobNames are the runtime's jobs, the modules' and its own.
func JobNames(rt *Runtime) []string {
	names := make([]string, 0, len(rt.jobs))
	for _, j := range rt.jobs {
		names = append(names, j.name)
	}
	return names
}
