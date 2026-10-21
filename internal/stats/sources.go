package stats

import (
	"slices"

	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/store"
)

// Feed is what events of a source may count towards: ADR-0011's trust mapping by source. The
// mapping by trust (Official or Community) is the boards' own (Boards.Board).
type Feed string

// The feeds.
const (
	// FeedCombat is the combat boards and profiles: what a server or a publisher's API reported.
	FeedCombat Feed = "combat"
	// FeedParticipation is the participation boards and profiles (ADR-0014).
	FeedParticipation Feed = "participation"
	// FeedReview is nothing until a reviewer approves the event, and then profile badges only.
	FeedReview Feed = "review"
)

// Feeds is what events of a source may count towards. A source it doesn't know feeds nothing
// until reviewed.
func Feeds(source string) Feed {
	switch source {
	case store.SourceServerAdapter, store.SourcePlatformAPI:
		return FeedCombat
	case store.SourceFirstParty:
		return FeedParticipation
	default:
		return FeedReview
	}
}

// CombatBoards reports whether a game can have combat boards: one of its sources feeds them.
// A server-less game whose stats are only participation and claims has none (gravel#2).
func CombatBoards(g servers.Spec) bool {
	return slices.ContainsFunc(g.Sources, func(s string) bool { return Feeds(s) == FeedCombat })
}
