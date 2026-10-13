package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"connectrpc.com/connect"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/internal/session"
	"github.com/gravel-project/gravel/internal/web/templates"
)

// The Members page (ADR-0027): every member with their grants, for anyone who may read members;
// a caller holding roles:grant also grants and revokes roles here, instance-wide or for a game.

// members is the Members page; anyone who may not read members is told so.
func (h *Handler) members(w http.ResponseWriter, r *http.Request) {
	if _, ok := session.FromContext(r.Context()); !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	mp, b, status, err := h.membersPage(r, r.URL.Query().Get("page"), nil)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	if status == http.StatusForbidden {
		h.fail(w, r, http.StatusForbidden, "Staff only", "Only the hub's staff see its members.")
		return
	}
	h.render(w, r, http.StatusOK, b, templates.Members(mp), templates.MembersContent(mp))
}

func (h *Handler) membersPage(r *http.Request, token string, flash *templates.Flash) (templates.MembersPage, *browser, int, error) {
	ctx, _ := withBrowser(r)
	resp, err := h.idAPI.ListUsers(ctx, connect.NewRequest(&hubv1.ListUsersRequest{PageSize: 100, PageToken: token}))
	switch connect.CodeOf(err) {
	case connect.CodePermissionDenied, connect.CodeUnauthenticated:
		return templates.MembersPage{}, nil, http.StatusForbidden, nil
	}
	if err != nil {
		return templates.MembersPage{}, nil, 0, fmt.Errorf("ListUsers: %w", err)
	}
	games, err := h.serverAPI.ListGames(ctx, connect.NewRequest(&hubv1.ListGamesRequest{}))
	if err != nil {
		return templates.MembersPage{}, nil, 0, fmt.Errorf("ListGames: %w", err)
	}
	gameName := map[string]string{}
	for _, g := range games.Msg.GetGames() {
		gameName[g.GetId()] = g.GetName()
	}
	p, b, err := h.page(r, "Members")
	if err != nil {
		return templates.MembersPage{}, b, 0, err
	}
	if flash != nil {
		p.Flash = flash
	}
	mp := templates.MembersPage{Page: p.Page}
	for _, role := range []string{"moderator", "operator", "admin"} {
		mp.Roles = append(mp.Roles, templates.Option{Value: role, Label: role, Selected: role == "moderator"})
	}
	mp.Scopes = append(mp.Scopes, templates.Option{Value: "", Label: "Every game", Selected: true})
	for _, g := range games.Msg.GetGames() {
		mp.Scopes = append(mp.Scopes, templates.Option{Value: g.GetId(), Label: g.GetName()})
	}
	for _, u := range resp.Msg.GetUsers() {
		var accounts []string
		for _, i := range u.GetIdentities() {
			accounts = append(accounts, h.providerDisplay(i.GetProvider()))
		}
		row := templates.MemberRow{ID: u.GetId(), DisplayName: u.GetDisplayName(), Accounts: strings.Join(accounts, ", "),
			Joined: u.GetCreatedAt().AsTime(), Owner: u.GetOwner()}
		for _, g := range u.GetGrants() {
			row.Grants = append(row.Grants, templates.GrantView{Role: g.GetRole(), GameID: g.GetGameId(), Label: grantLabel(g.GetRole(), g.GetGameId(), gameName)})
		}
		mp.Members = append(mp.Members, row)
	}
	if next := resp.Msg.GetNextPageToken(); next != "" {
		mp.NextURL = "/members?page=" + url.QueryEscape(next)
	}
	return mp, b, http.StatusOK, nil
}

func grantLabel(role, game string, names map[string]string) string {
	if game == "" {
		return role
	}
	if n := names[game]; n != "" {
		return role + " · " + n
	}
	return role + " · " + game
}

// setRole grants or revokes a role from the Members page; the API applies the rules (ADR-0027 §5).
func (h *Handler) setRole(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	ctx, b := withBrowser(r)
	granted := r.PostFormValue("granted") == "1"
	role, game := r.PostFormValue("role"), r.PostFormValue("game_id")
	resp, err := h.idAPI.SetUserRole(ctx, connect.NewRequest(&hubv1.SetUserRoleRequest{UserId: r.PostFormValue("user_id"), Role: role, GameId: game, Granted: granted}))
	var flash templates.Flash
	switch connect.CodeOf(err) {
	case connect.CodePermissionDenied:
		flash = templates.Flash{Kind: "err", Text: "You can't change that grant: " + strings.TrimPrefix(connectMessage(err), "not allowed to change that grant: ") + "."}
	case connect.CodeInvalidArgument, connect.CodeNotFound:
		flash = templates.Flash{Kind: "err", Text: "That member, role or game doesn't exist."}
	default:
		if err != nil {
			h.apiError(w, r, b, err)
			return
		}
		what := role
		if game != "" {
			what += " for " + game
		}
		name := resp.Msg.GetUser().GetDisplayName()
		flash = templates.Flash{Kind: "ok", Text: name + " is no longer " + what + "."}
		if granted {
			flash.Text = name + " is now " + what + "."
		}
	}
	if !isHX(r) {
		b.relay(w)
		h.setFlash(w, flash.Kind, flash.Text)
		http.Redirect(w, r, "/members", http.StatusSeeOther)
		return
	}
	mp, b2, _, err := h.membersPage(r, "", &flash)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	b.relay(w)
	h.render(w, r, http.StatusOK, b2, templates.Members(mp), templates.MembersContent(mp))
}
