// Package digitalocean is cloud.Provider over DigitalOcean's API through godo, the cloud's own Go
// SDK (ADR-0003). The DigitalOcean facts that shaped it, verified against the live API on
// 2026-10-11 and the DigitalOcean documentation on 2026-10-08:
//
//   - A resize through the API powers the Droplet off itself. A CPU-and-memory resize
//     (resize_disk false) is reversible; a disk resize is permanent, so this package never asks
//     for one. The target size's disk must be at least the Droplet's current disk, and a Droplet
//     keeps its original disk after a CPU-and-memory resize, so ResizeInstance checks against the
//     Droplet's own disk, not its size's. Resizes stay within a family of compatible plans;
//     CanResize encodes both rules as data, and is conservative about families (see
//     sizeFamily), because a refused resize at a map change costs a cooldown while a wrong
//     "yes" costs a failed resize with players waiting.
//   - A powered-off Droplet bills in full, so every size says BillsWhenStopped. Parking a
//     server on DigitalOcean means destroying the Droplet and keeping its volume and reserved
//     IP (ADR-0003 §5); destroying a Droplet detaches its volumes, it never deletes them.
//   - Volumes are created unformatted: the instance's cloud-init formats one once, on first
//     boot. The instance sees a volume at /dev/disk/by-id/scsi-0DO_Volume_<name>. Volume names
//     are lowercase letters, digits and hyphens, at most 64 characters, unique per region.
//   - A volume snapshot is made through its volume and answered at once (201, no action), and
//     a volume can be made from it straight away. It is kept in the volume's region, outlives
//     the volume, and makes volumes only in that region and at least as big as the volume it
//     was taken of (its min_disk_size; size_gigabytes is the data it holds, fractional). Names
//     need not be unique. Volume snapshots sit under the token's custom scope
//     block_storage_snapshot (create, read, delete), not snapshot, which is the Droplets' (the
//     live run of 2026-10-12, gravel#166), so they are read and deleted under
//     /v2/volumes/snapshots/, which that scope grants. The rest is from the API reference and
//     the product documentation on 2026-10-12; the live conformance run checks it.
//   - A reserved IP carries inbound traffic only: a Droplet's outbound traffic leaves from its
//     own address, which is the one to pin in another firewall. Reserved IPs take no tags, so
//     Addresses filtered by tag finds an address through the Droplet it is assigned to.
//   - A cloud firewall with no outbound rules blocks all outbound traffic; callers pass
//     cloud.AllowAllOutbound(). Firewalls apply by tag, and the tags are created first.
//   - Every Droplet, volume and reserved IP the provider creates goes into the project named in
//     Config.Project (an id or a name, resolved once), so the account's own scaffolding
//     (HTG's OpenTofu) can keep gravel's machines apart from everything else. An empty project is
//     the team's default project, and nothing is assigned.
//   - The API allows 5,000 requests an hour per token plus a burst limit. A 429 is retried a
//     few times with backoff, then surfaces as cloud.ErrRateLimited; Wait's polling backs off to
//     stay inside the budget.
//
// The token is a secret: it is sent only in the Authorization header and never appears in an
// error, a log line or Config's String.
//
// The dotest subpackage is a stateful emulator of the part of the API this package uses; the
// conformance suite (cloud/cloudtest) runs against it in CI and against the real API with
// `-tags live`.
package digitalocean
