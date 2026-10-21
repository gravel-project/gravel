package web

import (
	"cmp"
	"fmt"
	"net/http"
	"net/url"
	"slices"

	"connectrpc.com/connect"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/internal/web/templates"
)

// The games (gravel#2): a list of the games the hub runs, and a page for each with its servers,
// the top of its leaderboard and the top of each activity recorded for it. A server-less game's
// page is its participation alone. Like every page they read the hub's own API in process.

// gameTop is how many players the game page shows of each board.
const gameTop = 10

// sortedGameIDs are the games' ids, sorted.
func sortedGameIDs(games map[string]*hubv1.Game) []string {
	ids := make([]string, 0, len(games))
	for id := range games {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (h *Handler) games(w http.ResponseWriter, r *http.Request) {
	games, list, err := h.gamesAndServers(r)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	p, b, err := h.page(r, "Games")
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	gp := templates.GamesPage{Page: p.Page}
	count := map[string]int{}
	for _, s := range list {
		count[s.GetGameId()]++
	}
	for _, g := range games {
		row := templates.GameRow{ID: g.GetId(), Name: g.GetName()}
		switch n := count[g.GetId()]; {
		case g.GetServerLess():
			row.Summary = "No game servers: its community's participation"
		case n == 1:
			row.Summary = "1 server"
		default:
			row.Summary = fmt.Sprintf("%d servers", n)
		}
		gp.Games = append(gp.Games, row)
	}
	slices.SortFunc(gp.Games, func(a, b templates.GameRow) int { return cmp.Compare(a.Name, b.Name) })
	h.render(w, r, http.StatusOK, b, templates.Games(gp), templates.GamesContent(gp))
}

func (h *Handler) game(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	games, list, err := h.gamesAndServers(r)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	g, ok := games[id]
	if !ok {
		h.fail(w, r, http.StatusNotFound, "No such game", "This hub runs no game by that name.")
		return
	}
	p, b, err := h.page(r, g.GetName())
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	gp := templates.GamePage{Page: p.Page, Name: g.GetName(), ServerLess: g.GetServerLess()}
	withGame(&gp.Page, id, g.GetName())
	for _, s := range list {
		if s.GetGameId() == id {
			gp.Servers = append(gp.Servers, serverRow(s, games))
		}
	}
	if g.GetCombatBoards() {
		if gp.Board, err = h.gameBoard(r, g); err != nil {
			h.serverError(w, r, err)
			return
		}
		gp.Ranked = len(gp.Board.Rows) > 0
	}
	if err := h.gameActivities(r, id, &gp); err != nil {
		h.serverError(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, b, templates.GameHome(gp), templates.GameHomeContent(gp))
}

// gameBoard is the top of a game's leaderboard, all time, by the first of its columns.
func (h *Handler) gameBoard(r *http.Request, g *hubv1.Game) (*templates.GameBoard, error) {
	ctx, _ := withBrowser(r)
	columns := layoutOf(g, defaultBoard)
	out := &templates.GameBoard{URL: "/boards?" + url.Values{"scope": {"game:" + g.GetId()}}.Encode()}
	for i, c := range columns {
		out.Columns = append(out.Columns, templates.BoardColumn{Metric: c, Label: metricLabels[c], Sorted: i == 0})
	}
	resp, err := h.statsAPI.GetBoard(ctx, connect.NewRequest(&hubv1.GetBoardRequest{GameId: g.GetId(), Metric: columns[0], PageSize: gameTop}))
	switch connect.CodeOf(err) {
	case connect.CodeUnauthenticated, connect.CodePermissionDenied:
		out.Note = "Leaderboards are private on this hub until its privacy policy covers stats."
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetBoard: %w", err)
	}
	for _, e := range resp.Msg.GetEntries() {
		row := templates.BoardRow{Rank: int(e.GetRank()), Name: e.GetName(), Pseudonymous: e.GetPseudonymous(), UserID: e.GetUserId()}
		for _, c := range columns {
			row.Cells = append(row.Cells, cell(e, c))
		}
		out.Rows = append(out.Rows, row)
	}
	return out, nil
}

// gameActivities fills the top of each participation kind recorded for a game, all time: the
// first board's answer names the kinds, and each other kind is read in turn.
func (h *Handler) gameActivities(r *http.Request, game string, gp *templates.GamePage) error {
	ctx, _ := withBrowser(r)
	read := func(kind string) (*hubv1.GetParticipationBoardResponse, error) {
		resp, err := h.statsAPI.GetParticipationBoard(ctx, connect.NewRequest(&hubv1.GetParticipationBoardRequest{Kind: kind, GameId: game, PageSize: gameTop}))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}
	first, err := read("")
	switch connect.CodeOf(err) {
	case connect.CodeUnauthenticated, connect.CodePermissionDenied:
		gp.ParticipationNote = "Participation boards are private on this hub until its privacy policy covers stats."
		return nil
	}
	if err != nil {
		return fmt.Errorf("GetParticipationBoard: %w", err)
	}
	for _, kind := range first.GetKinds() {
		m := first
		if kind != first.GetKind() {
			if m, err = read(kind); err != nil {
				return fmt.Errorf("GetParticipationBoard %s: %w", kind, err)
			}
		}
		a := templates.Activity{Label: templates.KindLabel(kind),
			URL: (participationQuery{game: game, kind: kind}).url("")}
		for _, e := range m.GetEntries() {
			a.Rows = append(a.Rows, templates.ParticipationRow{Rank: int(e.GetRank()), Name: e.GetName(), Pseudonymous: e.GetPseudonymous(),
				UserID: e.GetUserId(), Time: templates.Duration(int(e.GetSeconds())), Times: int(e.GetEvents())})
		}
		gp.Ranked = gp.Ranked || len(a.Rows) > 0
		gp.Activities = append(gp.Activities, a)
	}
	return nil
}
