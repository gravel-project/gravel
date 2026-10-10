package web

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"connectrpc.com/connect"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/internal/session"
	"github.com/gravel-project/gravel/internal/web/templates"
)

// The owner's Members page (ADR-0013): every member, and the moderator role granted or revoked.

// members is the owner's Members page; anyone else is told it is the owner's.
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
		h.fail(w, r, http.StatusForbidden, "Owner only", "Only the hub's owner manages members.")
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
	p, b, err := h.page(r, "Members")
	if err != nil {
		return templates.MembersPage{}, b, 0, err
	}
	if flash != nil {
		p.Flash = flash
	}
	mp := templates.MembersPage{Page: p.Page}
	for _, u := range resp.Msg.GetUsers() {
		var accounts []string
		for _, i := range u.GetIdentities() {
			accounts = append(accounts, h.providerDisplay(i.GetProvider()))
		}
		mp.Members = append(mp.Members, templates.MemberRow{ID: u.GetId(), DisplayName: u.GetDisplayName(), Accounts: strings.Join(accounts, ", "),
			Joined: u.GetCreatedAt().AsTime(), Owner: u.GetOwner(), Moderator: slices.Contains(u.GetRoles(), "moderator")})
	}
	if next := resp.Msg.GetNextPageToken(); next != "" {
		mp.NextURL = "/members?page=" + url.QueryEscape(next)
	}
	return mp, b, http.StatusOK, nil
}

// setRole grants or revokes the moderator role from the Members page; the API allows the owner only.
func (h *Handler) setRole(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	ctx, b := withBrowser(r)
	granted := r.PostFormValue("granted") == "1"
	resp, err := h.idAPI.SetUserRole(ctx, connect.NewRequest(&hubv1.SetUserRoleRequest{UserId: r.PostFormValue("user_id"), Role: r.PostFormValue("role"), Granted: granted}))
	var flash templates.Flash
	switch connect.CodeOf(err) {
	case connect.CodePermissionDenied:
		b.relay(w)
		h.fail(w, r, http.StatusForbidden, "Owner only", "Only the hub's owner manages members.")
		return
	case connect.CodeInvalidArgument, connect.CodeNotFound:
		flash = templates.Flash{Kind: "err", Text: "That member or role doesn't exist."}
	default:
		if err != nil {
			h.apiError(w, r, b, err)
			return
		}
		name := resp.Msg.GetUser().GetDisplayName()
		flash = templates.Flash{Kind: "ok", Text: name + " is no longer a moderator."}
		if granted {
			flash.Text = name + " is now a moderator."
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
