package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

// demoDB is a reset database with an organization and one War Dogs server.
func demoDB(t *testing.T) (*store.Store, store.Organization) {
	t.Helper()
	st := storetest.Open(t)
	storetest.Reset(t, st)
	o := newOrg(t, st)
	srv := store.ManagedServer{ID: "wd-1", GameID: "wardogs", Name: "War Dogs #1", Driver: "wardogs", Location: "sim",
		Endpoint: "http://127.0.0.1:7789", CredentialFile: "/run/secrets/wd", PollInterval: 15 * time.Second, Trust: "official"}
	if err := st.ApplyServers(context.Background(), o.ID, []store.Game{{ID: "wardogs", Bands: json.RawMessage(`[]`)}}, []store.ManagedServer{srv}, time.Now()); err != nil {
		t.Fatal(err)
	}
	return st, o
}

func demoHistory(t0 time.Time) store.DemoHistory {
	mod := uuid.New()
	end := t0.Add(25 * time.Minute)
	finished := t0.Add(time.Hour + time.Second)
	lifted := t0.Add(2*time.Hour + time.Second)
	return store.DemoHistory{
		Members: []store.DemoMember{
			{User: store.User{ID: mod, DisplayName: "Atlas", CreatedAt: t0.Add(-24 * time.Hour), Roles: []string{store.RoleModerator}}, ShowName: true,
				Identities: []store.Identity{{Provider: "discord", Subject: "1000001", DisplayName: "Atlas", VerificationMethod: "oauth2", VerifiedAt: t0, LinkedAt: t0},
					{Provider: "steam", Subject: "76561190000000001", DisplayName: "AshAnvil", VerifiedAt: t0, LinkedAt: t0}}},
		},
		Matches: []store.DemoMatch{{
			Match: store.Match{ServerID: "wd-1", GameID: "wardogs", StartedAt: t0, EndedAt: &end, Map: "Ozeti", RotationIndex: 0},
			Teams: []store.TeamScore{{Name: "Lonestar", Color: "#4CB1EF", Score: 1200}, {Name: "Valkyra", Score: 900}},
			Players: []store.MatchPlayer{
				{Provider: "steam", Subject: "76561190000000001", Team: "Lonestar", Kills: 9, Deaths: 3, SecondsOn: 1500, FirstSeen: t0, LastSeen: end, Source: "server_adapter", Trust: "official"},
				{Provider: "steam", Subject: "76561190000000050", Team: "Valkyra", Kills: 3, Deaths: 9, SecondsOn: 1500, FirstSeen: t0, LastSeen: end, Source: "server_adapter", Trust: "official"},
			},
		}},
		Occupancy: []store.DemoOccupancy{{ServerID: "wd-1", Minute: t0, Fewest: 2, Most: 2}, {ServerID: "wd-1", Minute: t0.Add(time.Minute), Fewest: 1, Most: 2}},
		Moderation: []store.AuditEntry{
			{At: t0.Add(time.Hour), ActorUserID: &mod, ServerID: "wd-1", Action: "ban", TargetProvider: "steam", TargetSubject: "76561190000000050", Reason: "Team killing", Outcome: "ok", FinishedAt: &finished},
			{At: t0.Add(2 * time.Hour), ActorUserID: &mod, ServerID: "wd-1", Action: "unban", TargetProvider: "steam", TargetSubject: "76561190000000050", Reason: "Appeal accepted", Outcome: "ok", FinishedAt: &lifted},
		},
		Bans:     []store.DemoBan{{ServerID: "wd-1", Provider: "steam", Subject: "76561190000000050", Reason: "Team killing", BanEntry: 0, LiftEntry: 1}},
		BanLists: map[string]time.Time{"wd-1": t0},
	}
}

func TestSeedDemo(t *testing.T) {
	st, o := demoDB(t)
	ctx := context.Background()
	t0 := time.Date(2026, 10, 1, 20, 0, 0, 0, time.UTC)
	if c, err := st.DemoCensus(ctx); err != nil || c.Users+c.History() != 0 {
		t.Fatalf("an empty database: %+v %v", c, err)
	}
	if has, err := st.HasDemoMembers(ctx); err != nil || has {
		t.Fatalf("demo members before a seed: %v %v", has, err)
	}
	seeded := demoHistory(t0)
	if err := st.SeedDemo(ctx, o.ID, seeded); err != nil {
		t.Fatal(err)
	}
	c, err := st.DemoCensus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c.Users != 1 || c.DemoUsers != 1 || c.RealUsers() != 0 || c.Matches != 1 || c.AuditEntries != 2 || c.Bans != 1 || c.Occupancy != 2 {
		t.Errorf("after a seed: %+v", c)
	}
	if has, _ := st.HasDemoMembers(ctx); !has {
		t.Error("no demo members after a seed")
	}
	// The member is stored as made up, whatever the history said, and reads like any other.
	ids, err := st.ListIdentitiesForUsers(ctx, []uuid.UUID{seeded.Members[0].User.ID})
	if err != nil || len(ids) != 2 {
		t.Fatalf("identities: %v %v", ids, err)
	}
	for _, i := range ids {
		if i.VerificationMethod != store.DemoVerification {
			t.Errorf("%s identity stored as %q", i.Provider, i.VerificationMethod)
		}
	}
	if show, err := st.ShowNameOnBoards(ctx, ids[0].UserID); err != nil || !show {
		t.Errorf("show name: %v %v", show, err)
	}
	ms, err := st.ListMatches(ctx, o.ID, "wd-1", 0, 10)
	if err != nil || len(ms) != 1 || ms[0].Kills != 12 || ms[0].Players != 2 {
		t.Errorf("matches: %+v %v", ms, err)
	}
	if _, err := st.OpenMatch(ctx, o.ID, "wd-1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a seeded match is open: %v", err)
	}
	if active, err := st.ActiveBans(ctx, o.ID, "wd-1"); err != nil || len(active) != 0 {
		t.Errorf("the lifted ban is active: %v %v", active, err)
	}
	if adopted, _ := st.BanListAdopted(ctx, o.ID, "wd-1"); !adopted {
		t.Error("the ban list is not adopted")
	}
	entries, err := st.ListAuditEntries(ctx, o.ID, store.AuditFilter{Limit: 10})
	if err != nil || len(entries) != 2 || entries[0].Action != "unban" || entries[0].Outcome != "ok" {
		t.Errorf("audit: %+v %v", entries, err)
	}

	// A second seed is refused, and writes nothing.
	err = st.SeedDemo(ctx, o.ID, demoHistory(t0.Add(time.Hour)))
	var refusal *store.DemoRefusal
	if !errors.As(err, &refusal) || !errors.Is(err, store.ErrNotADemoDatabase) || refusal.Census.DemoUsers != 1 {
		t.Errorf("a second seed: %v", err)
	}
	if again, _ := st.DemoCensus(ctx); again != c {
		t.Errorf("a refused seed wrote: %+v, was %+v", again, c)
	}
}

func TestSeedDemoRefusesARealDatabase(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 10, 1, 20, 0, 0, 0, time.UTC)
	for name, fill := range map[string]func(t *testing.T, st *store.Store, o store.Organization){
		"a member who signed in": func(t *testing.T, st *store.Store, o store.Organization) {
			if _, err := st.RegisterUser(ctx, store.User{ID: uuid.New(), OrganizationID: o.ID, DisplayName: "Jo"}, ident("discord", "42", t0)); err != nil {
				t.Fatal(err)
			}
		},
		"a recorded match and nobody signed in": func(t *testing.T, st *store.Store, o store.Organization) {
			if _, err := st.StartMatch(ctx, store.Match{OrganizationID: o.ID, ServerID: "wd-1", GameID: "wardogs", StartedAt: t0, RotationIndex: -1}); err != nil {
				t.Fatal(err)
			}
		},
		"a minute of occupancy": func(t *testing.T, st *store.Store, o store.Organization) {
			if err := st.RecordOccupancy(ctx, o.ID, "wd-1", t0, 1, 3); err != nil {
				t.Fatal(err)
			}
		},
		"a registered bot and nobody signed in": func(t *testing.T, st *store.Store, o store.Organization) {
			if err := st.CreateApp(ctx, store.App{ID: uuid.New(), OrganizationID: o.ID, Name: "htg-bot", ClientID: "gravel_bot", SecretHash: []byte("x"), CreatedAt: t0}); err != nil {
				t.Fatal(err)
			}
		},
		"an ingested batch": func(t *testing.T, st *store.Store, o store.Organization) {
			if _, err := st.InsertIngestBatch(ctx, store.IngestBatch{OrganizationID: o.ID, ServerID: "wd-1", Source: "wardogs", ReceivedAt: t0, Body: []byte(`[]`)}); err != nil {
				t.Fatal(err)
			}
		},
		// A member with a demo identity and a real one signed in: real.
		"a demo member who linked a real account": func(t *testing.T, st *store.Store, o store.Organization) {
			h := demoHistory(t0)
			h.Matches, h.Occupancy, h.Moderation, h.Bans, h.BanLists = nil, nil, nil, nil, nil
			if err := st.SeedDemo(ctx, o.ID, h); err != nil {
				t.Fatal(err)
			}
			if err := st.LinkIdentity(ctx, store.Identity{Provider: "twitch", Subject: "99", UserID: h.Members[0].User.ID, VerificationMethod: "oauth2", VerifiedAt: t0}); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			st, o := demoDB(t)
			fill(t, st, o)
			before, err := st.DemoCensus(ctx)
			if err != nil {
				t.Fatal(err)
			}
			err = st.SeedDemo(ctx, o.ID, demoHistory(t0))
			var refusal *store.DemoRefusal
			if !errors.As(err, &refusal) {
				t.Fatalf("seeded: %v", err)
			}
			if after, _ := st.DemoCensus(ctx); after != before {
				t.Errorf("a refused seed wrote: %+v, was %+v", after, before)
			}
			t.Log(err)
		})
	}
}
