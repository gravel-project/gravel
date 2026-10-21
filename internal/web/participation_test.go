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
	"github.com/gravel-project/gravel/internal/web/templates"
)

// GetParticipationBoard answers with the fake's board: the request's kind, else the first.
func (f *fakeStats) GetParticipationBoard(_ context.Context, req *connect.Request[hubv1.GetParticipationBoardRequest]) (*connect.Response[hubv1.GetParticipationBoardResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.partLast = req.Msg
	f.partAll = append(f.partAll, req.Msg)
	if f.partErr != 0 {
		return nil, connect.NewError(f.partErr, errors.New("no season named \"Nope\""))
	}
	resp := &hubv1.GetParticipationBoardResponse{Kinds: f.partKinds, Entries: f.partEntries, NextPageToken: f.partNext, Kind: req.Msg.GetKind()}
	if resp.Kind == "" && len(f.partKinds) > 0 {
		resp.Kind = f.partKinds[0]
	}
	if req.Msg.GetWindow() == "week" {
		resp.From = timestamppb.New(time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC))
		resp.To = timestamppb.New(time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC))
	}
	return connect.NewResponse(resp), nil
}

func (f *fakeStats) partRequest() *hubv1.GetParticipationBoardRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.partLast
}

func (f *fakeStats) setPart(err connect.Code) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.partErr = err
}

func TestKindLabel(t *testing.T) {
	for in, want := range map[string]string{"seeding": "Seeding", "event_attendance": "Event attendance", "a": "A", "a_b_c9": "A b c9", "": ""} {
		if got := templates.KindLabel(in); got != want {
			t.Errorf("KindLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParticipationPage(t *testing.T) {
	r := newRig(t)

	// Nothing recorded yet: the empty state, no picker.
	resp, body := get(t, browser(t), r.srv.URL+"/participation")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Nothing has been recorded yet") || strings.Contains(body, `id="kind"`) {
		t.Errorf("empty: %d %s", resp.StatusCode, body)
	}
	if req := r.stats.partRequest(); req.GetKind() != "" || req.GetWindow() != "all" || req.GetPageSize() != 50 {
		t.Errorf("request = %v", req)
	}

	r.stats.mu.Lock()
	r.stats.partKinds = []string{"attendance", "event_attendance", "seeding"}
	r.stats.partEntries = []*hubv1.ParticipationEntry{
		{Rank: 1, Name: "Brave Falcon <b>", Pseudonymous: true, Events: 3, Seconds: 5400},
		{Rank: 2, Name: "Jo", UserId: "u1", Events: 1, Seconds: 600},
	}
	r.stats.partNext = "cGFnZTI"
	r.stats.mu.Unlock()

	// The first kind by default: the picker labels every kind, the rows rank by time.
	resp, body = get(t, browser(t), r.srv.URL+"/participation")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("participation: %d", resp.StatusCode)
	}
	for _, want := range []string{`<option value="attendance" selected>Attendance</option>`, `<option value="event_attendance">Event attendance</option>`,
		`<option value="seeding">Seeding</option>`, "<caption>Attendance</caption>", "Brave Falcon &lt;b&gt;", "1h 30m", "10m", `aria-sort="descending"`,
		`<a href="/members/u1">Jo</a>`, ">member<", "appear under a pseudonym", "page=cGFnZTI", "kind=attendance", `href="/boards"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if strings.Contains(body, "Brave Falcon <b>") || strings.Index(body, "Brave Falcon") > strings.Index(body, ">Jo<") {
		t.Error("a name was not escaped, or the rows are out of order")
	}

	// A kind and a window, as htmx asks for them: the content only.
	q := url.Values{"kind": {"seeding"}, "window": {"week"}}
	resp, body = get(t, browser(t), r.srv.URL+"/participation?"+q.Encode(), "HX-Request", "true")
	if req := r.stats.partRequest(); req.GetKind() != "seeding" || req.GetWindow() != "week" || req.GetSeason() != "" {
		t.Errorf("request = %v", req)
	}
	if resp.StatusCode != http.StatusOK || strings.Contains(body, "<html") || !strings.Contains(body, "<caption>Seeding</caption>") ||
		!strings.Contains(body, `<option value="week" selected>`) || !strings.Contains(body, "Oct 5, 2026 to Oct 11, 2026") || !strings.Contains(body, `hx-get="/participation"`) {
		t.Errorf("htmx fragment: %d %s", resp.StatusCode, body)
	}
	get(t, browser(t), r.srv.URL+"/participation?"+url.Values{"window": {"season:Season 02"}}.Encode())
	if req := r.stats.partRequest(); req.GetWindow() != "season" || req.GetSeason() != "Season 02" {
		t.Errorf("a season: %v", req)
	}
	get(t, browser(t), r.srv.URL+"/participation?page=cGFnZTI")
	if req := r.stats.partRequest(); req.GetPageToken() != "cGFnZTI" {
		t.Errorf("the next page: %v", req)
	}

	// A kind nobody recorded is no board.
	if resp, body := get(t, browser(t), r.srv.URL+"/participation?kind=streaming"); resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "Nothing of that kind") {
		t.Errorf("an unrecorded kind: %d", resp.StatusCode)
	}

	// Refusals become words on the page.
	for _, code := range []connect.Code{connect.CodePermissionDenied, connect.CodeUnauthenticated} {
		r.stats.setPart(code)
		if resp, body := get(t, browser(t), r.srv.URL+"/participation"); resp.StatusCode != http.StatusOK || !strings.Contains(body, "Participation boards are private") ||
			strings.Contains(body, "Brave Falcon") || strings.Contains(body, "Nothing has been recorded") {
			t.Errorf("%v: %d %s", code, resp.StatusCode, body)
		}
	}
	r.stats.setPart(connect.CodeInvalidArgument)
	if resp, body := get(t, browser(t), r.srv.URL+"/participation?window=season:Nope"); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "That board can&#39;t be shown: no season named") {
		t.Errorf("a bad season: %d %s", resp.StatusCode, body)
	}
	r.stats.setPart(connect.CodeInternal)
	if resp, _ := get(t, browser(t), r.srv.URL+"/participation"); resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("an API failure: %d", resp.StatusCode)
	}

	// The leaderboards link here.
	if _, body := get(t, browser(t), r.srv.URL+"/boards"); !strings.Contains(body, `<a href="/participation">Participation boards</a>`) {
		t.Error("the boards page does not link to the participation boards")
	}
}

// A profile shows a table per kind, a row per window, zero where the member did not take part.
func TestProfileParticipationSection(t *testing.T) {
	r := newRig(t)
	resp, body := get(t, browser(t), r.srv.URL+"/members/u2")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `id="participation-heading"`) || !strings.Contains(body, "Nothing recorded yet") {
		t.Errorf("no participation: %d %s", resp.StatusCode, body)
	}
	r.stats.mu.Lock()
	r.stats.participation = []*hubv1.ParticipationTotals{
		{Window: "all", Kind: "seeding", Events: 3, Seconds: 5400},
		{Window: "all", Kind: "event_attendance", Events: 2, Seconds: 7200},
		{Window: "season", Season: "Season 02", Kind: "seeding", Events: 1, Seconds: 600},
	}
	r.stats.mu.Unlock()
	_, body = get(t, browser(t), r.srv.URL+"/members/u2")
	section := body[strings.Index(body, `id="participation-heading"`):strings.Index(body, `id="recent-heading"`)]
	for _, want := range []string{"<caption>Event attendance</caption>", "<caption>Seeding</caption>", `href="/participation"`} {
		if !strings.Contains(section, want) {
			t.Errorf("the section lacks %q: %s", want, section)
		}
	}
	if strings.Index(section, "Event attendance") > strings.Index(section, "Seeding") {
		t.Error("the kinds are not sorted")
	}
	seedingTable := section[strings.Index(section, "<caption>Seeding</caption>"):]
	for _, row := range [][]string{{"All time", "3", "1h 30m"}, {"This week", "0", "0m"}, {"Season 02", "1", "10m"}} {
		want := `<th scope="row">` + row[0] + `</th><td class="num">` + row[1] + `</td><td class="num">` + row[2] + `</td>`
		if !strings.Contains(seedingTable, want) {
			t.Errorf("the seeding table lacks %s: %s", want, seedingTable)
		}
	}
}
