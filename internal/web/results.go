package web

import (
	"fmt"
	"net/http"
	"strconv"

	"connectrpc.com/connect"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/internal/web/templates"
)

// A match result (ADR-0024): a series a server's match plugin reported, its maps with their demos,
// and each player's totals on each map, named as on a board.

func (h *Handler) result(w http.ResponseWriter, r *http.Request) {
	ctx, _ := withBrowser(r)
	resp, err := h.statsAPI.GetMatchResult(ctx, connect.NewRequest(&hubv1.GetMatchResultRequest{Id: r.PathValue("id")}))
	switch connect.CodeOf(err) {
	case connect.CodeNotFound, connect.CodeInvalidArgument, connect.CodeUnimplemented:
		h.fail(w, r, http.StatusNotFound, "No such result", "This hub has no match result by that id.")
		return
	case connect.CodeUnauthenticated, connect.CodePermissionDenied:
		h.fail(w, r, http.StatusForbidden, "Results are private", "Match results are private on this hub until its privacy policy covers stats.")
		return
	}
	if err != nil {
		h.serverError(w, r, fmt.Errorf("GetMatchResult: %w", err))
		return
	}
	m := resp.Msg.GetResult()
	server := m.GetServerId()
	game := ""
	if s, err := h.serverAPI.GetServerStatus(ctx, connect.NewRequest(&hubv1.GetServerStatusRequest{ServerId: m.GetServerId()})); err == nil {
		server, game = s.Msg.GetServer().GetName(), s.Msg.GetServer().GetGameId()
	}
	p, b, err := h.page(r, m.GetTeam1Name()+" vs "+m.GetTeam2Name())
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	withGame(&p.Page, game, h.gameName(r, game))
	loc := h.orgZone(ctx)
	rp := templates.ResultPage{Page: p.Page, ID: m.GetId(), ServerID: m.GetServerId(), ServerName: server, Team1: name(m.GetTeam1Name(), "Team 1"),
		Team2: name(m.GetTeam2Name(), "Team 2"), Score1: int(m.GetTeam1Score()), Score2: int(m.GetTeam2Score()), Winner: m.GetWinner(),
		Started: m.GetStartedAt().AsTime().In(loc).Format("Jan 2, 15:04 MST"), Ended: m.GetEndedAt() != nil}
	for _, mp := range m.GetMaps() {
		row := templates.ResultMap{Number: int(mp.GetMapNumber()) + 1, Name: mp.GetMapName(), Score1: int(mp.GetTeam1Score()), Score2: int(mp.GetTeam2Score()),
			Winner: mp.GetWinner()}
		if mp.GetHasDemo() {
			row.DemoURL = fmt.Sprintf("/results/%s/demo/%d", m.GetId(), mp.GetMapNumber())
		}
		for _, pl := range m.GetPlayers() {
			if pl.GetMapNumber() != mp.GetMapNumber() {
				continue
			}
			pr := templates.ResultPlayer{Name: pl.GetName(), Pseudonymous: pl.GetPseudonymous(), UserID: pl.GetUserId(), Kills: int(pl.GetKills()),
				Deaths: int(pl.GetDeaths()), Assists: int(pl.GetAssists()), Damage: int(pl.GetDamage()), MVPs: int(pl.GetMvps())}
			if pl.GetTeam() == "team2" {
				row.Players2 = append(row.Players2, pr)
			} else {
				row.Players1 = append(row.Players1, pr)
			}
		}
		rp.Maps = append(rp.Maps, row)
	}
	h.render(w, r, http.StatusOK, b, templates.Result(rp), templates.ResultContent(rp))
}

func name(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// resultDemo sends the browser to a short-lived link to a map's demo.
func (h *Handler) resultDemo(w http.ResponseWriter, r *http.Request) {
	ctx, b := withBrowser(r)
	mapNumber, err := strconv.ParseInt(r.PathValue("map"), 10, 32)
	if err != nil || mapNumber < 0 {
		h.fail(w, r, http.StatusNotFound, "No such demo", "That map has no demo.")
		return
	}
	resp, err := h.statsAPI.GetMatchDemo(ctx, connect.NewRequest(&hubv1.GetMatchDemoRequest{Id: r.PathValue("id"), MapNumber: int32(mapNumber)}))
	switch connect.CodeOf(err) {
	case connect.CodeNotFound, connect.CodeInvalidArgument, connect.CodeUnimplemented:
		h.fail(w, r, http.StatusNotFound, "No such demo", "That map has no demo.")
		return
	case connect.CodeUnauthenticated, connect.CodePermissionDenied:
		h.fail(w, r, http.StatusForbidden, "Demos are private", "Match results are private on this hub until its privacy policy covers stats.")
		return
	}
	if err != nil {
		h.apiError(w, r, b, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, resp.Msg.GetUrl(), http.StatusFound)
}
