package web_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
)

// fakeMod is ModerationService with canned answers; it records the calls the pages make. Who may
// call is the API's rule, tested in internal/api; err makes every call fail with a code.
type fakeMod struct {
	hubv1connect.UnimplementedModerationServiceHandler
	mu    sync.Mutex
	calls []string
	err   connect.Code
}

func (f *fakeMod) record(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != 0 {
		return connect.NewError(f.err, errors.New("the player is not on the server"))
	}
	f.calls = append(f.calls, call)
	return nil
}

func (f *fakeMod) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeMod) fail(code connect.Code) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = code
}

func (f *fakeMod) KickPlayer(_ context.Context, req *connect.Request[hubv1.KickPlayerRequest]) (*connect.Response[hubv1.KickPlayerResponse], error) {
	if err := f.record("kick " + req.Msg.GetSubject() + " " + req.Msg.GetReason()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.KickPlayerResponse{AuditId: 1}), nil
}

func (f *fakeMod) BanPlayer(_ context.Context, req *connect.Request[hubv1.BanPlayerRequest]) (*connect.Response[hubv1.BanPlayerResponse], error) {
	if err := f.record("ban " + req.Msg.GetSubject() + " " + req.Msg.GetReason()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.BanPlayerResponse{AuditId: 2}), nil
}

func (f *fakeMod) UnbanPlayer(_ context.Context, req *connect.Request[hubv1.UnbanPlayerRequest]) (*connect.Response[hubv1.UnbanPlayerResponse], error) {
	if err := f.record("unban " + req.Msg.GetSubject()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.UnbanPlayerResponse{AuditId: 3}), nil
}

func (f *fakeMod) Broadcast(_ context.Context, req *connect.Request[hubv1.BroadcastRequest]) (*connect.Response[hubv1.BroadcastResponse], error) {
	if err := f.record("broadcast " + req.Msg.GetMessage()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.BroadcastResponse{AuditId: 4}), nil
}

func (f *fakeMod) ListServerBans(context.Context, *connect.Request[hubv1.ListServerBansRequest]) (*connect.Response[hubv1.ListServerBansResponse], error) {
	if err := f.record(""); err != nil {
		return nil, err
	}
	return connect.NewResponse(&hubv1.ListServerBansResponse{Bans: []*hubv1.Ban{
		{Provider: "steam", Subject: "76561190000000009", Reason: "cheating", BannedBy: "config", BannedAt: timestamppb.New(time.Time{})},
	}}), nil
}

func (f *fakeMod) ListAuditLog(context.Context, *connect.Request[hubv1.ListAuditLogRequest]) (*connect.Response[hubv1.ListAuditLogResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != 0 {
		return nil, connect.NewError(f.err, errors.New("moderators only"))
	}
	at := timestamppb.New(time.Date(2026, 10, 10, 20, 0, 0, 0, time.UTC))
	return connect.NewResponse(&hubv1.ListAuditLogResponse{NextPageToken: "older", Entries: []*hubv1.AuditEntry{
		{Id: 2, At: at, UserId: "u1", UserName: "Jo", Action: "ban", TargetSubject: "76561190000000009", Reason: "cheating", Outcome: "ok"},
		{Id: 1, At: at, AppId: "a1", AppName: "htg-bot", OnBehalfOf: &hubv1.Actor{Provider: "discord", Subject: "42"}, Action: "kick",
			TargetSubject: "76561190000000002", Reason: "afk", Outcome: "player_not_found"},
	}}), nil
}

// moderator logs Jo in and makes them the owner, who moderates (a moderator sees the same).
func moderator(t *testing.T, r *rig) *http.Client {
	t.Helper()
	c := browser(t)
	r.loginAs(t, c, "jo")
	_, acct := get(t, c, r.srv.URL+"/account")
	post(t, c, r.srv.URL+"/account/claim", url.Values{"_csrf": {csrfOf(t, acct)}, "token": {r.token}}, nil)
	return c
}

func TestModerationControls(t *testing.T) {
	r := newRig(t)

	// A member sees who is on, without controls.
	member := browser(t)
	r.loginAs(t, member, "sam")
	if _, body := get(t, member, r.srv.URL+"/servers/htg-wardogs-1"); strings.Contains(body, "Moderate ") || strings.Contains(body, "mod-heading") {
		t.Errorf("a member sees moderation controls")
	}

	// The owner sees a form per player with the driver's actions, the broadcast and ban-by-id
	// forms, the ban list with unban, and the log link.
	c := moderator(t, r)
	_, body := get(t, c, r.srv.URL+"/servers/htg-wardogs-1")
	for _, want := range []string{"Moderate Jo &lt;script&gt;", `<option value="kick">Kick</option>`, `<option value="message">Message</option>`,
		"Message everyone on the server", "by their SteamID64", "Unban 76561190000000009", `href="/servers/htg-wardogs-1/log"`, "cheating"} {
		if !strings.Contains(body, want) {
			t.Errorf("the owner's server page lacks %q", want)
		}
	}
	// The game wrote that ban in its config: no date.
	if strings.Contains(body, "Jan 1, 00:00") {
		t.Error("a config ban shows year 1")
	}

	// A driver without a capability hides its control.
	r.servers.mu.Lock()
	r.servers.servers[0].Status.Capabilities = []string{"status", "players", "message"}
	r.servers.mu.Unlock()
	_, body = get(t, c, r.srv.URL+"/servers/htg-wardogs-1")
	if strings.Contains(body, `<option value="kick">`) || strings.Contains(body, `<option value="ban">`) || strings.Contains(body, "by their SteamID64") ||
		strings.Contains(body, "Message everyone") || strings.Contains(body, "Unban ") || !strings.Contains(body, `<option value="message">`) {
		t.Errorf("controls without capabilities: %s", body)
	}
}

func TestModerateConfirmsThenSends(t *testing.T) {
	r := newRig(t)
	c := moderator(t, r)
	_, page := get(t, c, r.srv.URL+"/servers/htg-wardogs-1")
	csrf := csrfOf(t, page)
	form := url.Values{"_csrf": {csrf}, "action": {"kick"}, "subject": {"76561190000000002"}, "name": {"Stranger"}, "text": {"afk in spawn"}}

	// First the question, with nothing sent.
	resp, body := post(t, c, r.srv.URL+"/servers/htg-wardogs-1/moderate", form, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Kick Stranger from HTG WARDOGS | NA WEST | #1?") || !strings.Contains(body, "afk in spawn") ||
		!strings.Contains(body, `name="confirm" value="1"`) || !strings.Contains(body, ">Cancel<") {
		t.Fatalf("confirmation: %d %s", resp.StatusCode, body)
	}
	if calls := r.mod.recorded(); len(calls) != 1 { // the page's ban list read only
		t.Fatalf("sent before the confirmation: %v", calls)
	}

	// Confirmed: sent, then back to the server with the result.
	form.Set("confirm", "1")
	resp, _ = post(t, c, r.srv.URL+"/servers/htg-wardogs-1/moderate", form, nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/servers/htg-wardogs-1" {
		t.Fatalf("confirmed: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if calls := r.mod.recorded(); calls[len(calls)-1] != "kick 76561190000000002 afk in spawn" {
		t.Errorf("calls = %v", calls)
	}
	if _, body = get(t, c, r.srv.URL+"/servers/htg-wardogs-1"); !strings.Contains(body, "Kicked Stranger.") {
		t.Error("no flash after the kick")
	}

	// A broadcast asks its own question.
	resp, body = post(t, c, r.srv.URL+"/servers/htg-wardogs-1/moderate", url.Values{"_csrf": {csrf}, "action": {"broadcast"}, "text": {"restart in 5"}}, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Message everyone on HTG WARDOGS | NA WEST | #1?") {
		t.Errorf("broadcast confirmation: %s", body)
	}

	// A missing reason never reaches the API; an unknown action is refused; so is a form
	// without the token.
	before := len(r.mod.recorded())
	if resp, _ = post(t, c, r.srv.URL+"/servers/htg-wardogs-1/moderate", url.Values{"_csrf": {csrf}, "action": {"ban"}, "subject": {"76561190000000002"}, "confirm": {"1"}}, nil); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("no reason: %d", resp.StatusCode)
	}
	if resp, _ = post(t, c, r.srv.URL+"/servers/htg-wardogs-1/moderate", url.Values{"_csrf": {csrf}, "action": {"nuke"}, "text": {"x"}}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("an unknown action: %d", resp.StatusCode)
	}
	form.Del("_csrf")
	if resp, _ = post(t, c, r.srv.URL+"/servers/htg-wardogs-1/moderate", form, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("no token: %d", resp.StatusCode)
	}
	if len(r.mod.recorded()) != before {
		t.Errorf("refused forms reached the API: %v", r.mod.recorded()[before:])
	}

	// The API's refusals: a player who left is a flash; a non-moderator is a 403.
	form.Set("_csrf", csrf)
	r.mod.fail(connect.CodeNotFound)
	post(t, c, r.srv.URL+"/servers/htg-wardogs-1/moderate", form, nil)
	r.mod.fail(0)
	if _, body = get(t, c, r.srv.URL+"/servers/htg-wardogs-1"); !strings.Contains(body, "Nothing done: the player is not on the server.") {
		t.Errorf("player gone: %s", body)
	}
	r.mod.fail(connect.CodePermissionDenied)
	if resp, _ = post(t, c, r.srv.URL+"/servers/htg-wardogs-1/moderate", form, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("refused by the API: %d", resp.StatusCode)
	}
}

func TestModerationLog(t *testing.T) {
	r := newRig(t)
	c := moderator(t, r)
	resp, body := get(t, c, r.srv.URL+"/servers/htg-wardogs-1/log")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("log: %d", resp.StatusCode)
	}
	for _, want := range []string{"Moderation log", "HTG WARDOGS | NA WEST | #1", ">Jo<", "htg-bot for Discord 42", "cheating", "player not found", "Older", "page=older"} {
		if !strings.Contains(body, want) {
			t.Errorf("the log lacks %q", want)
		}
	}
	r.mod.fail(connect.CodePermissionDenied)
	if resp, _ = get(t, c, r.srv.URL+"/servers/htg-wardogs-1/log"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a non-moderator: %d", resp.StatusCode)
	}
	r.mod.fail(connect.CodeUnauthenticated)
	if resp, _ = get(t, browser(t), r.srv.URL+"/servers/htg-wardogs-1/log"); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Errorf("anonymous: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}
