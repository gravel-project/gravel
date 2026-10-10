package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/internal/web/templates"
)

// The pages for a community's game servers and their boards (gravel#18): the server list, a
// server's page and the leaderboards. Like every page they read the hub's own API in process
// (ADR-0005); what a reader may see is the API's to decide, and a page says so when it is refused.

// stateLabels are what a reader sees for the hub's state words.
var stateLabels = map[string]string{
	"ok":                 "Online",
	"unreachable":        "Offline",
	"unknown":            "Checking",
	"credential_refused": "Unavailable",
	"credential_missing": "Unavailable",
	"rate_limited":       "Unavailable",
	"error":              "Unavailable",
}

// metricLabels are a board column's headers.
var metricLabels = map[string]string{"kills": "Kills", "deaths": "Deaths", "kd": "K/D", "time": "Time on", "matches": "Matches"}

// defaultBoard is a board's columns when no game's layout says otherwise.
var defaultBoard = []string{"kills", "deaths", "kd", "time", "matches"}

// boardPageSize is how many players a page of a board shows.
const boardPageSize = 50

func serverRow(s *hubv1.Server, games map[string]*hubv1.Game) templates.ServerRow {
	st := s.GetStatus()
	label := stateLabels[st.GetState()]
	if label == "" {
		label = "Unavailable"
	}
	game := s.GetGameId()
	if g, ok := games[game]; ok {
		game = g.GetName()
	}
	return templates.ServerRow{ID: s.GetId(), Name: s.GetName(), Game: game, Location: s.GetLocation(), Official: s.GetTrust() == "official",
		State: st.GetState(), StateLabel: label, Players: int(st.GetPlayers()), MaxPlayers: int(st.GetMaxPlayers()), Map: st.GetMap()}
}

// gamesAndServers reads the enabled games (by id) and the servers.
func (h *Handler) gamesAndServers(r *http.Request) (map[string]*hubv1.Game, []*hubv1.Server, error) {
	ctx, _ := withBrowser(r)
	gs, err := h.serverAPI.ListGames(ctx, connect.NewRequest(&hubv1.ListGamesRequest{}))
	if err != nil {
		return nil, nil, fmt.Errorf("ListGames: %w", err)
	}
	games := map[string]*hubv1.Game{}
	for _, g := range gs.Msg.GetGames() {
		games[g.GetId()] = g
	}
	ss, err := h.serverAPI.ListServers(ctx, connect.NewRequest(&hubv1.ListServersRequest{}))
	if err != nil {
		return nil, nil, fmt.Errorf("ListServers: %w", err)
	}
	return games, ss.Msg.GetServers(), nil
}

func (h *Handler) servers(w http.ResponseWriter, r *http.Request) {
	p, b, err := h.page(r, "Servers")
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	games, list, err := h.gamesAndServers(r)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	sp := templates.ServersPage{Page: p.Page}
	for _, s := range list {
		sp.Servers = append(sp.Servers, serverRow(s, games))
	}
	h.render(w, r, http.StatusOK, b, templates.Servers(sp), templates.ServersContent(sp))
}

func (h *Handler) server(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx, _ := withBrowser(r)
	resp, err := h.serverAPI.GetServerStatus(ctx, connect.NewRequest(&hubv1.GetServerStatusRequest{ServerId: id}))
	switch connect.CodeOf(err) {
	case connect.CodeNotFound, connect.CodeInvalidArgument:
		h.fail(w, r, http.StatusNotFound, "No such server", "This hub has no server by that name.")
		return
	}
	if err != nil {
		h.serverError(w, r, fmt.Errorf("GetServerStatus: %w", err))
		return
	}
	games, _, err := h.gamesAndServers(r)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	s := resp.Msg.GetServer()
	p, b, err := h.page(r, s.GetName())
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	sp := templates.ServerPage{Page: p.Page, Server: serverRow(s, games), Build: s.GetStatus().GetBuild(),
		BoardURL: "/boards?scope=" + url.QueryEscape("server:"+s.GetId())}
	if t := s.GetStatus().GetObservedAt(); t != nil {
		at := t.AsTime()
		sp.ObservedAt = &at
	}
	for _, t := range s.GetStatus().GetTeams() {
		sp.Teams = append(sp.Teams, templates.Team{Name: t.GetName(), Score: t.GetScore()})
	}

	// Who is on: members and the owner may see it; the API refuses anyone else.
	players, err := h.serverAPI.ListServerPlayers(ctx, connect.NewRequest(&hubv1.ListServerPlayersRequest{ServerId: id}))
	switch connect.CodeOf(err) {
	case connect.CodeUnauthenticated:
		sp.PlayersNote = "Log in to see who's on."
	case connect.CodePermissionDenied:
		sp.PlayersNote = "Only members can see who's on."
	case connect.CodeFailedPrecondition, connect.CodeUnavailable:
		sp.PlayersNote = "The server isn't answering right now."
	default:
		if err != nil {
			h.serverError(w, r, fmt.Errorf("ListServerPlayers: %w", err))
			return
		}
		for _, pl := range players.Msg.GetPlayers() {
			sp.Players = append(sp.Players, templates.PlayerRow{Name: pl.GetName(), Subject: pl.GetSubject(), Team: pl.GetTeam(), Kills: int(pl.GetKills()),
				Deaths: int(pl.GetDeaths()), PingMs: int(pl.GetPingMs()), Member: pl.GetUserId() != ""})
		}
	}

	// Recent matches: public when the organization's stats are, else the owner's.
	matches, err := h.statsAPI.ListMatches(ctx, connect.NewRequest(&hubv1.ListMatchesRequest{ServerId: id, PageSize: 10}))
	switch connect.CodeOf(err) {
	case connect.CodeUnauthenticated, connect.CodePermissionDenied:
		sp.MatchesNote = "Match history is private on this hub."
	default:
		if err != nil {
			h.serverError(w, r, fmt.Errorf("ListMatches: %w", err))
			return
		}
		for _, m := range matches.Msg.GetMatches() {
			row := templates.MatchRow{Started: m.GetStartedAt().AsTime(), Map: m.GetMap(), Players: int(m.GetPlayers()), Kills: int(m.GetKills())}
			if m.GetEndedAt() != nil {
				t := m.GetEndedAt().AsTime()
				row.Ended = &t
			}
			sp.Matches = append(sp.Matches, row)
		}
	}
	if p.User.CanModerate() {
		sp.Mod = h.modControls(r, id, s.GetStatus().GetCapabilities())
	}
	h.render(w, r, http.StatusOK, b, templates.Server(sp), templates.ServerContent(sp))
}

// boardQuery is a board page's query string: scope ("", "game:<id>", "server:<id>"), window
// ("all", "week", "month", "season:<name>"), metric, and the page token.
type boardQuery struct {
	scope, window, metric, page string
}

func (q boardQuery) url(metric, page string) string {
	v := url.Values{}
	if q.scope != "" {
		v.Set("scope", q.scope)
	}
	if q.window != "" && q.window != "all" {
		v.Set("window", q.window)
	}
	if metric != "" {
		v.Set("metric", metric)
	}
	if page != "" {
		v.Set("page", page)
	}
	if len(v) == 0 {
		return "/boards"
	}
	return "/boards?" + v.Encode()
}

func (h *Handler) boards(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	q := boardQuery{scope: qs.Get("scope"), window: qs.Get("window"), metric: qs.Get("metric"), page: qs.Get("page")}
	if q.window == "" {
		q.window = "all"
	}
	games, list, err := h.gamesAndServers(r)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	p, b, err := h.page(r, "Leaderboards")
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	bp := templates.BoardsPage{Page: p.Page, Heading: "Leaderboards"}

	// The scope: everyone (Official servers), a game, or a server.
	req := &hubv1.GetBoardRequest{PageSize: boardPageSize, PageToken: q.page}
	columns := defaultBoard
	bp.Scopes = append(bp.Scopes, templates.Option{Value: "", Label: "Everyone (official servers)", Selected: q.scope == ""})
	gameIDs := make([]string, 0, len(games))
	for id := range games {
		gameIDs = append(gameIDs, id)
	}
	slices.Sort(gameIDs)
	for _, id := range gameIDs {
		v := "game:" + id
		bp.Scopes = append(bp.Scopes, templates.Option{Value: v, Label: games[id].GetName(), Selected: q.scope == v})
	}
	for _, s := range list {
		v := "server:" + s.GetId()
		bp.Scopes = append(bp.Scopes, templates.Option{Value: v, Label: s.GetName(), Selected: q.scope == v})
	}
	kind, id, _ := strings.Cut(q.scope, ":")
	switch kind {
	case "":
	case "game":
		g, ok := games[id]
		if !ok {
			h.fail(w, r, http.StatusNotFound, "No such board", "This hub has no game by that name.")
			return
		}
		req.GameId, bp.Heading = id, g.GetName()+" leaderboard"
		columns = layoutOf(g, columns)
	case "server":
		i := slices.IndexFunc(list, func(s *hubv1.Server) bool { return s.GetId() == id })
		if i < 0 {
			h.fail(w, r, http.StatusNotFound, "No such board", "This hub has no server by that name.")
			return
		}
		req.ServerId, bp.Heading = id, list[i].GetName()+" leaderboard"
		columns = layoutOf(games[list[i].GetGameId()], columns)
	default:
		h.fail(w, r, http.StatusNotFound, "No such board", "That is not a board this hub has.")
		return
	}

	// The window: all time, this week, this month, or a named season.
	bp.Windows = []templates.Option{
		{Value: "all", Label: "All time", Selected: q.window == "all"},
		{Value: "week", Label: "This week", Selected: q.window == "week"},
		{Value: "month", Label: "This month", Selected: q.window == "month"},
	}
	ctx, _ := withBrowser(r)
	loc := time.UTC // the window's dates are shown in the organization's timezone
	if set, err := h.orgAPI.GetOrganizationSettings(ctx, connect.NewRequest(&hubv1.GetOrganizationSettingsRequest{})); err == nil {
		if tz := set.Msg.GetSettings().GetStats().GetTimezone(); tz != "" {
			if l, err := time.LoadLocation(tz); err == nil {
				loc = l
			}
		}
		for _, se := range set.Msg.GetSettings().GetStats().GetSeasons() {
			v := "season:" + se.GetName()
			bp.Windows = append(bp.Windows, templates.Option{Value: v, Label: se.GetName(), Selected: q.window == v})
		}
	}
	if season, ok := strings.CutPrefix(q.window, "season:"); ok {
		req.Window, req.Season = "season", season
	} else {
		req.Window = q.window
	}

	metric := q.metric
	if !slices.Contains(columns, metric) {
		metric = columns[0]
	}
	req.Metric, bp.Metric = metric, metric
	for _, c := range columns {
		bp.Columns = append(bp.Columns, templates.BoardColumn{Metric: c, Label: metricLabels[c], Sorted: c == metric, URL: q.url(c, "")})
	}

	resp, err := h.statsAPI.GetBoard(ctx, connect.NewRequest(req))
	status := http.StatusOK
	switch connect.CodeOf(err) {
	case connect.CodeUnauthenticated, connect.CodePermissionDenied:
		bp.Note = "Leaderboards are private on this hub until its privacy policy covers stats."
	case connect.CodeInvalidArgument:
		status, bp.Note = http.StatusBadRequest, "That board can't be shown: "+connectMessage(err)
	default:
		if err != nil {
			h.serverError(w, r, fmt.Errorf("GetBoard: %w", err))
			return
		}
		m := resp.Msg
		if m.GetFrom() != nil {
			bp.Range = m.GetFrom().AsTime().In(loc).Format("Jan 2, 2006") + " to " + m.GetTo().AsTime().Add(-time.Second).In(loc).Format("Jan 2, 2006")
		}
		for _, e := range m.GetEntries() {
			row := templates.BoardRow{Rank: int(e.GetRank()), Name: e.GetName(), Pseudonymous: e.GetPseudonymous(), UserID: e.GetUserId()}
			for _, c := range columns {
				row.Cells = append(row.Cells, cell(e, c))
			}
			bp.Rows = append(bp.Rows, row)
		}
		if m.GetNextPageToken() != "" {
			bp.NextURL = q.url(q.metric, m.GetNextPageToken())
		}
	}
	h.render(w, r, status, b, templates.Boards(bp), templates.BoardsContent(bp))
}

// layoutOf is a game's board columns, or def when it has none.
func layoutOf(g *hubv1.Game, def []string) []string {
	var out []string
	for _, c := range g.GetLayout().GetBoard() {
		if _, ok := metricLabels[c]; ok {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

func cell(e *hubv1.BoardEntry, metric string) string {
	switch metric {
	case "kills":
		return strconv.Itoa(int(e.GetKills()))
	case "deaths":
		return strconv.Itoa(int(e.GetDeaths()))
	case "kd":
		return strconv.FormatFloat(e.GetKd(), 'f', 2, 64)
	case "time":
		return templates.Duration(int(e.GetSecondsOn()))
	case "matches":
		return strconv.Itoa(int(e.GetMatches()))
	}
	return ""
}

// connectMessage is an API error's message without its code.
func connectMessage(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Message()
	}
	return err.Error()
}
