package hub_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/gravel-project/gravel/games/wardogs/wardogstest"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/apps"
	"github.com/gravel-project/gravel/internal/hub"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/ingest"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

// The stats sources' conformance (gravel#3, ADR-0011's trust mapping), one case per source that
// has a producer, each read back through the API:
//
//   - server_adapter, the poll: a match and its totals, on the combat boards of every scope;
//   - server_adapter, the feed: kill events in stats_events, on no board (the boards count from
//     the poll; the feed's events reach them in a later step of #17);
//   - server_adapter, a gap: listed by ListGaps, on no board and never counted as a kill;
//   - first_party, an app's participation: on the participation board only (ADR-0014).
//
// platform_api (a publisher's API, PUBG) and self_report (Sea of Thieves and Star Citizen
// milestones) have no producer yet; their cases come with them (gravel#2).
func TestStatsSourcesConformance(t *testing.T) {
	cfg := testConfig(t)
	cfg.Stats.PseudonymKey = "a test pseudonym key, at least 32 bytes long"
	st := storetest.Open(t)
	storetest.Reset(t, st)
	t.Cleanup(func() { storetest.Reset(t, st) })
	const ana, ben = "76561190000000001", "76561190000000002"
	fake := wardogstest.New(t, "../../games/wardogs/testdata/CL-509546", wardogstest.Options{Handle: map[string]http.HandlerFunc{
		"GET /v1/players": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"players":[{"name":"Ana","steamId":"`+ana+`","faction":"Lonestar","kills":3,"deaths":1,"cash":0,"pingMs":40},`+
				`{"name":"Ben","steamId":"`+ben+`","faction":"Valkyra","kills":1,"deaths":3,"cash":0,"pingMs":60}],"count":2}`)
		},
	}})
	cred := filepath.Join(t.TempDir(), "rcon")
	if err := os.WriteFile(cred, []byte(wardogstest.Token), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	h, err := hub.New(ctx, hub.Options{Config: cfg, Logger: quiet(), Version: "test", Providers: []identity.Registration{}})
	if err != nil {
		t.Fatal(err)
	}
	o, err := st.GetBuiltinOrganization(ctx)
	if err != nil {
		t.Fatal(err)
	}
	m := servers.Manifest{Games: []servers.GameRef{{ID: "wardogs"}}, Servers: []servers.Server{{ID: "wd-1", Name: "War Dogs #1", Game: "wardogs",
		Driver: "wardogs", Location: "slc", Endpoint: fake.URL, CredentialFile: cred, PollInterval: servers.Duration(5 * time.Second), Trust: servers.TrustOfficial}}}
	if _, err := servers.New(st, o.ID, hub.DriverNames(), quiet()).Apply(ctx, m, servers.ApplyOptions{CheckCredentials: true}); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- h.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	<-h.Started()
	base := "http://" + h.PublicAddr().String()
	anon := hubv1connect.NewStatsServiceClient(http.DefaultClient, base)

	// server_adapter, the poll: a match with Ana's 3 kills and Ben's 1.
	deadline := time.Now().Add(30 * time.Second)
	for {
		if ms, err := st.ListMatches(ctx, o.ID, "wd-1", 0, 10); err == nil && len(ms) == 1 && ms[0].Kills == 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no match recorded from the polls")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// server_adapter, the feed: 50 of Ana's kills of Ben, and a gap, as the parser and the gap
	// detectors write them.
	// Whole seconds: Postgres keeps microseconds of a row's time and the gap's end keeps what it was given.
	now := time.Now().UTC().Truncate(time.Second)
	var feed []store.StatsEvent
	for i := range 50 {
		feed = append(feed, store.StatsEvent{OrganizationID: o.ID, ServerID: "wd-1", GameID: "wardogs", Source: ingest.SourceServerAdapter,
			SourceKey: fmt.Sprintf("kill-%d", i), Kind: ingest.KindKill, ObservedAt: now.Add(-10 * time.Minute), ReceivedAt: now, Trust: "official",
			SchemaVersion: ingest.StatsSchemaVersion, Player: store.PlayerKey{Provider: "steam", Subject: ana}, Target: store.PlayerKey{Provider: "steam", Subject: ben}})
	}
	feed = append(feed, store.StatsEvent{OrganizationID: o.ID, ServerID: "wd-1", GameID: "wardogs", Source: ingest.SourceServerAdapter,
		SourceKey: "gap:dropped:1", Kind: ingest.KindGap, ObservedAt: now.Add(-time.Hour), ReceivedAt: now, Trust: "official",
		SchemaVersion: ingest.StatsSchemaVersion, Extensions: []byte(`{"reason":"dropped","to":"` + now.Add(-59*time.Minute).Format(time.RFC3339Nano) + `","events":7}`)})
	if _, err := st.RecordStatsEvents(ctx, feed); err != nil {
		t.Fatal(err)
	}

	// While stats are private, the gaps are as closed as the boards.
	if _, err := anon.ListGaps(ctx, connect.NewRequest(&hubv1.ListGapsRequest{ServerId: "wd-1"})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("private gaps, anonymously: %v", err)
	}
	if _, err := org.New(st, cfg.Claim.TokenTTL, quiet()).UpdateSettings(ctx, org.Settings{Stats: org.Stats{Public: true}}, store.Actor{}); err != nil {
		t.Fatal(err)
	}

	// first_party: an app's participation for Ana (by her Steam id), 10 minutes of seeding.
	svc := apps.New(st, o.ID, time.Hour, quiet())
	c, err := svc.Create(ctx, store.Actor{}, "seeder", []string{apps.ScopeParticipationWrite})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := svc.Issue(ctx, c.App.ClientID, c.Secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	app := hubv1connect.NewStatsServiceClient(&http.Client{Transport: bearerTransport{next: http.DefaultTransport, token: tok.Value}}, base)
	if _, err := app.RecordParticipation(ctx, connect.NewRequest(&hubv1.RecordParticipationRequest{Events: []*hubv1.ParticipationEvent{{
		Key: "seed-1/ana", Kind: "seeding", Identity: &hubv1.PlayerIdentity{Provider: "steam", Subject: ana}, GameId: "wardogs", ServerId: "wd-1",
		OccurredAt: timestamppb.New(now.Add(-30 * time.Minute)), DurationSeconds: 600}}})); err != nil {
		t.Fatal(err)
	}

	// The combat boards, at every scope, hold the poll's numbers only: not the feed's 50 kills,
	// not the gap, not the seeding.
	for name, req := range map[string]*hubv1.GetBoardRequest{"server": {ServerId: "wd-1"}, "game": {GameId: "wardogs"}, "organization": {}} {
		b, err := anon.GetBoard(ctx, connect.NewRequest(req))
		if err != nil {
			t.Fatalf("%s board: %v", name, err)
		}
		es := b.Msg.GetEntries()
		if len(es) != 2 || es[0].GetKills() != 3 || es[0].GetDeaths() != 1 || es[1].GetKills() != 1 || es[1].GetDeaths() != 3 {
			t.Errorf("%s board: %v, want the poll's 3/1 and 1/3", name, es)
		}
	}
	ms, err := anon.ListMatches(ctx, connect.NewRequest(&hubv1.ListMatchesRequest{ServerId: "wd-1"}))
	if err != nil || len(ms.Msg.GetMatches()) != 1 || ms.Msg.GetMatches()[0].GetKills() != 4 {
		t.Errorf("matches: %v %v, want the one polled match with 4 kills", ms, err)
	}

	// The participation board holds the seeding only: Ana, and not Ben, whom only the poll saw.
	pb, err := anon.GetParticipationBoard(ctx, connect.NewRequest(&hubv1.GetParticipationBoardRequest{Kind: "seeding"}))
	if err != nil {
		t.Fatal(err)
	}
	if es := pb.Msg.GetEntries(); len(es) != 1 || es[0].GetSeconds() != 600 || es[0].GetEvents() != 1 {
		t.Errorf("participation board: %v, want Ana's one seeding of 600 s", es)
	}
	if kinds := pb.Msg.GetKinds(); len(kinds) != 1 || kinds[0] != "seeding" {
		t.Errorf("participation kinds: %v, want only seeding (a kill or a gap is no participation)", kinds)
	}

	// The gap is listed, in its own fields, and the kills beside it are not.
	g, err := anon.ListGaps(ctx, connect.NewRequest(&hubv1.ListGapsRequest{ServerId: "wd-1"}))
	if err != nil {
		t.Fatal(err)
	}
	if gs := g.Msg.GetGaps(); len(gs) != 1 || gs[0].GetReason() != "dropped" || gs[0].GetEvents() != 7 || g.Msg.GetMore() ||
		gs[0].GetTo().AsTime().Sub(gs[0].GetFrom().AsTime()) != time.Minute {
		t.Errorf("gaps: %v", g.Msg)
	}
	if _, err := anon.ListGaps(ctx, connect.NewRequest(&hubv1.ListGapsRequest{ServerId: "nope"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("an unknown server's gaps: %v", err)
	}
	if _, err := anon.ListGaps(ctx, connect.NewRequest(&hubv1.ListGapsRequest{ServerId: "wd-1", From: timestamppb.New(now.Add(-91 * 24 * time.Hour))})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a window over 90 days: %v", err)
	}
}
