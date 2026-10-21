package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

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
	"parked":             "Parked",
	"provisioning":       "Starting up",
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

// gameName is a game's display name through ListGames, or its id when the hub doesn't list it.
func (h *Handler) gameName(r *http.Request, id string) string {
	ctx, _ := withBrowser(r)
	gs, err := h.serverAPI.ListGames(ctx, connect.NewRequest(&hubv1.ListGamesRequest{}))
	if err == nil {
		for _, g := range gs.Msg.GetGames() {
			if g.GetId() == id {
				return g.GetName()
			}
		}
	}
	return id
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
	row := serverRow(s, games)
	withGame(&p.Page, s.GetGameId(), row.Game)
	sp := templates.ServerPage{Page: p.Page, Server: row, Build: s.GetStatus().GetBuild(), NextMap: s.GetStatus().GetNextRotationMap(),
		BoardURL: "/boards?scope=" + url.QueryEscape("server:"+s.GetId())}
	if t := s.GetStatus().GetObservedAt(); t != nil {
		at := t.AsTime()
		sp.ObservedAt = &at
	}
	if j := s.GetStatus().GetJoin(); j.GetValue() != "" {
		sp.Join = &templates.Join{Label: templates.JoinLabel(j.GetMethod()), Value: j.GetValue(), Hint: j.GetHint()}
	}
	for _, t := range s.GetStatus().GetTeams() {
		sp.Teams = append(sp.Teams, templates.Team{Name: t.GetName(), Score: t.GetScore(), Players: t.Players})
		sp.Scored = sp.Scored || t.GetScore() > 0
	}

	// Who is on: members and the owner may see it; the API refuses anyone else.
	players, err := h.serverAPI.ListServerPlayers(ctx, connect.NewRequest(&hubv1.ListServerPlayersRequest{ServerId: id}))
	switch connect.CodeOf(err) {
	case connect.CodeUnauthenticated:
		sp.PlayersNote = "Log in to see who's on."
		if !h.canLogIn() {
			sp.PlayersNote = "Only members can see who's on, and this hub has no login."
		}
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
	// The feed's gaps (ADR-0011 §5): the same access as the matches, and nothing shown when they
	// are private or none were recorded.
	gaps, err := h.statsAPI.ListGaps(ctx, connect.NewRequest(&hubv1.ListGapsRequest{ServerId: id}))
	switch connect.CodeOf(err) {
	case connect.CodeUnauthenticated, connect.CodePermissionDenied:
		// Private stats: the matches' note already says so.
	default:
		if err != nil {
			h.serverError(w, r, fmt.Errorf("ListGaps: %w", err))
			return
		}
		for _, g := range gaps.Msg.GetGaps() {
			sp.Gaps = append(sp.Gaps, templates.GapRow{From: g.GetFrom().AsTime(), To: g.GetTo().AsTime(),
				What: templates.GapWords(g.GetReason(), int(g.GetEvents()), int(g.GetKills()), int(g.GetDeaths()), int(g.GetPlayers()))})
		}
		sp.GapsMore = gaps.Msg.GetMore()
	}
	if p.User.CanModerate() {
		sp.Mod = h.modControls(r, id, s.GetStatus().GetCapabilities())
		if s.GetDriver() == "cloudvm" {
			sp.Host = h.hostPanel(r, id, p.User.Owner)
		}
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
	// A game whose stats make no combat board (a server-less game's participation, gravel#2) has
	// no scope here: its page shows its participation.
	for _, id := range sortedGameIDs(games) {
		if !games[id].GetCombatBoards() {
			continue
		}
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
		if !g.GetCombatBoards() {
			h.fail(w, r, http.StatusNotFound, "No such board", g.GetName()+" has no leaderboard: its stats are its community's participation, on its game page.")
			return
		}
		req.GameId, bp.Heading = id, g.GetName()+" leaderboard"
		columns = layoutOf(g, columns)
		withGame(&bp.Page, id, g.GetName())
	case "server":
		i := slices.IndexFunc(list, func(s *hubv1.Server) bool { return s.GetId() == id })
		if i < 0 {
			h.fail(w, r, http.StatusNotFound, "No such board", "This hub has no server by that name.")
			return
		}
		req.ServerId, bp.Heading = id, list[i].GetName()+" leaderboard"
		columns = layoutOf(games[list[i].GetGameId()], columns)
		withGame(&bp.Page, list[i].GetGameId(), serverRow(list[i], games).Game)
	default:
		h.fail(w, r, http.StatusNotFound, "No such board", "That is not a board this hub has.")
		return
	}

	// The window: all time, this week, this month, or a named season.
	ctx, _ := withBrowser(r)
	var loc *time.Location
	bp.Windows, loc = h.windowOptions(ctx, q.window)
	req.Window, req.Season = windowOf(q.window)

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
		bp.Range = rangeOf(m.GetFrom(), m.GetTo(), loc)
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

// windowOptions are a board's windows (all time, this week, this month and each season in the
// Organization settings) with selected marked, and the organization's timezone, in which a
// window's dates are shown.
func (h *Handler) windowOptions(ctx context.Context, selected string) ([]templates.Option, *time.Location) {
	out := []templates.Option{
		{Value: "all", Label: "All time", Selected: selected == "all"},
		{Value: "week", Label: "This week", Selected: selected == "week"},
		{Value: "month", Label: "This month", Selected: selected == "month"},
	}
	loc := time.UTC
	if set, err := h.orgAPI.GetOrganizationSettings(ctx, connect.NewRequest(&hubv1.GetOrganizationSettingsRequest{})); err == nil {
		loc = zoneOf(set.Msg.GetSettings())
		for _, se := range set.Msg.GetSettings().GetStats().GetSeasons() {
			v := "season:" + se.GetName()
			out = append(out, templates.Option{Value: v, Label: se.GetName(), Selected: selected == v})
		}
	}
	return out, loc
}

// windowOf is a page's window parameter ("all", "week", "month", "season:<name>") as the API's
// window and season.
func windowOf(param string) (window, season string) {
	if name, ok := strings.CutPrefix(param, "season:"); ok {
		return "season", name
	}
	return param, ""
}

// rangeOf is a window's dates as a page shows them, or "" for all time.
func rangeOf(from, to *timestamppb.Timestamp, loc *time.Location) string {
	if from == nil || to == nil {
		return ""
	}
	return from.AsTime().In(loc).Format("Jan 2, 2006") + " to " + to.AsTime().Add(-time.Second).In(loc).Format("Jan 2, 2006")
}
