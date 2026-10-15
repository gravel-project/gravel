package web_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
)

// fakeResult is the one match result the fake stats service has.
var fakeResult = &hubv1.MatchResult{Id: "r1", ServerId: "htg-wardogs-1", Team1Name: "Alpha <b>", Team2Name: "Bravo", NumMaps: 1, Team1Score: 1, Winner: "team1",
	StartedAt: timestamppb.New(time.Date(2026, 10, 14, 1, 0, 0, 0, time.UTC)), EndedAt: timestamppb.New(time.Date(2026, 10, 14, 2, 0, 0, 0, time.UTC)),
	Maps: []*hubv1.MatchResultMap{{MapNumber: 0, MapName: "de_nuke", Team1Score: 13, Team2Score: 9, Winner: "team1", HasDemo: true}},
	Players: []*hubv1.MatchResultPlayer{
		{MapNumber: 0, Name: "Jo", UserId: "u1", Team: "team1", Kills: 21, Deaths: 12, Extensions: map[string]float64{"assists": 4, "damage": 2310, "mvps": 0}},
		{MapNumber: 0, Name: "Brave Falcon", Pseudonymous: true, Team: "team2", Kills: 15, Deaths: 18},
	}}

func (f *fakeStats) GetMatchResult(_ context.Context, req *connect.Request[hubv1.GetMatchResultRequest]) (*connect.Response[hubv1.GetMatchResultResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != 0 {
		return nil, connect.NewError(f.err, errors.New("private"))
	}
	if req.Msg.GetId() != "r1" {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("no such result"))
	}
	return connect.NewResponse(&hubv1.GetMatchResultResponse{Result: fakeResult, StatExtensions: []*hubv1.StatExtension{
		{Name: "assists", Label: "Assists", Type: "int", Aggregate: "sum"},
		{Name: "damage", Label: "Damage", Type: "int", Unit: "HP", Aggregate: "sum"},
		{Name: "mvps", Label: "MVPs", Type: "int", Aggregate: "sum"},
	}}), nil
}

func (f *fakeStats) GetMatchDemo(_ context.Context, req *connect.Request[hubv1.GetMatchDemoRequest]) (*connect.Response[hubv1.GetMatchDemoResponse], error) {
	if req.Msg.GetId() != "r1" || req.Msg.GetMapNumber() != 0 {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("no demo"))
	}
	return connect.NewResponse(&hubv1.GetMatchDemoResponse{Url: "https://demos.example.com/r1/0?sig=x"}), nil
}

// A match result's page (ADR-0024): the score, the map, each team's players named as on a board,
// and the demo through a short-lived link.
func TestResultPage(t *testing.T) {
	r := newRig(t)
	resp, body := get(t, browser(t), r.srv.URL+"/results/r1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("result: %d", resp.StatusCode)
	}
	for _, want := range []string{"<h1>Alpha &lt;b&gt; 1–0 Bravo</h1>", "Alpha &lt;b&gt; won", "Map 1", "de_nuke", "13–9", `href="/results/r1/demo/0"`,
		`<a href="/members/u1">Jo</a>`, "Brave Falcon", ">2310<", ">Damage (HP)<", ">MVPs<",
		// Jo's 0 MVPs is a reported zero; Brave Falcon's row has none reported.
		`<td class="num">0</td>`, `<span class="visually-hidden">not reported</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if resp, _ := get(t, browser(t), r.srv.URL+"/results/r1/demo/0"); resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "https://demos.example.com/r1/0?sig=x" {
		t.Errorf("demo: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	for _, path := range []string{"/results/nope", "/results/r1/demo/1", "/results/r1/demo/x"} {
		if resp, _ := get(t, browser(t), r.srv.URL+path); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d", path, resp.StatusCode)
		}
	}
	r.stats.set(connect.CodePermissionDenied)
	if resp, _ := get(t, browser(t), r.srv.URL+"/results/r1"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("private: %d", resp.StatusCode)
	}
}
