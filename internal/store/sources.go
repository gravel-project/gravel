package store

// The provenance of a stats event (ADR-0011): where it came from, which decides what it may feed.
// The CHECKs on player_stats.source and stats_events.source list the same four.
const (
	// SourceServerAdapter is what a game server reported: its poll or its feed.
	SourceServerAdapter = "server_adapter"
	// SourcePlatformAPI is what a publisher's API reported (PUBG's).
	SourcePlatformAPI = "platform_api"
	// SourceFirstParty is what a first-party app recorded: participation (ADR-0014).
	SourceFirstParty = "first_party"
	// SourceSelfReport is what a player claimed, for a reviewer to approve.
	SourceSelfReport = "self_report"
)

// Sources are the four sources, in that order: what a Game spec's sources may name.
var Sources = []string{SourceServerAdapter, SourcePlatformAPI, SourceFirstParty, SourceSelfReport}
