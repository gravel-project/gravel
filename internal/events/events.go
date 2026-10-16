// Package events is the registry of the hub's event kinds (ADR-0028): every kind the events log
// holds, with its fixed severity and what it means. The store writes events in each source's own
// transaction and takes their severity from here; notifiers, the dashboard and the CLI filter by
// kind and severity.
package events

import (
	"fmt"
	"slices"
	"strings"
)

// Severity is how much an event asks of a person, lowest first.
type Severity string

// The severities, lowest first.
const (
	Info     Severity = "info"     // it happened; nobody needs to act
	Notice   Severity = "notice"   // a person did something worth seeing
	Warning  Severity = "warning"  // something may need a person soon
	Critical Severity = "critical" // something needs a person now
)

// Severities are every severity, lowest first.
var Severities = []Severity{Info, Notice, Warning, Critical}

// Rank orders severities; an unknown one ranks below Info.
func Rank(s Severity) int { return slices.Index(Severities, s) }

// AtLeast are the severities at or above min, for a filter; all of them when min is "".
func AtLeast(min Severity) []Severity {
	r := Rank(min)
	if r < 0 {
		return slices.Clone(Severities)
	}
	return slices.Clone(Severities[r:])
}

// Kind is one kind of event.
type Kind struct {
	Name     string
	Severity Severity
	About    string
}

var kinds = []Kind{
	// Identity (logins stay out: they are every visit).
	{"identity.registered", Info, "a new member registered with a provider or a username"},
	{"identity.linked", Info, "a member linked another account"},
	{"identity.unlinked", Notice, "a member unlinked an account"},
	{"identity.proven", Info, "a member proved an account again in game"},
	{"identity.password_set", Notice, "a local account's first password was set from its link"},
	{"identity.password_reset", Notice, "a local account's password was reset from its link"},
	{"identity.password_reset_issued", Notice, "staff or the host issued a password link"},
	{"identity.password_reset_mailed", Notice, "a member asked for a password link and the hub mailed it to their confirmed recovery address"},
	{"identity.email_verified", Info, "a member confirmed a local account's recovery address with the link mailed to it"},
	// The hub itself: who owns it and how it is set up.
	{"ownership.claim_token_minted", Notice, "the host minted an owner-claim token on its command line (gravel-hub claim-token)"},
	{"ownership.claimed", Notice, "a member claimed the hub with the one-time token and became its owner"},
	{"ownership.nominated", Notice, "the owner nominated a member to take the hub over"},
	{"ownership.transfer_cancelled", Notice, "the owner cancelled a nomination to take the hub over"},
	{"ownership.declined", Notice, "a member declined to take the hub over"},
	{"ownership.transferred", Warning, "a member accepted the hub and is its owner; the old owner is an admin"},
	{"settings.updated", Notice, "the Organization settings were replaced"},
	// Apps (ADR-0008, ADR-0015): registered, revoked, and the access members give them.
	{"app.created", Notice, "an app was registered"},
	{"app.revoked", Notice, "an app was revoked: its tokens and members' grants are gone"},
	{"app.granted", Info, "a member let an app act for them, or widened what it may do"},
	{"app.grant_revoked", Info, "a member took back an app's access"},
	// The operator's admin actions through the API or the host's command line (#195).
	{"servers.applied", Notice, "the servers manifest was applied and changed something"},
	{"stats.erased", Notice, "a player's identity was erased from the stats"},
	{"sessions.revoked", Notice, "staff logged a member out everywhere"},
	// Roles.
	{"role.granted", Notice, "a member was granted a role, everywhere or for a game"},
	{"role.revoked", Notice, "a member lost a role"},
	// Moderation and other actions through the hub, with their outcome (the audit log).
	{"moderation.kick", Notice, "a player was kicked"},
	{"moderation.ban", Notice, "a player was banned"},
	{"moderation.unban", Notice, "a player was unbanned"},
	{"moderation.message", Info, "a player was sent a message"},
	{"moderation.broadcast", Info, "a server was sent a broadcast"},
	{"moderation.move_player", Info, "a player was moved to a team"},
	{"action.apply_config", Notice, "a server's configuration was applied"},
	{"action.host_park", Notice, "someone asked a cloud-VM server to park"},
	{"action.host_unpark", Notice, "someone asked a cloud-VM server to start"},
	{"action.host_resize", Notice, "someone resized a cloud-VM server's machine"},
	{"action.host_unpin", Info, "someone let the scaling policy size a server again"},
	{"action.host_snapshot", Notice, "someone snapshotted a parked server's volume"},
	{"action.host_expect", Info, "someone said how many players to expect"},
	{"action.host_destroy", Warning, "someone destroyed a parked server's remains"},
	{"action.host_reboot", Notice, "someone rebooted a cloud-VM server's machine"},
	{"action.lease_request", Info, "an app or a member asked for a lease"},
	{"action.lease_release", Info, "an app or a member gave a lease back"},
	// A server's health, from the monitor's polls (gravel#190): on a transition only.
	{"server.unreachable", Warning, "a server stopped answering its polls"},
	{"server.recovered", Info, "a server is answering again"},
	{"server.credential_refused", Warning, "a server refuses the hub's credential"},
	{"server.credential_missing", Warning, "a server's credential file is missing or empty"},
	// A cloud-VM server's host (ADR-0003): its transitions and steps.
	{"host.absent", Info, "the host is gone"},
	{"host.creating", Info, "the host is being made"},
	{"host.booting", Info, "the host is booting"},
	{"host.starting", Info, "the game is starting on the host"},
	{"host.running", Info, "the server is up"},
	{"host.parking", Info, "the host is parking"},
	{"host.parked", Info, "the host is parked"},
	{"host.resizing", Info, "the host is being resized"},
	{"host.resized", Notice, "the host was resized and is running again"},
	{"host.rebooting", Notice, "the host is being rebooted: its instance stopped and started again"},
	{"host.destroying", Notice, "the host is being destroyed"},
	{"host.failing", Warning, "a step on the host keeps failing; the hub retries"},
	{"host.recovered", Info, "the host's failing step went through"},
	{"host.expect", Info, "the host was told how many players to expect"},
	{"host.snapshot", Notice, "a volume snapshot was taken"},
	{"host.scale", Info, "the scaling policy moved the host to another size"},
	{"host.scale_cancelled", Info, "a planned resize was called off"},
	{"host.env_drift", Notice, "the host's game files had drifted and were put back"},
	{"host.step", Info, "another step on the host"},
	// Leases (ADR-0023).
	{"lease.starting", Info, "a lease was taken; its server is getting ready"},
	{"lease.ready", Info, "a lease's server is ready to join"},
	{"lease.ending", Info, "a lease's match ended; the server is given back shortly"},
	{"lease.released", Info, "a lease ended and its server was given back"},
	{"lease.failed", Warning, "a lease's server did not get ready"},
	// Match results (ADR-0024).
	{"match.ended", Info, "a series ended on a server"},
	// Backups (ADR-0006).
	{"backup.failed", Critical, "the last base backup failed"},
	{"backup.stale", Warning, "no base backup has succeeded for over 26 hours"},
	{"backup.recovered", Info, "a base backup succeeded again"},
}

// Kinds is every kind, in the registry's order.
func Kinds() []Kind { return slices.Clone(kinds) }

// Lookup returns a kind.
func Lookup(name string) (Kind, bool) {
	i := slices.IndexFunc(kinds, func(k Kind) bool { return k.Name == name })
	if i < 0 {
		return Kind{}, false
	}
	return kinds[i], true
}

// SeverityOf is a kind's severity, or an error for a kind the registry doesn't have: a writer
// that invents one fails its own transaction and its test.
func SeverityOf(name string) (Severity, error) {
	k, ok := Lookup(name)
	if !ok {
		return "", fmt.Errorf("events: no such kind %q", name)
	}
	return k.Severity, nil
}

// Group is a kind's family, the part before the dot ("host" of "host.running"): a filter may name
// one.
func Group(name string) string {
	g, _, _ := strings.Cut(name, ".")
	return g
}
