package stats

import (
	"context"
	"testing"
	"time"

	"github.com/gravel-project/gravel/drivers"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/store"
)

func TestRecorderKeepsOccupancyAndScores(t *testing.T) {
	st := newMemStore()
	r := newRecorder(st)
	ctx := context.Background()
	at := func(d time.Duration, count int, teams []drivers.TeamScore, players ...drivers.Player) servers.Observation {
		o := obs(d, 0, players...)
		o.Status.Players, o.Status.Teams = count, teams
		return o
	}
	scores := []drivers.TeamScore{{Name: "Alpha", Color: "#4CB1EF", Score: 10}, {Name: "Bravo", Score: 5}}

	r.Observe(ctx, srv, at(0, 0, nil))                                    // empty: no minute written
	r.Observe(ctx, srv, at(15*time.Second, 3, scores, player("1", 0, 0))) // the server counts 3, one has an identity
	r.Observe(ctx, srv, at(30*time.Second, 0, scores, player("1", 1, 0), player("2", 0, 0), player("3", 0, 0), player("4", 0, 0)))
	r.Observe(ctx, srv, at(45*time.Second, 2, scores, player("1", 1, 0), player("2", 0, 0)))
	m0 := t0
	if got := st.occupancy[m0]; got != [2]int{0, 4} {
		t.Errorf("minute 0, with an empty poll, 3, 4 (identities beat the server's 0) and 2: %v", got)
	}

	// The next minute starts its own range; the server empties at its end.
	r.Observe(ctx, srv, at(60*time.Second, 2, scores, player("1", 1, 0), player("2", 0, 0)))
	r.Observe(ctx, srv, at(75*time.Second, 5, scores, player("1", 1, 0), player("2", 0, 0)))
	r.Observe(ctx, srv, at(90*time.Second, 0, scores))
	if got := st.occupancy[m0.Add(time.Minute)]; got != [2]int{0, 5} {
		t.Errorf("minute 1 dips to 0 when the server empties: %v", got)
	}
	if _, ok := st.occupancy[m0.Add(2*time.Minute)]; ok {
		t.Error("a minute nobody was on is not written")
	}
	r.Observe(ctx, srv, at(125*time.Second, 0, nil))
	if _, ok := st.occupancy[m0.Add(2*time.Minute)]; ok {
		t.Error("an empty minute stays unwritten")
	}

	// Scores: written once per change, on the match they belong to.
	if got := st.teams[1]; len(got) != 2 || got[0] != (store.TeamScore{Name: "Alpha", Color: "#4CB1EF", Score: 10}) {
		t.Errorf("match 1's scores: %+v", got)
	}
	st.teams = nil
	r.Observe(ctx, srv, at(140*time.Second, 1, scores, player("5", 0, 0))) // a new match after the empty server
	r.Observe(ctx, srv, at(155*time.Second, 1, scores, player("5", 0, 0)))
	if _, ok := st.teams[1]; ok || len(st.teams[2]) != 2 {
		t.Errorf("a new match writes its own scores once: %+v", st.teams)
	}
	st.teams = nil
	r.Observe(ctx, srv, at(170*time.Second, 1, scores, player("5", 0, 0)))
	if st.teams != nil {
		t.Errorf("unchanged scores are not written again: %+v", st.teams)
	}
	r.Observe(ctx, srv, at(185*time.Second, 1, []drivers.TeamScore{{Name: "Alpha", Score: 11}}, player("5", 0, 0)))
	if got := st.teams[2]; len(got) != 1 || got[0].Score != 11 {
		t.Errorf("a change is written: %+v", got)
	}
	r.Observe(ctx, srv, at(200*time.Second, 1, nil, player("5", 0, 0)))
	if got, ok := st.teams[2]; !ok || len(got) != 0 {
		t.Errorf("a game without teams writes an empty list, not a stale one: %+v %v", got, ok)
	}
}
