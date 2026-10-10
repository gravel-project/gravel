package web

import (
	"fmt"
	"net/http"
	"strconv"

	"connectrpc.com/connect"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/internal/session"
	"github.com/gravel-project/gravel/internal/web/templates"
)

// The member profile (gravel#18): a member's stats across their linked accounts, and their switch
// for being shown by name. The API decides who may read it; a refusal is "no such profile", so a
// page never says who is behind a pseudonym.

// windowLabels name a profile's windows.
var windowLabels = map[string]string{"all": "All time", "week": "This week", "month": "This month"}

// profile sends a logged-in member to their own profile.
func (h *Handler) profile(w http.ResponseWriter, r *http.Request) {
	s, ok := session.FromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/members/"+s.UserID.String(), http.StatusSeeOther)
}

func (h *Handler) member(w http.ResponseWriter, r *http.Request) {
	mp, b, status, err := h.memberPage(r, r.PathValue("id"), nil)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	if status == http.StatusNotFound {
		h.fail(w, r, http.StatusNotFound, "No such profile", "There is no profile here, or it is private.")
		return
	}
	h.render(w, r, http.StatusOK, b, templates.Member(mp), templates.MemberContent(mp))
}

// memberPage builds a profile from the API; status is 404 when the API refuses or knows no such
// member.
func (h *Handler) memberPage(r *http.Request, id string, flash *templates.Flash) (templates.MemberPage, *browser, int, error) {
	ctx, _ := withBrowser(r)
	resp, err := h.statsAPI.GetMemberProfile(ctx, connect.NewRequest(&hubv1.GetMemberProfileRequest{UserId: id}))
	switch connect.CodeOf(err) {
	case connect.CodeNotFound, connect.CodeInvalidArgument, connect.CodeUnauthenticated, connect.CodePermissionDenied:
		return templates.MemberPage{}, nil, http.StatusNotFound, nil
	}
	if err != nil {
		return templates.MemberPage{}, nil, 0, fmt.Errorf("GetMemberProfile: %w", err)
	}
	m := resp.Msg
	p, b, err := h.page(r, m.GetDisplayName())
	if err != nil {
		return templates.MemberPage{}, b, 0, err
	}
	if flash != nil {
		p.Flash = flash
	}
	mp := templates.MemberPage{Page: p.Page, UserID: m.GetUserId(), DisplayName: m.GetDisplayName(), Self: m.GetSelf(), ShowName: m.GetShowName()}
	for _, t := range m.GetTotals() {
		label := windowLabels[t.GetWindow()]
		if t.GetWindow() == "season" {
			label = t.GetSeason()
		}
		mp.Totals = append(mp.Totals, templates.TotalsRow{Label: label, Kills: int(t.GetKills()), Deaths: int(t.GetDeaths()),
			KD: strconv.FormatFloat(t.GetKd(), 'f', 2, 64), Time: templates.Duration(int(t.GetSecondsOn())), Matches: int(t.GetMatches())})
	}
	for _, pm := range m.GetRecent() {
		mp.Recent = append(mp.Recent, templates.PlayedMatch{Started: pm.GetStartedAt().AsTime(), Live: pm.GetEndedAt() == nil, ServerID: pm.GetServerId(),
			Server: pm.GetServerName(), Map: pm.GetMap(), Kills: int(pm.GetKills()), Deaths: int(pm.GetDeaths()), Time: templates.Duration(int(pm.GetSecondsOn()))})
	}
	return mp, b, http.StatusOK, nil
}

// setName is the profile's switch: show the member's name on boards, or their pseudonym.
func (h *Handler) setName(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	ctx, b := withBrowser(r)
	show := r.PostFormValue("show") == "1"
	if _, err := h.statsAPI.SetBoardName(ctx, connect.NewRequest(&hubv1.SetBoardNameRequest{ShowName: show})); err != nil {
		h.apiError(w, r, b, err)
		return
	}
	flash := templates.Flash{Kind: "ok", Text: "Leaderboards now show you under your pseudonym."}
	if show {
		flash.Text = "Leaderboards now show your name."
	}
	if !isHX(r) {
		b.relay(w)
		h.setFlash(w, flash.Kind, flash.Text)
		http.Redirect(w, r, "/profile", http.StatusSeeOther)
		return
	}
	mp, b2, status, err := h.memberPage(r, "", &flash)
	if err != nil || status != http.StatusOK {
		h.serverError(w, r, fmt.Errorf("profile after the name switch (status %d): %w", status, err))
		return
	}
	b.relay(w)
	h.render(w, r, http.StatusOK, b2, templates.Member(mp), templates.MemberContent(mp))
}
