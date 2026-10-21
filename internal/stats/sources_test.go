package stats

import (
	"testing"

	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/store"
)

// ADR-0011's trust mapping by source: servers and publishers' APIs feed the combat boards,
// first-party apps the participation boards, and claims (or anything unknown) wait for review.
func TestFeeds(t *testing.T) {
	for source, want := range map[string]Feed{
		store.SourceServerAdapter: FeedCombat,
		store.SourcePlatformAPI:   FeedCombat,
		store.SourceFirstParty:    FeedParticipation,
		store.SourceSelfReport:    FeedReview,
		"rumour":                  FeedReview,
	} {
		if got := Feeds(source); got != want {
			t.Errorf("Feeds(%q) = %q, want %q", source, got, want)
		}
	}
}

// A shipped game has combat boards exactly when one of its sources feeds them (gravel#2).
func TestCombatBoards(t *testing.T) {
	specs := servers.Specs()
	for id, want := range map[string]bool{"wardogs": true, "cs2": true, "pubg": true, "seaofthieves": false, "starcitizen": false} {
		if got := CombatBoards(specs[id]); got != want {
			t.Errorf("CombatBoards(%s) = %v, want %v", id, got, want)
		}
	}
}
