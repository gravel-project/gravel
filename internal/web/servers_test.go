package web_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
)

// fakeServers is ServerService with canned answers: the pages are API clients, so a stub is all
// they need (the procedures' own rules are tested in internal/api).
type fakeServers struct {
	hubv1connect.UnimplementedServerServiceHandler
	mu         sync.Mutex
	games      []*hubv1.Game
	servers    []*hubv1.Server
	players    []*hubv1.ServerPlayer
	playersErr connect.Code // 0: answer
}

func newFakeServers() *fakeServers {
	observed := timestamppb.New(time.Date(2026, 10, 10, 20, 0, 0, 0, time.UTC))
	return &fakeServers{
		games: []*hubv1.Game{{Id: "wardogs", Name: "War Dogs", Layout: &hubv1.GameLayout{Board: []string{"kills", "deaths", "kd", "time", "matches"}}}},
		servers: []*hubv1.Server{
			{Id: "htg-wardogs-1", Name: "HTG WARDOGS | NA WEST | #1", GameId: "wardogs", Location: "qonzer-slc", Trust: "official",
				Status: &hubv1.ServerStatus{State: "ok", Reachable: true, ObservedAt: observed, Players: 24, MaxPlayers: 80, Map: "Ozeti",
					Teams: []*hubv1.TeamScore{{Name: "Lonestar", Score: 412}, {Name: "Boreal", Score: 388}}}},
			{Id: "friends-1", Name: "Friends' server", GameId: "wardogs", Location: "home", Trust: "community",
				Status: &hubv1.ServerStatus{State: "unreachable"}},
		},
		players: []*hubv1.ServerPlayer{
			{Name: "Jo <script>", Team: "Lonestar", Kills: 7, Deaths: 2, PingMs: 41, UserId: "u1"},
			{Name: "Stranger", Team: "Boreal", Kills: 1, Deaths: 5, PingMs: 90},
		},
	}
}

func (f *fakeServers) ListGames(context.Context, *connect.Request[hubv1.ListGamesRequest]) (*connect.Response[hubv1.ListGamesResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return connect.NewResponse(&hubv1.ListGamesResponse{Games: f.games}), nil
}

func (f *fakeServers) ListServers(context.Context, *connect.Request[hubv1.ListServersRequest]) (*connect.Response[hubv1.ListServersResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return connect.NewResponse(&hubv1.ListServersResponse{Servers: f.servers}), nil
}

func (f *fakeServers) GetServerStatus(_ context.Context, req *connect.Request[hubv1.GetServerStatusRequest]) (*connect.Response[hubv1.GetServerStatusResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.servers {
		if s.GetId() == req.Msg.GetServerId() {
			return connect.NewResponse(&hubv1.GetServerStatusResponse{Server: s}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, errors.New("no such server"))
}

func (f *fakeServers) ListServerPlayers(context.Context, *connect.Request[hubv1.ListServerPlayersRequest]) (*connect.Response[hubv1.ListServerPlayersResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.playersErr != 0 {
		return nil, connect.NewError(f.playersErr, errors.New("refused"))
	}
	return connect.NewResponse(&hubv1.ListServerPlayersResponse{Players: f.players}), nil
}

// fakeStats is StatsService with canned answers; it remembers the last board request.
type fakeStats struct {
	hubv1connect.UnimplementedStatsServiceHandler
	mu      sync.Mutex
	err     connect.Code // 0: answer
	last    *hubv1.GetBoardRequest
	entries []*hubv1.BoardEntry
	next    string
	matches []*hubv1.MatchSummary
}

func (f *fakeStats) GetBoard(_ context.Context, req *connect.Request[hubv1.GetBoardRequest]) (*connect.Response[hubv1.GetBoardResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last = req.Msg
	if f.err != 0 {
		return nil, connect.NewError(f.err, errors.New("no season named \"Nope\""))
	}
	resp := &hubv1.GetBoardResponse{Entries: f.entries, NextPageToken: f.next}
	if req.Msg.GetWindow() == "week" {
		resp.From = timestamppb.New(time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)) // the rig's organization is on UTC
		resp.To = timestamppb.New(time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC))
	}
	return connect.NewResponse(resp), nil
}

func (f *fakeStats) ListMatches(context.Context, *connect.Request[hubv1.ListMatchesRequest]) (*connect.Response[hubv1.ListMatchesResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != 0 {
		return nil, connect.NewError(f.err, errors.New("refused"))
	}
	return connect.NewResponse(&hubv1.ListMatchesResponse{Matches: f.matches}), nil
}

func (f *fakeStats) set(err connect.Code) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeStats) request() *hubv1.GetBoardRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

func TestServersPage(t *testing.T) {
	r := newRig(t)
	resp, body := get(t, browser(t), r.srv.URL+"/servers")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("servers: %d", resp.StatusCode)
	}
	for _, want := range []string{"HTG WARDOGS | NA WEST | #1", `href="/servers/htg-wardogs-1"`, "Online", "24/80 players", "Ozeti", "official",
		"Friends&#39; server", "Offline", "community", `href="/boards"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the server list lacks %q", want)
		}
	}
	// An offline server shows no player count or map.
	if i := strings.Index(body, "Offline"); i >= 0 && strings.Contains(body[i:], "0/0 players") {
		t.Error("an offline server shows a player count")
	}
	// htmx gets only the content.
	if _, frag := get(t, browser(t), r.srv.URL+"/servers", "HX-Request", "true"); strings.Contains(frag, "<html") || !strings.Contains(frag, "<h1>Servers</h1>") {
		t.Errorf("htmx fragment: %s", frag)
	}
}

func TestServerPage(t *testing.T) {
	r := newRig(t)
	r.stats.matches = []*hubv1.MatchSummary{
		{Id: "2", StartedAt: timestamppb.New(time.Date(2026, 10, 10, 19, 30, 0, 0, time.UTC)), Map: "Ozeti", Players: 24, Kills: 310},
		{Id: "1", StartedAt: timestamppb.New(time.Date(2026, 10, 10, 18, 0, 0, 0, time.UTC)), EndedAt: timestamppb.New(time.Date(2026, 10, 10, 18, 42, 0, 0, time.UTC)), Map: "Bakurani", Players: 31, Kills: 402},
	}

	// A member sees who is on, with names escaped.
	r.servers.playersErr = 0
	resp, body := get(t, browser(t), r.srv.URL+"/servers/htg-wardogs-1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("server: %d", resp.StatusCode)
	}
	for _, want := range []string{"<h1>HTG WARDOGS | NA WEST | #1</h1>", "As of Oct 10, 20:00 UTC", "Lonestar", "412", "Jo &lt;script&gt;", ">member<",
		"41 ms", "Bakurani", "42m", ">live<", `href="/boards?scope=server%3Ahtg-wardogs-1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the server page lacks %q", want)
		}
	}
	if strings.Contains(body, "Jo <script>") {
		t.Error("a player name was not escaped")
	}

	// Anonymous: the API refuses the player list and, while stats are private, the matches.
	r.servers.playersErr = connect.CodeUnauthenticated
	r.stats.set(connect.CodePermissionDenied)
	_, body = get(t, browser(t), r.srv.URL+"/servers/htg-wardogs-1")
	if !strings.Contains(body, "Log in to see who&#39;s on.") || !strings.Contains(body, "Match history is private on this hub.") || strings.Contains(body, "Stranger") {
		t.Errorf("anonymous server page: %s", body)
	}
	r.servers.playersErr = connect.CodeUnavailable
	if _, body = get(t, browser(t), r.srv.URL+"/servers/htg-wardogs-1"); !strings.Contains(body, "isn&#39;t answering") {
		t.Errorf("an unavailable server: %s", body)
	}

	if resp, _ := get(t, browser(t), r.srv.URL+"/servers/nope"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown server: %d", resp.StatusCode)
	}
}

func TestBoardsPage(t *testing.T) {
	r := newRig(t)
	r.stats.entries = []*hubv1.BoardEntry{
		{Rank: 1, Name: "Brave Falcon", Pseudonymous: true, Kills: 42, Deaths: 10, Kd: 4.2, SecondsOn: 7500, Matches: 5},
		{Rank: 2, Name: "Jo", UserId: "u1", Kills: 30, Deaths: 12, Kd: 2.5, SecondsOn: 2700, Matches: 4},
	}
	r.stats.next = "b2Zmc2V0OjUw"

	// The organization's board, ranked by the layout's first column.
	resp, body := get(t, browser(t), r.srv.URL+"/boards")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("boards: %d", resp.StatusCode)
	}
	req := r.stats.request()
	if req.GetServerId() != "" || req.GetGameId() != "" || req.GetWindow() != "all" || req.GetMetric() != "kills" || req.GetPageSize() != 50 {
		t.Errorf("request = %v", req)
	}
	for _, want := range []string{"Brave Falcon", "4.20", "2h 05m", "45m", `aria-sort="descending"`, "Kills ▼", ">member<",
		`<option value="game:wardogs">War Dogs</option>`, `<option value="server:htg-wardogs-1">`, "Next page", "page=b2Zmc2V0OjUw", "appear under a pseudonym"} {
		if !strings.Contains(body, want) {
			t.Errorf("the board lacks %q", want)
		}
	}
	if strings.Index(body, "Brave Falcon") > strings.Index(body, ">Jo") {
		t.Error("the rows are out of order")
	}

	// A server's board this week, ranked by K/D.
	q := url.Values{"scope": {"server:htg-wardogs-1"}, "window": {"week"}, "metric": {"kd"}}
	_, body = get(t, browser(t), r.srv.URL+"/boards?"+q.Encode())
	req = r.stats.request()
	if req.GetServerId() != "htg-wardogs-1" || req.GetWindow() != "week" || req.GetMetric() != "kd" {
		t.Errorf("request = %v", req)
	}
	if !strings.Contains(body, "HTG WARDOGS | NA WEST | #1 leaderboard") || !strings.Contains(body, "K/D ▼") || !strings.Contains(body, "Oct 5, 2026 to Oct 11, 2026") ||
		!strings.Contains(body, `<option value="week" selected>`) {
		t.Errorf("server board: %s", body)
	}

	// A season, by name; an unknown metric falls back to the first column.
	q = url.Values{"scope": {"game:wardogs"}, "window": {"season:Season 02"}, "metric": {"rating"}}
	get(t, browser(t), r.srv.URL+"/boards?"+q.Encode())
	if req = r.stats.request(); req.GetGameId() != "wardogs" || req.GetWindow() != "season" || req.GetSeason() != "Season 02" || req.GetMetric() != "kills" {
		t.Errorf("request = %v", req)
	}

	// Refusals become words on the page.
	r.stats.set(connect.CodePermissionDenied)
	if resp, body = get(t, browser(t), r.srv.URL+"/boards"); resp.StatusCode != http.StatusOK || !strings.Contains(body, "Leaderboards are private") || strings.Contains(body, "Brave Falcon") {
		t.Errorf("private boards: %d %s", resp.StatusCode, body)
	}
	r.stats.set(connect.CodeInvalidArgument)
	if resp, body = get(t, browser(t), r.srv.URL+"/boards?window=season:Nope"); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "no season named") {
		t.Errorf("a bad season: %d %s", resp.StatusCode, body)
	}
	for _, scope := range []string{"server:nope", "game:quake", "planet:earth"} {
		if resp, _ := get(t, browser(t), r.srv.URL+"/boards?scope="+url.QueryEscape(scope)); resp.StatusCode != http.StatusNotFound {
			t.Errorf("scope %s: %d", scope, resp.StatusCode)
		}
	}
}
