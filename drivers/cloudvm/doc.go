// Package cloudvm is the cloud-VM driver (ADR-0003): a server on a single-tenant VM the hub
// creates, sizes, parks and destroys through cloud.Provider. The game runs on the VM in its
// container under the runtime user's systemd, installed by the cloud-init document vmdoc renders;
// its secrets reach the VM over SSH from the hub, never in user-data; the hub controls it over the
// game's own channels (RCON, and the log the game pushes to the hub's ingest route).
//
// A Manager owns every cloud-VM server's host: a state machine per host, run by the hub's job
// runner, recorded in Postgres so a restart resumes where it stopped, and idempotent by the tags
// and names everything it makes carries. The per-server Driver the monitor builds is thin: it
// reads the Manager. What a game needs on its VM (image, ports, settings) is the Game spec's
// cloudvm section (Profile).
package cloudvm

// Name is the driver's name in a servers manifest.
const Name = "cloudvm"
