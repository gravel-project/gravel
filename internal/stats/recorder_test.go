package stats

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/gravel-project/gravel/drivers"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/store"
)

// memStore is the stats store in memory, for the recorder's and the namer's logic; the SQL is
// tested against Postgres in internal/store.
type memStore struct {
	Store      // the methods a test does not use panic
	mu         sync.Mutex
	matches    []store.Match
	players    map[int64]map[store.PlayerKey]store.MatchPlayer
	pseudonyms map[store.PlayerKey]string
	fail       bool
}

func newMemStore() *memStore {
	return &memStore{players: map[int64]map[store.PlayerKey]store.MatchPlayer{}, pseudonyms: map[store.PlayerKey]string{}}
}

func (m *memStore) OpenMatch(_ context.Context, _ uuid.UUID, serverID string) (store.Match, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.matches {
		if x.ServerID == serverID && x.EndedAt == nil {
			return x, nil
		}
	}
	return store.Match{}, store.ErrNotFound
}

func (m *memStore) StartMatch(_ context.Context, x store.Match) (store.Match, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return store.Match{}, errors.New("database away")
	}
	for i := range m.matches {
		if m.matches[i].ServerID == x.ServerID && m.matches[i].EndedAt == nil {
			at := x.StartedAt
			m.matches[i].EndedAt = &at
		}
	}
	x.ID = int64(len(m.matches) + 1)
	m.matches = append(m.matches, x)
	m.players[x.ID] = map[store.PlayerKey]store.MatchPlayer{}
	return x, nil
}

func (m *memStore) EndMatch(_ context.Context, id int64, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.matches[id-1].EndedAt == nil {
		m.matches[id-1].EndedAt = &at
	}
	return nil
}

func (m *memStore) MatchPlayers(_ context.Context, id int64) ([]store.MatchPlayer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.MatchPlayer
	for _, p := range m.players[id] {
		out = append(out, p)
	}
	return out, nil
}

func (m *memStore) SaveMatchPlayers(_ context.Context, ps []store.MatchPlayer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return errors.New("database away")
	}
	for _, p := range ps {
		k := store.PlayerKey{Provider: p.Provider, Subject: p.Subject}
		if old, ok := m.players[p.MatchID][k]; ok {
			p.FirstSeen, p.Source, p.Trust = old.FirstSeen, old.Source, old.Trust
		}
		m.players[p.MatchID][k] = p
	}
	return nil
}

func (m *memStore) row(matchID int64, subject string) store.MatchPlayer {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.players[matchID][store.PlayerKey{Provider: "steam", Subject: subject}]
}

var (
	t0  = time.Date(2026, 10, 10, 20, 0, 0, 0, time.UTC)
	srv = servers.Server{ID: "wd-1", Game: "wardogs", Trust: servers.TrustOfficial}
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func player(subject string, kills, deaths int) drivers.Player {
	return drivers.Player{Name: "p" + subject, Identity: drivers.Identity{Provider: "steam", Subject: subject}, Team: "Lonestar", Kills: kills, Deaths: deaths}
}

func obs(at time.Duration, rotation int, players ...drivers.Player) servers.Observation {
	return servers.Observation{ServerID: "wd-1", ObservedAt: t0.Add(at), Status: drivers.Status{Map: "Ozeti", RotationIndex: rotation}, Players: players}
}

func newRecorder(st *memStore) *Recorder {
	return NewRecorder(st, uuid.New(), nil, prometheus.NewRegistry(), quiet())
}

func TestRecorderFollowsAMatch(t *testing.T) {
	st := newMemStore()
	r := newRecorder(st)
	ctx := context.Background()

	r.Observe(ctx, srv, obs(0, 0))                                 // empty: no match
	r.Observe(ctx, srv, obs(15*time.Second, 0, player("1", 0, 0))) // the first player
	r.Observe(ctx, srv, obs(30*time.Second, 0, player("1", 2, 1), player("2", 0, 0)))
	r.Observe(ctx, srv, obs(45*time.Second, 0, player("1", 3, 1), player("2", 1, 2)))
	if len(st.matches) != 1 || st.matches[0].Map != "Ozeti" || st.matches[0].GameID != "wardogs" || !st.matches[0].StartedAt.Equal(t0.Add(15*time.Second)) {
		t.Fatalf("matches = %+v", st.matches)
	}
	p1 := st.row(1, "1")
	if p1.Kills != 3 || p1.Deaths != 1 || p1.SecondsOn != 30 || p1.Trust != "official" || p1.Source != SourceServerAdapter || p1.Team != "Lonestar" {
		t.Errorf("player 1 = %+v", p1)
	}
	if p2 := st.row(1, "2"); p2.Kills != 1 || p2.Deaths != 2 || p2.SecondsOn != 15 {
		t.Errorf("player 2 = %+v", p2)
	}

	// Player 1 leaves and comes back: the game counts from the rejoin, the row keeps what it had.
	r.Observe(ctx, srv, obs(60*time.Second, 0, player("2", 2, 2)))
	r.Observe(ctx, srv, obs(75*time.Second, 0, player("1", 1, 0), player("2", 2, 3)))
	if p1 := st.row(1, "1"); p1.Kills != 4 || p1.Deaths != 1 || p1.SecondsOn != 30 {
		t.Errorf("a rejoin keeps the earlier segment, time away is not time on: %+v", p1)
	}
	if len(st.matches) != 1 {
		t.Errorf("one player's counters going down is a rejoin, not a new match: %d matches", len(st.matches))
	}

	// The rotation moves: a new match, the old one ended.
	r.Observe(ctx, srv, obs(90*time.Second, 1, player("1", 0, 0), player("2", 0, 0)))
	if len(st.matches) != 2 || st.matches[0].EndedAt == nil || st.matches[1].RotationIndex != 1 {
		t.Fatalf("rotation: %+v", st.matches)
	}
	if p := st.row(2, "1"); p.Kills != 0 || p.SecondsOn != 0 {
		t.Errorf("the new match starts from zero: %+v", p)
	}

	// Everyone's counters drop together without a rotation (a restart, a reset): a new match.
	r.Observe(ctx, srv, obs(105*time.Second, 1, player("1", 5, 2), player("2", 4, 4)))
	r.Observe(ctx, srv, obs(120*time.Second, 1, player("1", 1, 0), player("2", 0, 1)))
	if len(st.matches) != 3 {
		t.Errorf("all counters dropping is a new match: %d", len(st.matches))
	}

	// The server empties: the match ends; the next player starts another.
	r.Observe(ctx, srv, obs(135*time.Second, 1))
	if st.matches[2].EndedAt == nil {
		t.Error("an empty server ends its match")
	}
	r.Observe(ctx, srv, obs(150*time.Second, 1, player("3", 0, 0)))
	if len(st.matches) != 4 {
		t.Errorf("a player after an empty server starts a match: %d", len(st.matches))
	}
	if n := testutil.ToFloat64(r.matches.WithLabelValues("wd-1", "wardogs")); n != 4 {
		t.Errorf("matches metric = %v", n)
	}
}

// After a hub restart the open match is read back: a player whose counters carried on is not
// counted twice; one who rejoined while the hub was down keeps the stored totals.
func TestRecorderResumesAfterARestart(t *testing.T) {
	st := newMemStore()
	ctx := context.Background()
	r := newRecorder(st)
	r.Observe(ctx, srv, obs(0, 0, player("1", 4, 2), player("2", 3, 3)))

	r = newRecorder(st) // the hub restarted
	r.Observe(ctx, srv, obs(5*time.Minute, 0, player("1", 6, 2), player("2", 1, 0)))
	if len(st.matches) != 1 {
		t.Fatalf("the open match carries on: %d", len(st.matches))
	}
	if p := st.row(1, "1"); p.Kills != 6 || p.Deaths != 2 {
		t.Errorf("counters that carried on: %+v", p)
	}
	if p := st.row(1, "2"); p.Kills != 4 || p.Deaths != 3 {
		t.Errorf("a rejoin during the restart keeps the stored totals: %+v", p)
	}
	if p := st.row(1, "1"); p.SecondsOn != 0 {
		t.Errorf("five minutes without a poll are not time on: %+v", p)
	}
}

func TestRecorderCommunityTrustAndFailures(t *testing.T) {
	st := newMemStore()
	ctx := context.Background()
	r := newRecorder(st)
	community := srv
	community.Trust = servers.TrustCommunity
	r.Observe(ctx, community, obs(0, 0, player("1", 1, 0)))
	if p := st.row(1, "1"); p.Trust != "community" {
		t.Errorf("the server's trust is copied in: %+v", p)
	}

	st.fail = true
	r.Observe(ctx, community, obs(15*time.Second, 0, player("1", 2, 0)))
	if n := testutil.ToFloat64(r.errs.WithLabelValues("wd-1")); n != 1 {
		t.Errorf("a failed record is counted: %v", n)
	}
	st.fail = false
	r.Observe(ctx, community, obs(30*time.Second, 0, player("1", 3, 0)))
	if p := st.row(1, "1"); p.Kills != 3 || len(st.matches) != 1 {
		t.Errorf("after a failure the next poll records from the store: %+v, %d matches", p, len(st.matches))
	}
	// A player the game reports without an identity is not recorded.
	r.Observe(ctx, community, obs(45*time.Second, 0, player("1", 3, 0), drivers.Player{Name: "bot", Kills: 9}))
	st.mu.Lock()
	n := len(st.players[1])
	st.mu.Unlock()
	if n != 1 {
		t.Errorf("rows = %d", n)
	}
}

// The recorder counts its writes for the boards' cache: an empty server writes nothing, a poll
// with players and a match's end do.
func TestRecorderCountsItsWrites(t *testing.T) {
	st := newMemStore()
	var c Changes
	r := NewRecorder(st, uuid.New(), &c, prometheus.NewRegistry(), quiet())
	ctx := context.Background()
	r.Observe(ctx, srv, obs(0, 0))
	if c.Seen() != 0 {
		t.Errorf("an empty server counted %d writes", c.Seen())
	}
	r.Observe(ctx, srv, obs(15*time.Second, 0, player("1", 0, 0)))
	r.Observe(ctx, srv, obs(30*time.Second, 0, player("1", 1, 0)))
	if c.Seen() != 2 {
		t.Errorf("two polls with a player counted %d writes", c.Seen())
	}
	r.Observe(ctx, srv, obs(45*time.Second, 0)) // empties: the match ends
	if c.Seen() != 3 {
		t.Errorf("a match's end was not counted: %d", c.Seen())
	}
}
