// Package deploy embeds the stock production units from quadlet/ so the gravel CLI can install
// them on a hub host it makes (`gravel hub create --profile stock`, ADR-0020). The files stay the
// source of truth for a manual install too (deploy/README.md); this package only carries them.
package deploy

import "embed"

// Quadlet holds the stock hub's units: the hub, Postgres with its volume, the network, and the
// optional Cloudflare Tunnel. The backup and observability sets are not included.
//
//go:embed quadlet/gravel-hub.container quadlet/gravel-postgres.container quadlet/gravel-postgres.volume quadlet/gravel.network quadlet/gravel-cloudflared.container
var Quadlet embed.FS
