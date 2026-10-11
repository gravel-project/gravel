package cloudvm

// What the hub makes for a server carries the server's tags and a name derived from its id, so a
// restart or a retry after a crash finds what exists by asking the cloud, and makes only what is
// missing (ADR-0003; the hub's own host does the same, ADR-0020).
const BaseTag = "gravel-server"

// Tag is the tag on everything made for one server.
func Tag(serverID string) string { return BaseTag + ":" + serverID }

// InstanceName is the server's instance (and its hostname).
func InstanceName(serverID string) string { return serverID }

// VolumeName is the server's data volume, where the game's install lives.
func VolumeName(serverID string) string { return serverID + "-data" }

// FirewallName is the server's firewall.
func FirewallName(serverID string) string { return BaseTag + "-" + serverID }

// SSHKeyName is the name the hub's SSH key is registered under for the server's location.
func SSHKeyName(locationID string) string { return "gravel-hub-" + locationID }
