package hub_test

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/apps"
	"github.com/gravel-project/gravel/internal/hub"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/store"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

// A server-less game end to end (gravel#2): Sea of Thieves enabled with no servers, so nothing is
// polled; an app records a crew's participation by the members' Xbox identities, which only a
// Sea of Thieves event may name; its participation board and its page show it, and no leaderboard.
func TestServerLessGame(t *testing.T) {
	cfg := testConfig(t)
	cfg.Stats.PseudonymKey = "a test pseudonym key, at least 32 bytes long"
	st := storetest.Open(t)
	storetest.Reset(t, st)
	t.Cleanup(func() { storetest.Reset(t, st) })
	ctx := context.Background()
	h, err := hub.New(ctx, hub.Options{Config: cfg, Logger: quiet(), Version: "test", Providers: []identity.Registration{}})
	if err != nil {
		t.Fatal(err)
	}
	o, err := st.GetBuiltinOrganization(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := servers.New(st, o.ID, hub.DriverNames(), quiet()).Apply(ctx, servers.Manifest{Games: []servers.GameRef{{ID: "seaofthieves"}}}, servers.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := org.New(st, cfg.Claim.TokenTTL, quiet()).UpdateSettings(ctx, org.Settings{Stats: org.Stats{Public: true}}, store.Actor{}); err != nil {
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

	// Listed as server-less, with no combat boards and no servers.
	srv := hubv1connect.NewServerServiceClient(http.DefaultClient, base)
	games, err := srv.ListGames(ctx, connect.NewRequest(&hubv1.ListGamesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if g := games.Msg.GetGames(); len(g) != 1 || g[0].GetId() != "seaofthieves" || !g[0].GetServerLess() || g[0].GetCombatBoards() ||
		!slices.Equal(g[0].GetSources(), []string{"first_party", "self_report"}) {
		t.Errorf("games = %v", g)
	}
	if list, err := srv.ListServers(ctx, connect.NewRequest(&hubv1.ListServersRequest{})); err != nil || len(list.Msg.GetServers()) != 0 {
		t.Errorf("servers = %v, %v", list.Msg.GetServers(), err)
	}

	// An app records a crew: two members by their Xbox identities, an hour each.
	svc := apps.New(st, o.ID, time.Hour, quiet())
	c, err := svc.Create(ctx, store.Actor{}, "crews", []string{apps.ScopeParticipationWrite})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := svc.Issue(ctx, c.App.ClientID, c.Secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	app := hubv1connect.NewStatsServiceClient(&http.Client{Transport: bearerTransport{next: http.DefaultTransport, token: tok.Value}}, base)
	at := timestamppb.New(time.Now().Add(-2 * time.Hour))
	crew := func(key, xbox, game string) *hubv1.ParticipationEvent {
		return &hubv1.ParticipationEvent{Key: key, Kind: "crew", Identity: &hubv1.PlayerIdentity{Provider: "xbox", Subject: xbox}, GameId: game,
			OccurredAt: at, DurationSeconds: 3600}
	}
	resp, err := app.RecordParticipation(ctx, connect.NewRequest(&hubv1.RecordParticipationRequest{Events: []*hubv1.ParticipationEvent{
		crew("voyage-1/a", "2535400000000001", "seaofthieves"), crew("voyage-1/b", "2535400000000002", "seaofthieves")}}))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range resp.Msg.GetResults() {
		if r.GetOutcome() != "recorded" {
			t.Errorf("result = %v", r)
		}
	}
	// An Xbox identity names a Sea of Thieves event only.
	_, err = app.RecordParticipation(ctx, connect.NewRequest(&hubv1.RecordParticipationRequest{Events: []*hubv1.ParticipationEvent{
		crew("voyage-2/a", "2535400000000001", "")}}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), `identity.provider: "xbox" is not one of discord, steam`) {
		t.Errorf("an Xbox identity without the game: %v", err)
	}

	// Its participation board, by game; another game's is empty.
	stats := hubv1connect.NewStatsServiceClient(http.DefaultClient, base)
	board, err := stats.GetParticipationBoard(ctx, connect.NewRequest(&hubv1.GetParticipationBoardRequest{GameId: "seaofthieves"}))
	if err != nil {
		t.Fatal(err)
	}
	if m := board.Msg; m.GetKind() != "crew" || !slices.Equal(m.GetKinds(), []string{"crew"}) || len(m.GetEntries()) != 2 || m.GetEntries()[0].GetSeconds() != 3600 ||
		!m.GetEntries()[0].GetPseudonymous() {
		t.Errorf("board = %v", m)
	}
	if other, err := stats.GetParticipationBoard(ctx, connect.NewRequest(&hubv1.GetParticipationBoardRequest{GameId: "wardogs"})); err != nil || len(other.Msg.GetKinds()) != 0 {
		t.Errorf("War Dogs' board = %v, %v", other.Msg, err)
	}

	// Its page: the crew's board, no servers and no leaderboard.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/games/seaofthieves", nil)
	if err != nil {
		t.Fatal(err)
	}
	page, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(page.Body)
	_ = page.Body.Close()
	if page.StatusCode != http.StatusOK || !strings.Contains(string(body), "<caption>Crew</caption>") ||
		strings.Contains(string(body), "<h2>Servers</h2>") || strings.Contains(string(body), "<h2>Leaderboard</h2>") {
		t.Errorf("the game page: %d %s", page.StatusCode, body)
	}
}
