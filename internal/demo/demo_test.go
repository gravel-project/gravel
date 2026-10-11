package demo

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/games/wardogs/wardogsim"
	"github.com/gravel-project/gravel/internal/store"
)

var (
	now     = time.Date(2026, 10, 11, 18, 30, 0, 0, time.UTC)
	servers = []store.ManagedServer{
		{ID: "wd-2", GameID: "wardogs", Trust: "community"},
		{ID: "wd-1", GameID: "wardogs", Trust: "official"},
		{ID: "cs-1", GameID: "cs2", Trust: "official"},
	}
)

func generate(t *testing.T, opt Options) (store.DemoHistory, Summary, []store.PlayerKey) {
	t.Helper()
	if opt.Now.IsZero() {
		opt.Now = now
	}
	h, sum, keys, err := Generate(servers, opt)
	if err != nil {
		t.Fatal(err)
	}
	return h, sum, keys
}

func TestGenerateIsRepeatable(t *testing.T) {
	a, _, _ := generate(t, Options{Days: 14, Members: 20, Seed: 7})
	b, _, _ := generate(t, Options{Days: 14, Members: 20, Seed: 7})
	if !reflect.DeepEqual(a, b) {
		t.Error("the same options gave two histories")
	}
	c, _, _ := generate(t, Options{Days: 14, Members: 20, Seed: 8})
	if reflect.DeepEqual(a.Matches, c.Matches) {
		t.Error("another seed gave the same matches")
	}
}

func TestGenerate(t *testing.T) {
	h, sum, keys := generate(t, Options{Days: 30, Members: DefaultMembers, Seed: 1})
	if sum.Members != DefaultMembers || sum.Moderators != 3 || sum.ShowingNames == 0 || sum.ShowingNames == DefaultMembers {
		t.Errorf("members: %+v", sum)
	}
	if len(sum.Skipped) != 1 || sum.Skipped[0] != "cs-1" || sum.Matches["wd-1"] < 300 || sum.Matches["wd-2"] == 0 || sum.Matches["wd-2"] >= sum.Matches["wd-1"] {
		t.Errorf("matches: %+v", sum)
	}

	steamOf := map[string]bool{}
	for i, m := range h.Members {
		if len(m.Identities) != 2 || m.Identities[0].Provider != "discord" || m.Identities[1].Provider != "steam" || m.User.DisplayName == "" {
			t.Fatalf("member %d: %+v", i, m)
		}
		if d := m.Identities[0].Subject; d != DiscordSubject(i+1) || len(d) > 7 {
			t.Errorf("member %d's Discord id %s could be a real account", i, d)
		}
		if s := m.Identities[1].Subject; s != wardogsim.SteamID(i+1) || s >= "76561197960265728" {
			t.Errorf("member %d's SteamID %s could be a real account", i, s)
		}
		if m.User.CreatedAt.Before(now.Add(-30*24*time.Hour)) || m.User.CreatedAt.After(now) || m.Identities[1].LinkedAt.Before(m.User.CreatedAt) {
			t.Errorf("member %d joined %v, linked Steam %v", i, m.User.CreatedAt, m.Identities[1].LinkedAt)
		}
		steamOf[m.Identities[1].Subject] = true
	}

	type minute struct {
		server string
		at     time.Time
	}
	minutes := map[minute]bool{}
	for _, o := range h.Occupancy {
		k := minute{o.ServerID, o.Minute}
		if minutes[k] || o.Fewest > o.Most || o.Most < 1 || o.Minute.Truncate(time.Minute) != o.Minute {
			t.Fatalf("occupancy %+v (twice: %v)", o, minutes[k])
		}
		minutes[k] = true
	}

	rows, memberRows := 0, 0
	var last time.Time
	for _, m := range h.Matches {
		n := len(m.Players)
		if n < MinMatchPlayers-3 || n > MaxMatchPlayers || m.Match.EndedAt == nil || !m.Match.EndedAt.After(m.Match.StartedAt) || !m.Match.EndedAt.Before(now) || m.Match.Map == "" {
			t.Fatalf("match %+v with %d players", m.Match, n)
		}
		if m.Match.ServerID == "wd-1" {
			if !m.Match.StartedAt.After(last) {
				t.Fatalf("wd-1's matches overlap or go backwards at %v", m.Match.StartedAt)
			}
			last = *m.Match.EndedAt
		}
		if len(m.Teams) != 3 {
			t.Errorf("teams %+v", m.Teams)
		}
		seen := map[string]bool{}
		for _, p := range m.Players {
			if seen[p.Subject] || !strings.HasPrefix(p.Subject, wardogsim.StandInSteamIDPrefix) || p.Source != "server_adapter" || p.SecondsOn <= 0 || p.LastSeen.After(*m.Match.EndedAt) || p.FirstSeen.Before(m.Match.StartedAt) {
				t.Fatalf("player %+v in %+v", p, m.Match)
			}
			if want := map[string]string{"wd-1": "official", "wd-2": "community"}[m.Match.ServerID]; p.Trust != want {
				t.Fatalf("trust %q on %s", p.Trust, m.Match.ServerID)
			}
			seen[p.Subject] = true
			rows++
			if steamOf[p.Subject] {
				memberRows++
			}
		}
	}
	if share := float64(memberRows) / float64(rows); share < 0.1 || share > 0.6 {
		t.Errorf("members are %.0f%% of the rows; the pool should be mostly unlinked", share*100)
	}
	if len(keys) <= DefaultMembers*2 {
		t.Errorf("%d distinct players", len(keys))
	}

	// Moderation: in time order, by moderators, bans pointing at their entries, nobody banned plays.
	moderators := map[uuid.UUID]bool{}
	for _, m := range h.Members {
		if len(m.User.Roles) > 0 {
			moderators[m.User.ID] = true
		}
	}
	for i, e := range h.Moderation {
		if e.ActorUserID == nil || !moderators[*e.ActorUserID] || e.Outcome == "" || e.FinishedAt == nil || (i > 0 && e.At.Before(h.Moderation[i-1].At)) {
			t.Fatalf("entry %d: %+v", i, e)
		}
		if steamOf[e.TargetSubject] {
			t.Errorf("a member was moderated: %+v", e)
		}
	}
	if sum.AuditEntries < 30 || sum.Bans == 0 || len(h.BanLists) != 2 {
		t.Errorf("moderation: %+v, ban lists %v", sum, h.BanLists)
	}
	for _, b := range h.Bans {
		ban := h.Moderation[b.BanEntry]
		if ban.Action != "ban" || ban.TargetSubject != b.Subject || ban.ServerID != b.ServerID {
			t.Fatalf("ban %+v points at %+v", b, ban)
		}
		until := now
		if b.LiftEntry >= 0 {
			lift := h.Moderation[b.LiftEntry]
			if lift.Action != "unban" || lift.TargetSubject != b.Subject || !lift.At.After(ban.At) {
				t.Fatalf("ban %+v lifted by %+v", b, lift)
			}
			until = lift.At
		}
		for _, m := range h.Matches {
			if m.Match.ServerID != b.ServerID || m.Match.StartedAt.Before(ban.At) || !m.Match.StartedAt.Before(until) {
				continue
			}
			for _, p := range m.Players {
				if p.Subject == b.Subject {
					t.Fatalf("%s played on %s at %v while banned", p.Subject, b.ServerID, m.Match.StartedAt)
				}
			}
		}
	}
}

func TestGenerateRefusesNonsense(t *testing.T) {
	for name, opt := range map[string]Options{
		"too many members": {Members: 5000, Now: now},
		"no days":          {Days: -1, Now: now},
		"a decade":         {Days: 3650, Now: now},
		"no end":           {Members: 1},
	} {
		if _, _, _, err := Generate(servers, opt); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// fakeStore records what a seed asked of it.
type fakeStore struct {
	listed, seeded int
	refuse         error
	servers        []store.ManagedServer
}

func (f *fakeStore) ListManagedServers(context.Context, uuid.UUID, bool) ([]store.ManagedServer, error) {
	f.listed++
	return f.servers, nil
}

func (f *fakeStore) SeedDemo(context.Context, uuid.UUID, store.DemoHistory) error {
	f.seeded++
	return f.refuse
}

func TestSeedRefusesAHubThatIsNotADemo(t *testing.T) {
	st := &fakeStore{servers: servers}
	if _, _, err := Seed(context.Background(), st, uuid.New(), false, Options{Days: 3, Members: 5, Now: now}); !errors.Is(err, ErrNotDemo) {
		t.Errorf("seed without demo: %v", err)
	}
	if st.listed+st.seeded != 0 {
		t.Errorf("a refused seed touched the store: %+v", st)
	}
	none := &fakeStore{servers: servers[2:]} // only a cs2 server
	if _, _, err := Seed(context.Background(), none, uuid.New(), true, Options{Days: 3, Members: 5, Now: now}); !errors.Is(err, ErrNoServers) || none.seeded != 0 {
		t.Errorf("seed with no War Dogs server: %v, %+v", err, none)
	}
	st.refuse = &store.DemoRefusal{Census: store.DemoCensus{Users: 1}}
	if _, _, err := Seed(context.Background(), st, uuid.New(), true, Options{Days: 3, Members: 5, Now: now}); !errors.Is(err, store.ErrNotADemoDatabase) {
		t.Errorf("the store's refusal: %v", err)
	}
	st.refuse = nil
	sum, keys, err := Seed(context.Background(), st, uuid.New(), true, Options{Days: 3, Members: 5, Now: now})
	if err != nil || st.seeded != 2 || sum.Members != 5 || len(keys) == 0 {
		t.Errorf("a demo seed: %+v %v %v", sum, err, st)
	}
}
