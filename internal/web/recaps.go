package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/internal/web/templates"
)

// A session recap's page (ADR-0018 §4): what one server's window held, at a link that stays, so
// the bot's post can point at it. The page reads StatsService.GetSessionRecap in process like the
// boards do; the window's rules (at most a day, ended) and who may see it are the API's.

// notARecap is what a link that names no recap window says.
const notARecap = "This link is not a recap window"

func (h *Handler) recap(w http.ResponseWriter, r *http.Request) {
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
	s := resp.Msg.GetServer()

	// The window as the link gives it. A parameter that isn't a time or a number can't be asked
	// for; one that is missing is the API's to refuse.
	req := &hubv1.GetSessionRecapRequest{ServerId: s.GetId()}
	qs := r.URL.Query()
	for _, p := range []struct {
		name string
		to   **timestamppb.Timestamp
	}{{"from", &req.From}, {"to", &req.To}} {
		v := qs.Get(p.name)
		if v == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			h.fail(w, r, http.StatusBadRequest, "Not a recap", fmt.Sprintf("%s: %s is not a time.", notARecap, p.name))
			return
		}
		*p.to = timestamppb.New(t)
	}
	if v := qs.Get("floor"); v != "" {
		f, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			h.fail(w, r, http.StatusBadRequest, "Not a recap", notARecap+": the floor is not a whole number.")
			return
		}
		req.Floor = int32(f)
	}

	p, b, err := h.page(r, s.GetName()+" recap")
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	withGame(&p.Page, s.GetGameId(), h.gameName(r, s.GetGameId()))
	rp := templates.RecapPage{Page: p.Page, ServerID: s.GetId(), ServerName: s.GetName(),
		BoardURL: "/boards?scope=" + url.QueryEscape("server:"+s.GetId())}

	recap, err := h.statsAPI.GetSessionRecap(ctx, connect.NewRequest(req))
	switch connect.CodeOf(err) {
	case connect.CodeUnauthenticated, connect.CodePermissionDenied:
		rp.Note = "Recaps are private on this hub until its privacy policy covers stats."
	case connect.CodeInvalidArgument:
		h.fail(w, r, http.StatusBadRequest, "Not a recap", notARecap+": "+connectMessage(err)+".")
		return
	default:
		if err != nil {
			h.serverError(w, r, fmt.Errorf("GetSessionRecap: %w", err))
			return
		}
		loc := h.orgZone(ctx)
		m := recap.Msg
		from, to := m.GetFrom().AsTime().In(loc), m.GetTo().AsTime().In(loc)
		rp.Window = recapWindow(from, to)
		rp.Floor, rp.PeakPlayers, rp.Minutes, rp.MinutesAtFloor, rp.Players = int(m.GetFloor()), int(m.GetPeakPlayers()), int(m.GetMinutes()),
			int(m.GetMinutesAtFloor()), int(m.GetPlayers())
		for _, x := range m.GetMatches() {
			started := x.GetStartedAt().AsTime().In(loc)
			row := templates.RecapMatch{Started: clock(started, from), Map: x.GetMap(), Players: int(x.GetPlayers())}
			if x.GetEndedAt() != nil {
				row.Ended = clock(x.GetEndedAt().AsTime().In(loc), started)
			}
			for _, t := range x.GetTeams() {
				row.Teams = append(row.Teams, templates.Team{Name: t.GetName(), Score: t.GetScore()})
			}
			rp.Matches = append(rp.Matches, row)
		}
		for _, e := range m.GetTop() {
			row := templates.BoardRow{Rank: int(e.GetRank()), Name: e.GetName(), Pseudonymous: e.GetPseudonymous(), UserID: e.GetUserId()}
			for _, c := range defaultBoard {
				row.Cells = append(row.Cells, cell(e, c))
			}
			rp.Top = append(rp.Top, row)
		}
	}
	h.render(w, r, http.StatusOK, b, templates.Recap(rp), templates.RecapContent(rp))
}

// orgZone is the Organization settings' stats.timezone, UTC when unset or unreadable.
func (h *Handler) orgZone(ctx context.Context) *time.Location {
	set, err := h.orgAPI.GetOrganizationSettings(ctx, connect.NewRequest(&hubv1.GetOrganizationSettingsRequest{}))
	if err != nil {
		return time.UTC
	}
	return zoneOf(set.Msg.GetSettings())
}

// zoneOf is a settings document's stats.timezone, UTC when unset or unknown.
func zoneOf(s *hubv1.OrganizationSettings) *time.Location {
	if tz := s.GetStats().GetTimezone(); tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			return l
		}
	}
	return time.UTC
}

// recapWindow is a recap's window as the page shows it, in its zone: "Wed Oct 7, 2026, 18:00 to
// 22:00 CDT", with the end's date when it falls on another day and both zones when they differ.
func recapWindow(from, to time.Time) string {
	start := from.Format("Mon Jan 2, 2006, 15:04")
	if from.Format("MST") != to.Format("MST") {
		start += " " + from.Format("MST")
	}
	if sameDay(from, to) {
		return start + " to " + to.Format("15:04 MST")
	}
	return start + " to " + to.Format("Mon Jan 2, 2006, 15:04 MST")
}

// clock is t's time of day, with its date when it is not on ref's day.
func clock(t, ref time.Time) string {
	if sameDay(t, ref) {
		return t.Format("15:04")
	}
	return t.Format("Jan 2, 15:04")
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}
