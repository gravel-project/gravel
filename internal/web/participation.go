package web

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"

	"connectrpc.com/connect"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/internal/web/templates"
)

// The participation boards (ADR-0014): who took part, and for how long, in what the community's
// first-party apps record (seeding a server, attending an event). Like the leaderboards they read
// the hub's own API in process, and the API decides who may see them.

// participationQuery is a participation page's query string: kind (empty: the first recorded),
// window ("all", "week", "month", "season:<name>") and the page token.
type participationQuery struct {
	kind, window, page string
}

func (q participationQuery) url(page string) string {
	v := url.Values{}
	if q.kind != "" {
		v.Set("kind", q.kind)
	}
	if q.window != "" && q.window != "all" {
		v.Set("window", q.window)
	}
	if page != "" {
		v.Set("page", page)
	}
	if len(v) == 0 {
		return "/participation"
	}
	return "/participation?" + v.Encode()
}

func (h *Handler) participation(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	q := participationQuery{kind: qs.Get("kind"), window: qs.Get("window"), page: qs.Get("page")}
	if q.window == "" {
		q.window = "all"
	}
	p, b, err := h.page(r, "Participation")
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	pp := templates.ParticipationPage{Page: p.Page}
	ctx, _ := withBrowser(r)
	windows, loc := h.windowOptions(ctx, q.window)
	pp.Windows = windows
	req := &hubv1.GetParticipationBoardRequest{Kind: q.kind, PageSize: boardPageSize, PageToken: q.page}
	req.Window, req.Season = windowOf(q.window)

	resp, err := h.statsAPI.GetParticipationBoard(ctx, connect.NewRequest(req))
	status := http.StatusOK
	switch connect.CodeOf(err) {
	case connect.CodeUnauthenticated, connect.CodePermissionDenied:
		pp.Note = "Participation boards are private on this hub until its privacy policy covers stats."
	case connect.CodeInvalidArgument:
		status, pp.Note = http.StatusBadRequest, "That board can't be shown: "+connectMessage(err)
	default:
		if err != nil {
			h.serverError(w, r, fmt.Errorf("GetParticipationBoard: %w", err))
			return
		}
		m := resp.Msg
		if len(m.GetKinds()) == 0 {
			pp.Empty = true
			break
		}
		if !slices.Contains(m.GetKinds(), m.GetKind()) {
			h.fail(w, r, http.StatusNotFound, "No such board", "Nothing of that kind has been recorded on this hub.")
			return
		}
		for _, k := range m.GetKinds() {
			pp.Kinds = append(pp.Kinds, templates.Option{Value: k, Label: templates.KindLabel(k), Selected: k == m.GetKind()})
		}
		pp.Kind = templates.KindLabel(m.GetKind())
		pp.Range = rangeOf(m.GetFrom(), m.GetTo(), loc)
		for _, e := range m.GetEntries() {
			pp.Rows = append(pp.Rows, templates.ParticipationRow{Rank: int(e.GetRank()), Name: e.GetName(), Pseudonymous: e.GetPseudonymous(),
				UserID: e.GetUserId(), Time: templates.Duration(int(e.GetSeconds())), Times: int(e.GetEvents())})
		}
		if m.GetNextPageToken() != "" {
			q.kind = m.GetKind()
			pp.NextURL = q.url(m.GetNextPageToken())
		}
	}
	h.render(w, r, status, b, templates.Participation(pp), templates.ParticipationContent(pp))
}

// profileParticipation turns a profile's participation into a table per kind, a row per window
// in the profile's order, zero where the member did not take part in that window.
func profileParticipation(windows []*hubv1.ProfileTotals, parts []*hubv1.ParticipationTotals) []templates.KindTable {
	var kinds []string
	for _, x := range parts {
		if !slices.Contains(kinds, x.GetKind()) {
			kinds = append(kinds, x.GetKind())
		}
	}
	slices.Sort(kinds)
	out := make([]templates.KindTable, 0, len(kinds))
	for _, k := range kinds {
		t := templates.KindTable{Label: templates.KindLabel(k)}
		for _, w := range windows {
			row := templates.KindTotalsRow{Label: windowLabel(w.GetWindow(), w.GetSeason()), Time: templates.Duration(0)}
			for _, x := range parts {
				if x.GetKind() == k && x.GetWindow() == w.GetWindow() && x.GetSeason() == w.GetSeason() {
					row.Times, row.Time = int(x.GetEvents()), templates.Duration(int(x.GetSeconds()))
				}
			}
			t.Rows = append(t.Rows, row)
		}
		out = append(out, t)
	}
	return out
}

// windowLabel names a profile's window: "All time", "This week", "This month", or the season.
func windowLabel(window, season string) string {
	if window == "season" {
		return season
	}
	return windowLabels[window]
}
