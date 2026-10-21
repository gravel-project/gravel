package web_test

import (
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
)

// withSeaOfThieves adds a server-less game to the rig: no driver, no combat boards (gravel#2).
func withSeaOfThieves(r *rig) {
	r.servers.games = append(r.servers.games, &hubv1.Game{Id: "seaofthieves", Name: "Sea of Thieves", IdentityProvider: "xbox",
		ServerLess: true, Sources: []string{"first_party", "self_report"}})
}

// The games list: each enabled game, by name, with its servers or its lack of them, and the site's
// navigation leads to it.
func TestGamesList(t *testing.T) {
	r := newRig(t)
	withSeaOfThieves(r)
	resp, body := get(t, browser(t), r.srv.URL+"/games")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("games: %d", resp.StatusCode)
	}
	for _, want := range []string{`<a href="/games">Games</a>`, `<a href="/games/seaofthieves">Sea of Thieves</a>`, "No game servers: its community&#39;s participation",
		`<a href="/games/wardogs">War Dogs</a>`, "2 servers<"} {
		if !strings.Contains(body, want) {
			t.Errorf("the list lacks %q", want)
		}
	}
	if strings.Index(body, "Sea of Thieves</a>") > strings.Index(body, "War Dogs</a>") {
		t.Error("the games are not sorted by name")
	}
}

// A game with servers: its overlay, its servers, the top of its leaderboard and its participation.
func TestGamePageWithServers(t *testing.T) {
	r := newRig(t)
	r.stats.mu.Lock()
	r.stats.entries = []*hubv1.BoardEntry{{Rank: 1, Name: "Jo", UserId: "u1", Kills: 12, Deaths: 3, Kd: 4, SecondsOn: 600, Matches: 2}}
	r.stats.partKinds = []string{"seeding"}
	r.stats.partEntries = []*hubv1.ParticipationEntry{{Rank: 1, Name: "Brave Falcon", Pseudonymous: true, Events: 2, Seconds: 1800}}
	r.stats.mu.Unlock()

	resp, body := get(t, browser(t), r.srv.URL+"/games/wardogs")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("game: %d", resp.StatusCode)
	}
	for _, want := range []string{`<div class="game" data-game="wardogs">`, "<h1>War Dogs</h1>", "<h2>Servers</h2>", `<h3><a href="/servers/htg-wardogs-1">`,
		"<h2>Leaderboard</h2>", `<a href="/members/u1">Jo</a>`, `<th scope="col" class="num" aria-sort="descending">Kills ▼</th>`,
		`href="/boards?scope=game%3Awardogs"`, "<h2>Participation</h2>", "<caption>Seeding</caption>", "Brave Falcon",
		`href="/participation?game=wardogs&amp;kind=seeding"`, "appear under a pseudonym"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	r.stats.mu.Lock()
	board, part := r.stats.last, r.stats.partLast
	r.stats.mu.Unlock()
	if board.GetGameId() != "wardogs" || board.GetMetric() != "kills" || board.GetPageSize() != 10 || board.GetWindow() != "" {
		t.Errorf("board request = %v", board)
	}
	if part.GetGameId() != "wardogs" || part.GetPageSize() != 10 {
		t.Errorf("participation request = %v", part)
	}
}

// A server-less game's page is its participation: no servers section, no leaderboard, a board per
// kind recorded for it, each read for the game (gravel#2).
func TestServerLessGamePage(t *testing.T) {
	r := newRig(t)
	withSeaOfThieves(r)

	// Nothing recorded yet.
	resp, body := get(t, browser(t), r.srv.URL+"/games/seaofthieves")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Nothing has been recorded for Sea of Thieves yet") || strings.Contains(body, "pseudonym") {
		t.Errorf("empty: %d %s", resp.StatusCode, body)
	}

	r.stats.mu.Lock()
	r.stats.partKinds = []string{"crew", "voyage"}
	r.stats.partEntries = []*hubv1.ParticipationEntry{{Rank: 1, Name: "Jo", UserId: "u1", Events: 4, Seconds: 7200}}
	r.stats.partAll = nil
	r.stats.last = nil
	r.stats.mu.Unlock()
	resp, body = get(t, browser(t), r.srv.URL+"/games/seaofthieves", "HX-Request", "true")
	if resp.StatusCode != http.StatusOK || strings.Contains(body, "<html") {
		t.Fatalf("game fragment: %d", resp.StatusCode)
	}
	for _, want := range []string{`<div class="game" data-game="seaofthieves">`, "<h1>Sea of Thieves</h1>", "<caption>Crew</caption>", "<caption>Voyage</caption>",
		`href="/participation?game=seaofthieves&amp;kind=crew"`, `href="/participation?game=seaofthieves&amp;kind=voyage"`, "2h 00m"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	for _, never := range []string{"<h2>Servers</h2>", "<h2>Leaderboard</h2>", "/boards?scope"} {
		if strings.Contains(body, never) {
			t.Errorf("a server-less game's page has %q", never)
		}
	}
	r.stats.mu.Lock()
	all, board := r.stats.partAll, r.stats.last
	r.stats.mu.Unlock()
	if board != nil {
		t.Errorf("a server-less game's page read a combat board: %v", board)
	}
	if len(all) != 2 || all[0].GetKind() != "" || all[1].GetKind() != "voyage" || all[0].GetGameId() != "seaofthieves" || all[1].GetGameId() != "seaofthieves" {
		t.Errorf("participation requests = %v", all)
	}

	// Private participation is words on the page.
	r.stats.setPart(connect.CodePermissionDenied)
	if _, body := get(t, browser(t), r.srv.URL+"/games/seaofthieves"); !strings.Contains(body, "Participation boards are private") || strings.Contains(body, "<caption>") {
		t.Errorf("private: %s", body)
	}

	// An unknown game is no page.
	if resp, _ := get(t, browser(t), r.srv.URL+"/games/quake"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown game: %d", resp.StatusCode)
	}
}

// The leaderboards offer no server-less game, and refuse one by name; its participation board is
// scoped to it, in its overlay (gravel#2).
func TestServerLessGameBoards(t *testing.T) {
	r := newRig(t)
	withSeaOfThieves(r)
	_, body := get(t, browser(t), r.srv.URL+"/boards")
	if strings.Contains(body, "game:seaofthieves") || !strings.Contains(body, `<option value="game:wardogs">War Dogs</option>`) {
		t.Errorf("the board picker: %s", body)
	}
	if resp, body := get(t, browser(t), r.srv.URL+"/boards?scope=game:seaofthieves"); resp.StatusCode != http.StatusNotFound ||
		!strings.Contains(body, "Sea of Thieves has no leaderboard") {
		t.Errorf("a server-less game's board: %d %s", resp.StatusCode, body)
	}

	r.stats.mu.Lock()
	r.stats.partKinds = []string{"crew"}
	r.stats.mu.Unlock()
	resp, body := get(t, browser(t), r.srv.URL+"/participation?game=seaofthieves")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("participation: %d", resp.StatusCode)
	}
	for _, want := range []string{"<h1>Sea of Thieves participation</h1>", `<div class="game" data-game="seaofthieves">`, `<option value="seaofthieves" selected>Sea of Thieves</option>`,
		`<option value="">Every game</option>`, `<label for="game">Game</label>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the board lacks %q", want)
		}
	}
	if req := r.stats.partRequest(); req.GetGameId() != "seaofthieves" {
		t.Errorf("request = %v", req)
	}
	if resp, _ := get(t, browser(t), r.srv.URL+"/participation?game=quake"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown game's participation: %d", resp.StatusCode)
	}
}
