package web

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"connectrpc.com/connect"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/internal/web/templates"
)

// Moderation from the web (gravel#18, ADR-0013): the controls on a server's page for the owner and
// moderators, a confirmation page before anything is sent, and the server's moderation log. The
// API decides who may act; a control the server's driver doesn't offer is not shown.

// modActions are the actions the moderate route takes, with the question its confirmation asks.
var modActions = map[string]string{
	"kick":      "Kick %s from %s?",
	"ban":       "Ban %s from %s?",
	"unban":     "Unban %s on %s?",
	"message":   "Message %s on %s?",
	"broadcast": "Message everyone on %s?",
}

// modControls are the controls a server's capabilities allow, and its ban list.
func (h *Handler) modControls(r *http.Request, serverID string, caps []string) *templates.ModControls {
	has := func(c string) bool { return slices.Contains(caps, c) }
	m := &templates.ModControls{Kick: has("kick"), Ban: has("ban"), Unban: has("unban"), Message: has("message"), Broadcast: has("broadcast")}
	if !has("bans") {
		return m
	}
	ctx, _ := withBrowser(r)
	resp, err := h.modAPI.ListServerBans(ctx, connect.NewRequest(&hubv1.ListServerBansRequest{ServerId: serverID}))
	if err != nil {
		m.BansNote = "The ban list isn't available right now."
		return m
	}
	for _, b := range resp.Msg.GetBans() {
		row := templates.BanRow{Subject: b.GetSubject(), Name: b.GetSubject(), Reason: b.GetReason(), By: b.GetBannedBy()}
		if t := b.GetBannedAt(); t != nil && t.AsTime().Year() > 1 { // the game writes year 1 for config bans
			row.At = t.AsTime()
		}
		m.Bans = append(m.Bans, row)
	}
	return m
}

// moderate asks for confirmation, then sends the action through the API.
func (h *Handler) moderate(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	id := r.PathValue("id")
	action := r.PostFormValue("action")
	question, ok := modActions[action]
	if !ok {
		h.fail(w, r, http.StatusBadRequest, "No such action", "That is not something the hub can do to a server.")
		return
	}
	subject, name, text := strings.TrimSpace(r.PostFormValue("subject")), strings.TrimSpace(r.PostFormValue("name")), strings.TrimSpace(r.PostFormValue("text"))
	if name == "" {
		name = subject
	}
	back := "/servers/" + id
	if (action != "broadcast" && subject == "") || (action != "unban" && text == "") {
		h.setFlash(w, "err", "Fill in the player and the reason or message first.")
		h.redirect(w, r, back)
		return
	}
	ctx, b := withBrowser(r)
	status, err := h.serverAPI.GetServerStatus(ctx, connect.NewRequest(&hubv1.GetServerStatusRequest{ServerId: id}))
	if err != nil {
		h.fail(w, r, http.StatusNotFound, "No such server", "This hub has no server by that name.")
		return
	}
	serverName := status.Msg.GetServer().GetName()

	if r.PostFormValue("confirm") != "1" {
		p, b2, err := h.page(r, "Confirm")
		if err != nil {
			h.serverError(w, r, err)
			return
		}
		cp := templates.ConfirmPage{Page: p.Page, ServerID: id, ServerName: serverName, Action: action, Subject: subject, Name: name}
		if action == "broadcast" {
			cp.Question = fmt.Sprintf(question, serverName)
		} else {
			cp.Question = fmt.Sprintf(question, name, serverName)
		}
		if action == "message" || action == "broadcast" {
			cp.Message = text
		} else {
			cp.Reason = text
		}
		h.render(w, r, http.StatusOK, b2, templates.Confirm(cp), templates.ConfirmContent(cp))
		return
	}

	switch action {
	case "kick":
		_, err = h.modAPI.KickPlayer(ctx, connect.NewRequest(&hubv1.KickPlayerRequest{ServerId: id, Subject: subject, Reason: text}))
	case "ban":
		_, err = h.modAPI.BanPlayer(ctx, connect.NewRequest(&hubv1.BanPlayerRequest{ServerId: id, Subject: subject, Reason: text}))
	case "unban":
		_, err = h.modAPI.UnbanPlayer(ctx, connect.NewRequest(&hubv1.UnbanPlayerRequest{ServerId: id, Subject: subject, Reason: text}))
	case "message":
		_, err = h.modAPI.MessagePlayer(ctx, connect.NewRequest(&hubv1.MessagePlayerRequest{ServerId: id, Subject: subject, Message: text}))
	case "broadcast":
		_, err = h.modAPI.Broadcast(ctx, connect.NewRequest(&hubv1.BroadcastRequest{ServerId: id, Message: text}))
	}
	switch connect.CodeOf(err) {
	case connect.CodeUnauthenticated:
		h.apiError(w, r, b, err)
		return
	case connect.CodePermissionDenied:
		b.relay(w)
		h.fail(w, r, http.StatusForbidden, "Moderators only", "Only the hub's owner and its moderators can do that.")
		return
	case connect.CodeNotFound:
		h.setFlash(w, "err", "Nothing done: "+connectMessage(err)+".")
	case connect.CodeInvalidArgument, connect.CodeFailedPrecondition, connect.CodeUnavailable:
		h.setFlash(w, "err", "Nothing done: "+connectMessage(err)+".")
	default:
		if err != nil {
			h.apiError(w, r, b, err)
			return
		}
		done := map[string]string{"kick": "Kicked %s.", "ban": "Banned %s.", "unban": "Unbanned %s.", "message": "Messaged %s.", "broadcast": "Messaged everyone%s."}[action]
		who := name
		if action == "broadcast" {
			who = ""
		}
		h.setFlash(w, "ok", fmt.Sprintf(done, who))
	}
	b.relay(w)
	h.redirect(w, r, back)
}

// auditLog is a server's moderation log, for the owner and moderators.
func (h *Handler) auditLog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx, b := withBrowser(r)
	resp, err := h.modAPI.ListAuditLog(ctx, connect.NewRequest(&hubv1.ListAuditLogRequest{ServerId: id, PageSize: 50, PageToken: r.URL.Query().Get("page")}))
	switch connect.CodeOf(err) {
	case connect.CodeUnauthenticated:
		b.relay(w)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	case connect.CodePermissionDenied:
		h.fail(w, r, http.StatusForbidden, "Moderators only", "Only the hub's owner and its moderators can read the moderation log.")
		return
	}
	if err != nil {
		h.serverError(w, r, fmt.Errorf("ListAuditLog: %w", err))
		return
	}
	serverName := id
	if st, err := h.serverAPI.GetServerStatus(ctx, connect.NewRequest(&hubv1.GetServerStatusRequest{ServerId: id})); err == nil {
		serverName = st.Msg.GetServer().GetName()
	}
	p, b2, err := h.page(r, "Moderation log")
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	ap := templates.AuditPage{Page: p.Page, ServerID: id, ServerName: serverName}
	for _, e := range resp.Msg.GetEntries() {
		actor := e.GetUserName()
		if actor == "" && e.GetAppName() != "" {
			actor = e.GetAppName()
			if ob := e.GetOnBehalfOf(); ob != nil {
				actor += " for " + h.providerDisplay(ob.GetProvider()) + " " + ob.GetSubject()
			}
		}
		if actor == "" {
			actor = "a member"
		}
		detail := e.GetReason()
		if e.GetMessage() != "" {
			detail = e.GetMessage()
		}
		if e.GetTeam() != "" {
			detail = "to " + e.GetTeam()
		}
		outcome := e.GetOutcome()
		if outcome == "" {
			outcome = "no result"
		}
		ap.Entries = append(ap.Entries, templates.AuditRow{At: e.GetAt().AsTime(), Actor: actor, Action: strings.ReplaceAll(e.GetAction(), "_", " "),
			Target: e.GetTargetSubject(), Detail: detail, Outcome: strings.ReplaceAll(outcome, "_", " "), OK: outcome == "ok"})
	}
	if next := resp.Msg.GetNextPageToken(); next != "" {
		ap.NextURL = "/servers/" + id + "/log?page=" + url.QueryEscape(next)
	}
	h.render(w, r, http.StatusOK, b2, templates.Audit(ap), templates.AuditContent(ap))
}
