package web_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/internal/org"
)

// GetSessionRecap answers with the fake's recap over the request's window, or recapErr.
func (f *fakeStats) GetSessionRecap(_ context.Context, req *connect.Request[hubv1.GetSessionRecapRequest]) (*connect.Response[hubv1.GetSessionRecapResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recapLast = req.Msg
	if f.recapErr != 0 {
		return nil, connect.NewError(f.recapErr, errors.New("a recap's window must have ended"))
	}
	resp := &hubv1.GetSessionRecapResponse{From: req.Msg.GetFrom(), To: req.Msg.GetTo(), Floor: req.Msg.GetFloor()}
	if f.recap != nil {
		resp.PeakPlayers, resp.Minutes, resp.MinutesAtFloor, resp.Players = f.recap.GetPeakPlayers(), f.recap.GetMinutes(), f.recap.GetMinutesAtFloor(), f.recap.GetPlayers()
		resp.Matches, resp.Top = f.recap.GetMatches(), f.recap.GetTop()
	}
	return connect.NewResponse(resp), nil
}

func (f *fakeStats) recapRequest() *hubv1.GetSessionRecapRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.recapLast
}

func (f *fakeStats) setRecap(err connect.Code, recap *hubv1.GetSessionRecapResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recapErr, f.recap = err, recap
}

// Wardogs Wednesday, 18:00 to 22:00 in Chicago.
var (
	recapFrom = time.Date(2026, 10, 7, 23, 0, 0, 0, time.UTC)
	recapTo   = time.Date(2026, 10, 8, 3, 0, 0, 0, time.UTC)
)

func recapURL(r *rig, server string, floor string) string {
	q := url.Values{"from": {recapFrom.Format(time.RFC3339)}, "to": {recapTo.Format(time.RFC3339)}}
	if floor != "" {
		q.Set("floor", floor)
	}
	return r.srv.URL + "/servers/" + server + "/recap?" + q.Encode()
}

func TestRecapPage(t *testing.T) {
	r := newRig(t)
	if _, err := r.org.UpdateSettings(context.Background(), org.Settings{Stats: org.Stats{Timezone: "America/Chicago"}}); err != nil {
		t.Fatal(err)
	}
	ended := timestamppb.New(time.Date(2026, 10, 7, 23, 44, 0, 0, time.UTC))
	r.stats.setRecap(0, &hubv1.GetSessionRecapResponse{PeakPlayers: 31, Minutes: 236, MinutesAtFloor: 174, Players: 44,
		Matches: []*hubv1.RecapMatch{
			{Id: "7", StartedAt: timestamppb.New(time.Date(2026, 10, 7, 23, 2, 0, 0, time.UTC)), EndedAt: ended, Map: "Ozeti", Players: 28,
				Teams: []*hubv1.TeamScore{{Name: "Lonestar", Color: "#ff0000", Score: 412}, {Name: "Boreal", Color: "#0000ff", Score: 388}}},
			{Id: "8", StartedAt: timestamppb.New(time.Date(2026, 10, 8, 2, 30, 0, 0, time.UTC)), Map: "Bakurani <i>", Players: 19},
		},
		Top: []*hubv1.BoardEntry{
			{Rank: 1, Name: "Brave Falcon <b>", Pseudonymous: true, Kills: 42, Deaths: 10, Kd: 4.2, SecondsOn: 7500, Matches: 2},
			{Rank: 2, Name: "Jo", UserId: "u1", Kills: 30, Deaths: 12, Kd: 2.5, SecondsOn: 2700, Matches: 1},
		}})

	// The numbers, the matches and the top five, in the organization's zone and the game's overlay.
	resp, body := get(t, browser(t), recapURL(r, "htg-wardogs-1", "20"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("recap: %d %s", resp.StatusCode, body)
	}
	if req := r.stats.recapRequest(); req.GetServerId() != "htg-wardogs-1" || !req.GetFrom().AsTime().Equal(recapFrom) || !req.GetTo().AsTime().Equal(recapTo) || req.GetFloor() != 20 {
		t.Errorf("request = %v", req)
	}
	for _, want := range []string{"<h1>HTG WARDOGS | NA WEST | #1</h1>", "Wed Oct 7, 2026, 18:00 to 22:00 CDT", `data-game="wardogs"`,
		"<dt>Peak players</dt><dd>31</dd>", "<dt>Minutes with anyone on</dt><dd>236</dd>", "<dt>Minutes at 20 or more</dt><dd>174</dd>", "<dt>Players</dt><dd>44</dd>",
		"<caption>Matches in this window</caption>", "18:02–18:44", "Ozeti", `<span class="team">Lonestar</span> <strong>412</strong>`,
		`<span class="team">Boreal</span> <strong>388</strong>`, "21:30", `<span class="badge">live</span>`, "Bakurani &lt;i&gt;",
		"<caption>The most kills in these matches</caption>", `aria-sort="descending"`, "Brave Falcon &lt;b&gt;", `<a href="/members/u1">Jo</a>`,
		">42<", "4.20", "2h 05m", "45m", "appear under a pseudonym", `href="/servers/htg-wardogs-1"`, `href="/boards?scope=server%3Ahtg-wardogs-1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if strings.Contains(body, "Brave Falcon <b>") || strings.Contains(body, "#ff0000") || strings.Contains(body, "style=") {
		t.Error("a name was not escaped, or a team's colour reached the page")
	}
	// A pseudonym is marked as one: no member badge on its row, one on the member's.
	falcon := body[strings.Index(body, "Brave Falcon"):]
	falcon = falcon[:strings.Index(falcon, "</tr>")]
	jo := body[strings.Index(body, `<a href="/members/u1">Jo</a>`):]
	jo = jo[:strings.Index(jo, "</tr>")]
	if strings.Contains(falcon, ">member<") || !strings.Contains(jo, ">member<") {
		t.Errorf("pseudonyms are not marked as the boards mark them:\n%s\n%s", falcon, jo)
	}

	// No floor asked for: no minutes at it. As htmx asks for it: the content only.
	resp, body = get(t, browser(t), recapURL(r, "htg-wardogs-1", ""), "HX-Request", "true")
	if resp.StatusCode != http.StatusOK || strings.Contains(body, "<html") || strings.Contains(body, "Minutes at") || !strings.Contains(body, "<dd>31</dd>") {
		t.Errorf("no floor, htmx: %d %s", resp.StatusCode, body)
	}
	if req := r.stats.recapRequest(); req.GetFloor() != 0 {
		t.Errorf("no floor: request = %v", req)
	}

	// Nobody came: the message and nothing else.
	r.stats.setRecap(0, nil)
	if resp, body := get(t, browser(t), recapURL(r, "htg-wardogs-1", "20")); resp.StatusCode != http.StatusOK || !strings.Contains(body, "Nobody was on during this window.") ||
		strings.Contains(body, "Peak players") || strings.Contains(body, "<table") {
		t.Errorf("nobody came: %d %s", resp.StatusCode, body)
	}

	// Private stats read as the boards do: words on the page, no numbers.
	for _, code := range []connect.Code{connect.CodePermissionDenied, connect.CodeUnauthenticated} {
		r.stats.setRecap(code, nil)
		if resp, body := get(t, browser(t), recapURL(r, "htg-wardogs-1", "20")); resp.StatusCode != http.StatusOK || !strings.Contains(body, "Recaps are private on this hub") ||
			strings.Contains(body, "Peak players") || strings.Contains(body, "Nobody was on") || !strings.Contains(body, "<h1>HTG WARDOGS | NA WEST | #1</h1>") {
			t.Errorf("%v: %d %s", code, resp.StatusCode, body)
		}
	}

	// A window the API refuses is a 400 that says so.
	r.stats.setRecap(connect.CodeInvalidArgument, nil)
	if resp, body := get(t, browser(t), recapURL(r, "htg-wardogs-1", "20")); resp.StatusCode != http.StatusBadRequest ||
		!strings.Contains(body, "This link is not a recap window: a recap&#39;s window must have ended.") {
		t.Errorf("a refused window: %d %s", resp.StatusCode, body)
	}
	r.stats.setRecap(connect.CodeInternal, nil)
	if resp, _ := get(t, browser(t), recapURL(r, "htg-wardogs-1", "20")); resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("an API failure: %d", resp.StatusCode)
	}

	// A link the page can't even ask with: a 400 before the API is called.
	r.stats.setRecap(0, nil)
	for name, u := range map[string]string{
		"from":  r.srv.URL + "/servers/htg-wardogs-1/recap?from=yesterday&to=" + url.QueryEscape(recapTo.Format(time.RFC3339)),
		"floor": recapURL(r, "htg-wardogs-1", "twenty"),
	} {
		before := r.stats.recapRequest()
		if resp, body := get(t, browser(t), u); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "This link is not a recap window") {
			t.Errorf("a bad %s: %d %s", name, resp.StatusCode, body)
		}
		if r.stats.recapRequest() != before {
			t.Errorf("a bad %s reached the API", name)
		}
	}

	// A server the hub doesn't have is no recap.
	if resp, body := get(t, browser(t), recapURL(r, "nope", "20")); resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "No such server") {
		t.Errorf("an unknown server: %d %s", resp.StatusCode, body)
	}
}

// A window across midnight, or in UTC when the organization names no zone, shows the end's date.
func TestRecapWindowAcrossMidnight(t *testing.T) {
	r := newRig(t)
	resp, body := get(t, browser(t), recapURL(r, "htg-wardogs-1", ""))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Wed Oct 7, 2026, 23:00 to Thu Oct 8, 2026, 03:00 UTC") {
		t.Errorf("UTC window: %d %s", resp.StatusCode, body)
	}
}
