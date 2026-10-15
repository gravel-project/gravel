// Package hubvm makes the hub's own host (ADR-0020): the cloud-init document a hub VM boots with,
// and the create, status, destroy and allow-ssh steps the gravel CLI runs through a
// cloud.Provider. The hub is never a ManagedServer: nothing here runs inside the hub, and nothing
// in the hub calls it.
//
// The document has two profiles. "host" is the machine and nothing more: an admin user for the
// OS, a rootless runtime user whose home is the data volume, podman, and automatic security
// updates; an operator's own configuration management installs the hub on top (Hidden Token
// Gaming's Ansible does). "stock" is the host plus gravel's own units from deploy/quadlet, with
// the database secrets generated on the host at first boot, so one command gives a running hub.
//
// # For a control plane
//
// The package is public so that a program provisioning hubs for others imports it rather than
// parsing the CLI's text (gravel#211). Open a cloud.Provider (cloud/digitalocean.New), fill a
// Spec and call Create; Status, AllowSSH and Destroy find the hub again by its name. Every step
// is idempotent and keeps no state outside the cloud: the resources are found by the tags
// BaseTag and Tag(name) and the names VolumeName(name) and FirewallName(name), so a run that
// stopped halfway is finished by running it again.
//
// What a caller can rely on, under docs/releasing.md "Versions": the exported names and their
// meaning, the Kind and Action values, and the tags and names above. A Step's Detail is for a
// person and may change in any release. Waiting for cloud-init and pushing secrets over SSH are
// the caller's (the CLI's are in cmd/gravel); the owner claim is ADR-0002's.
package hubvm
