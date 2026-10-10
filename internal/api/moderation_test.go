package api_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/gravel-project/gravel/drivers"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/apps"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/store"
)

const steamA = "76561190000000001"

// fakeDriver is the one driver the moderation rig hands out; errs fail a call by its name.
type fakeDriver struct {
	mu    sync.Mutex
	caps  []string // nil: the server was never observed
	errs  map[string]error
	calls []string
	bans  []drivers.Ban
	draft drivers.ConfigDraft
}

func (f *fakeDriver) Driver(id string) (drivers.ExternalReachable, []string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id != "wd-1" {
		return nil, nil, false
	}
	return f, f.caps, true
}

func (f *fakeDriver) record(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	return f.errs[strings.Fields(call)[0]]
}

func (f *fakeDriver) Build(context.Context) (string, error)          { return "b", nil }
func (f *fakeDriver) Capabilities(context.Context) ([]string, error) { return f.caps, nil }
func (f *fakeDriver) Status(context.Context) (drivers.Status, error) { return drivers.Status{}, nil }
func (f *fakeDriver) Players(context.Context) ([]drivers.Player, error) {
	return nil, nil
}
func (f *fakeDriver) SetCredential(string) {}
func (f *fakeDriver) Kick(_ context.Context, p drivers.Identity, reason string) error {
	return f.record("kick " + p.Provider + ":" + p.Subject + " " + reason)
}
func (f *fakeDriver) Kill(_ context.Context, p drivers.Identity) error {
	return f.record("kill " + p.Subject)
}
func (f *fakeDriver) Message(_ context.Context, p drivers.Identity, m string) error {
	return f.record("message " + p.Subject + " " + m)
}
func (f *fakeDriver) Broadcast(_ context.Context, m string) error { return f.record("broadcast " + m) }
func (f *fakeDriver) MovePlayer(_ context.Context, p drivers.Identity, team string) error {
	return f.record("move " + p.Subject + " " + team)
}
func (f *fakeDriver) Ban(_ context.Context, p drivers.Identity, reason string) error {
	return f.record("ban " + p.Subject + " " + reason)
}
func (f *fakeDriver) Unban(_ context.Context, p drivers.Identity) error {
	return f.record("unban " + p.Subject)
}
func (f *fakeDriver) Bans(context.Context) ([]drivers.Ban, error) {
	return f.bans, f.record("bans")
}
func (f *fakeDriver) Config(context.Context) (drivers.ConfigDocument, error) {
	return drivers.ConfigDocument{Revision: "r1", Text: "[S]\r\nPassword=<redacted>\r\n", Writable: true,
		Sections: []drivers.ConfigSection{{Name: "S", AppliesWhen: "applied", Keys: []string{"Password"}, Locked: []string{"Port: RCONPort"}}}}, f.record("config")
}
func (f *fakeDriver) PlanConfig(_ context.Context, d drivers.ConfigDraft) (drivers.ConfigPlan, error) {
	f.mu.Lock()
	f.draft = d
	f.mu.Unlock()
	return drivers.ConfigPlan{Revision: "r1", Changes: []drivers.ConfigChange{{Section: "K", Key: "ScorePeriod", Before: []string{"24"}, After: []string{"27"}}},
		Result: drivers.ConfigResult{OK: true, Outcomes: []drivers.ConfigOutcome{{Section: "K", State: "next-match"}}}}, f.record("plan")
}
func (f *fakeDriver) ApplyConfig(_ context.Context, d drivers.ConfigDraft, revision string) (drivers.ConfigResult, error) {
	return drivers.ConfigResult{OK: true, Revision: revision + "+1"}, f.record("apply " + revision)
}
func (f *fakeDriver) SetConfigBans(_ context.Context, bans []drivers.Identity) (drivers.ConfigResult, error) {
	return drivers.ConfigResult{OK: true}, f.record(fmt.Sprint("setbans ", len(bans)))
}

var allCaps = []string{drivers.CapBan, drivers.CapBans, drivers.CapBroadcast, drivers.CapKick, drivers.CapKill,
	drivers.CapMessage, drivers.CapMovePlayer, drivers.CapUnban}

func TestModerationIsTheOwnersOrAnAppsWithTheScope(t *testing.T) {
	r := newRig(t)
	r.withServer(t)
	r.drv.caps = allCaps
	ctx := context.Background()
	kick := &hubv1.KickPlayerRequest{ServerId: "wd-1", Subject: steamA, Reason: "spawn camping"}

	anon := hubv1connect.NewModerationServiceClient(http.DefaultClient, r.srv.URL)
	if _, err := anon.KickPlayer(ctx, connect.NewRequest(kick)); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("anonymous: %v", err)
	}
	member := r.user(t, "Member", "2")
	asMember := hubv1connect.NewModerationServiceClient(cookieClient(http.DefaultClient, r.login(t, member.ID)), r.srv.URL)
	if _, err := asMember.KickPlayer(ctx, connect.NewRequest(kick)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a member: %v", err)
	}
	if _, err := asMember.ListAuditLog(ctx, connect.NewRequest(&hubv1.ListAuditLogRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a member reading the log: %v", err)
	}
	reader := hubv1connect.NewModerationServiceClient(bearerClient(http.DefaultClient, r.bearer(t, "reader", apps.ScopeServersRead)), r.srv.URL)
	if _, err := reader.KickPlayer(ctx, connect.NewRequest(kick)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("an app with servers:read only: %v", err)
	}
	if len(r.audit.Entries()) != 0 || len(r.drv.calls) != 0 {
		t.Fatalf("refused callers reached the driver: %v", r.drv.calls)
	}

	owner := r.user(t, "Owner", "1")
	if _, err := r.org.Claim(ctx, r.token, owner.ID); err != nil {
		t.Fatal(err)
	}
	asOwner := hubv1connect.NewModerationServiceClient(cookieClient(http.DefaultClient, r.login(t, owner.ID)), r.srv.URL)
	got, err := asOwner.KickPlayer(ctx, connect.NewRequest(kick))
	if err != nil || got.Msg.GetAuditId() != 1 {
		t.Fatalf("the owner: %v, %v", got, err)
	}
	if _, err := asOwner.Broadcast(ctx, connect.NewRequest(&hubv1.BroadcastRequest{ServerId: "wd-1", Message: "hi",
		OnBehalfOf: &hubv1.Actor{Provider: "discord", Subject: "42"}})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("the owner acting for someone: %v", err)
	}

	// The bot acts for Discord account 42, a member the owner made a moderator (ADR-0013).
	mod := r.user(t, "Mod", "42")
	if _, err := r.ids.SetRole(ctx, mod.ID, store.RoleModerator, true, owner.ID); err != nil {
		t.Fatal(err)
	}
	bot := hubv1connect.NewModerationServiceClient(bearerClient(http.DefaultClient, r.bearer(t, "htg-bot", apps.ScopeServersModerate)), r.srv.URL)
	if _, err := bot.BanPlayer(ctx, connect.NewRequest(&hubv1.BanPlayerRequest{ServerId: "wd-1", Subject: steamA, Reason: "cheating",
		OnBehalfOf: &hubv1.Actor{Provider: "discord", Subject: "42"}})); err != nil {
		t.Fatal(err)
	}
	if _, err := bot.MovePlayer(ctx, connect.NewRequest(&hubv1.MovePlayerRequest{ServerId: "wd-1", Subject: steamA, Team: "Valkyra", Respawn: true})); err != nil {
		t.Fatal(err)
	}
	want := []string{"kick steam:" + steamA + " spawn camping", "ban " + steamA + " cheating", "move " + steamA + " Valkyra", "kill " + steamA}
	if fmt.Sprint(r.drv.calls) != fmt.Sprint(want) {
		t.Errorf("driver calls = %q, want %q", r.drv.calls, want)
	}

	log, err := bot.ListAuditLog(ctx, connect.NewRequest(&hubv1.ListAuditLogRequest{ServerId: "wd-1"}))
	if err != nil || len(log.Msg.GetEntries()) != 3 || log.Msg.GetNextPageToken() != "" {
		t.Fatalf("log = %v, %v", log, err)
	}
	mv, ban, k := log.Msg.GetEntries()[0], log.Msg.GetEntries()[1], log.Msg.GetEntries()[2]
	if mv.GetAction() != "move_player" || mv.GetTeam() != "Valkyra" || !mv.GetRespawn() || mv.GetOutcome() != "ok" {
		t.Errorf("move = %v", mv)
	}
	if ban.GetOnBehalfOf().GetSubject() != "42" || ban.GetAppId() == "" || ban.GetUserId() != "" || ban.GetReason() != "cheating" {
		t.Errorf("ban = %v", ban)
	}
	if k.GetUserId() != owner.ID.String() || k.GetAppId() != "" || k.GetTargetSubject() != steamA || k.GetFinishedAt() == nil {
		t.Errorf("kick = %v", k)
	}
}

func TestModerationErrorsSayWhatHappenedNotWhere(t *testing.T) {
	r := newRig(t)
	r.withServer(t)
	ctx := context.Background()
	bot := hubv1connect.NewModerationServiceClient(bearerClient(http.DefaultClient, r.bearer(t, "htg-bot", apps.ScopeServersModerate)), r.srv.URL)
	kick := &hubv1.KickPlayerRequest{ServerId: "wd-1", Subject: steamA, Reason: "x"}

	if _, err := bot.KickPlayer(ctx, connect.NewRequest(kick)); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("never observed: %v", err)
	}
	r.drv.caps = []string{drivers.CapBan}
	if _, err := bot.KickPlayer(ctx, connect.NewRequest(kick)); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("not offered: %v", err)
	}
	r.drv.caps = allCaps
	if _, err := bot.KickPlayer(ctx, connect.NewRequest(&hubv1.KickPlayerRequest{ServerId: "nope", Subject: steamA, Reason: "x"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("unknown server: %v", err)
	}
	if _, err := bot.KickPlayer(ctx, connect.NewRequest(&hubv1.KickPlayerRequest{ServerId: "wd-1", Subject: steamA})); connect.CodeOf(err) != connect.CodeInvalidArgument ||
		!strings.Contains(err.Error(), "reason") {
		t.Errorf("no reason: %v", err)
	}

	addr := "dial tcp 203.0.113.10:7789"
	for _, tc := range []struct {
		err  error
		code connect.Code
		word string
	}{
		{fmt.Errorf("%w: %s", drivers.ErrPlayerNotFound, addr), connect.CodeNotFound, "player_not_found"},
		{&drivers.RejectedError{Code: "message_too_long", Err: errors.New(addr)}, connect.CodeInvalidArgument, "rejected"},
		{fmt.Errorf("%w: %s", drivers.ErrCredentialRefused, addr), connect.CodeUnavailable, "credential_refused"},
		{fmt.Errorf("%w: %s", drivers.ErrRateLimited, addr), connect.CodeUnavailable, "rate_limited"},
		{fmt.Errorf("%s: %w", addr, context.DeadlineExceeded), connect.CodeUnavailable, "unreachable"},
		{errors.New(addr + ": something odd"), connect.CodeInternal, "error"},
	} {
		r.drv.errs["kick"] = tc.err
		before := len(r.audit.Entries())
		_, err := bot.KickPlayer(ctx, connect.NewRequest(kick))
		if connect.CodeOf(err) != tc.code || strings.Contains(err.Error(), "203.0.113.10") {
			t.Errorf("%v: answered %v", tc.err, err)
		}
		es := r.audit.Entries()
		if len(es) != before+1 || es[len(es)-1].Outcome != tc.word {
			t.Errorf("%v: outcome %q, want %q", tc.err, es[len(es)-1].Outcome, tc.word)
		}
	}
	r.drv.errs["kick"] = &drivers.RejectedError{Code: "message_too_long", Err: errors.New(addr)}
	if _, err := bot.KickPlayer(ctx, connect.NewRequest(kick)); err == nil || !strings.Contains(err.Error(), "message_too_long") {
		t.Errorf("the game's code is shown: %v", err)
	}
}

func TestListServerBansResolvesMembers(t *testing.T) {
	r := newRig(t)
	r.withServer(t)
	r.drv.caps = allCaps
	at := time.Date(2026, 10, 9, 20, 0, 0, 0, time.UTC)
	r.drv.bans = []drivers.Ban{
		{Identity: drivers.Identity{Provider: "steam", Subject: steamA}, At: at, By: "rcon", Reason: "cheating"},
		{Identity: drivers.Identity{Provider: "steam", Subject: "76561190000000002"}, By: "config"},
	}
	linked := r.user(t, "Linked", "3")
	if err := r.idSt.LinkIdentity(context.Background(), store.Identity{UserID: linked.ID, Provider: "steam", Subject: steamA, DisplayName: "Linked",
		VerificationMethod: identity.MethodOpenID, VerifiedAt: at}); err != nil {
		t.Fatal(err)
	}
	bot := hubv1connect.NewModerationServiceClient(bearerClient(http.DefaultClient, r.bearer(t, "htg-bot", apps.ScopeServersModerate)), r.srv.URL)
	got, err := bot.ListServerBans(context.Background(), connect.NewRequest(&hubv1.ListServerBansRequest{ServerId: "wd-1"}))
	if err != nil || len(got.Msg.GetBans()) != 2 {
		t.Fatalf("bans = %v, %v", got, err)
	}
	b0, b1 := got.Msg.GetBans()[0], got.Msg.GetBans()[1]
	if b0.GetUserId() != linked.ID.String() || !b0.GetBannedAt().AsTime().Equal(at) || b0.GetBannedBy() != "rcon" {
		t.Errorf("first = %v", b0)
	}
	if b1.GetUserId() != "" || b1.GetBannedAt() != nil || b1.GetBannedBy() != "config" {
		t.Errorf("config ban = %v", b1)
	}
	if len(r.audit.Entries()) != 0 {
		t.Error("a read was audited")
	}
}

func TestListAuditLogPages(t *testing.T) {
	r := newRig(t)
	r.withServer(t)
	r.drv.caps = allCaps
	ctx := context.Background()
	bot := hubv1connect.NewModerationServiceClient(bearerClient(http.DefaultClient, r.bearer(t, "htg-bot", apps.ScopeServersModerate)), r.srv.URL)
	for i := range 5 {
		if _, err := bot.Broadcast(ctx, connect.NewRequest(&hubv1.BroadcastRequest{ServerId: "wd-1", Message: fmt.Sprint("m", i)})); err != nil {
			t.Fatal(err)
		}
	}
	var seen []string
	token := ""
	for range 4 {
		page, err := bot.ListAuditLog(ctx, connect.NewRequest(&hubv1.ListAuditLogRequest{PageSize: 2, PageToken: token}))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page.Msg.GetEntries() {
			seen = append(seen, e.GetMessage())
		}
		if token = page.Msg.GetNextPageToken(); token == "" {
			break
		}
	}
	if fmt.Sprint(seen) != "[m4 m3 m2 m1 m0]" {
		t.Errorf("pages = %v", seen)
	}
	if _, err := bot.ListAuditLog(ctx, connect.NewRequest(&hubv1.ListAuditLogRequest{PageToken: "bogus"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a made-up token: %v", err)
	}
}

// The owner grants and revokes the moderator role; a moderator moderates and reads the log as the
// owner does; an app acting for someone acts only for a member who could moderate themselves.
func TestModeratorRole(t *testing.T) {
	r := newRig(t)
	r.withServer(t)
	r.drv.caps = allCaps
	ctx := context.Background()
	owner := r.user(t, "Owner", "1")
	if _, err := r.org.Claim(ctx, r.token, owner.ID); err != nil {
		t.Fatal(err)
	}
	sam := r.user(t, "Sam", "2")
	ids := func(c *http.Client) hubv1connect.IdentityServiceClient {
		return hubv1connect.NewIdentityServiceClient(c, r.srv.URL)
	}
	mods := func(c *http.Client) hubv1connect.ModerationServiceClient {
		return hubv1connect.NewModerationServiceClient(c, r.srv.URL)
	}
	ownerC, samC := cookieClient(http.DefaultClient, r.login(t, owner.ID)), cookieClient(http.DefaultClient, r.login(t, sam.ID))
	grant := func(c *http.Client, granted bool, role string) (*hubv1.User, connect.Code) {
		resp, err := ids(c).SetUserRole(ctx, connect.NewRequest(&hubv1.SetUserRoleRequest{UserId: sam.ID.String(), Role: role, Granted: granted}))
		if err != nil {
			return nil, connect.CodeOf(err)
		}
		return resp.Msg.GetUser(), 0
	}
	kick := &hubv1.KickPlayerRequest{ServerId: "wd-1", Subject: steamA, Reason: "spawn camping"}

	// Only the owner grants, and only a role the hub has.
	if _, code := grant(samC, true, store.RoleModerator); code != connect.CodePermissionDenied {
		t.Errorf("a member granting themselves: %v", code)
	}
	if _, code := grant(ownerC, true, "admin"); code != connect.CodeInvalidArgument {
		t.Errorf("an unknown role: %v", code)
	}
	if _, err := mods(samC).KickPlayer(ctx, connect.NewRequest(kick)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a member before the grant: %v", err)
	}
	u, code := grant(ownerC, true, store.RoleModerator)
	if code != 0 || len(u.GetRoles()) != 1 || u.GetRoles()[0] != store.RoleModerator {
		t.Fatalf("grant: %v %v", u, code)
	}
	if me, err := ids(samC).GetMe(ctx, connect.NewRequest(&hubv1.GetMeRequest{})); err != nil || len(me.Msg.GetUser().GetRoles()) != 1 {
		t.Errorf("GetMe after the grant: %v %v", me, err)
	}

	// A moderator kicks and reads the log; the row names them.
	if _, err := mods(samC).KickPlayer(ctx, connect.NewRequest(kick)); err != nil {
		t.Fatalf("a moderator kicking: %v", err)
	}
	log, err := mods(samC).ListAuditLog(ctx, connect.NewRequest(&hubv1.ListAuditLogRequest{ServerId: "wd-1"}))
	if err != nil || len(log.Msg.GetEntries()) != 1 || log.Msg.GetEntries()[0].GetUserId() != sam.ID.String() {
		t.Errorf("a moderator's log: %v %v", log, err)
	}
	if _, err := mods(samC).ListServerBans(ctx, connect.NewRequest(&hubv1.ListServerBansRequest{ServerId: "wd-1"})); err != nil {
		t.Errorf("a moderator's ban list: %v", err)
	}

	// An app acting for someone: a moderator yes, a plain member or a stranger no; for itself yes.
	bot := mods(bearerClient(http.DefaultClient, r.bearer(t, "htg-bot", apps.ScopeServersModerate)))
	r.user(t, "Plain", "3")
	for subject, want := range map[string]connect.Code{"2": 0, "3": connect.CodePermissionDenied, "999": connect.CodePermissionDenied} {
		_, err := bot.Broadcast(ctx, connect.NewRequest(&hubv1.BroadcastRequest{ServerId: "wd-1", Message: "hi", OnBehalfOf: &hubv1.Actor{Provider: "discord", Subject: subject}}))
		if (want == 0 && err != nil) || (want != 0 && connect.CodeOf(err) != want) {
			t.Errorf("the bot for discord %s: %v, want %v", subject, err, want)
		}
	}
	if _, err := bot.Broadcast(ctx, connect.NewRequest(&hubv1.BroadcastRequest{ServerId: "wd-1", Message: "restart in 5"})); err != nil {
		t.Errorf("the bot for itself: %v", err)
	}

	// Revoked: back to a member. Both changes were recorded, by the owner.
	if u, code := grant(ownerC, false, store.RoleModerator); code != 0 || len(u.GetRoles()) != 0 {
		t.Fatalf("revoke: %v %v", u, code)
	}
	if _, err := mods(samC).KickPlayer(ctx, connect.NewRequest(kick)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a member after the revocation: %v", err)
	}
	if ch := r.idSt.Changes(); len(ch) != 2 || !ch[0].Granted || ch[1].Granted || ch[1].By == nil || *ch[1].By != owner.ID {
		t.Errorf("role changes = %+v", ch)
	}
	// A repeated revocation changes nothing and records nothing.
	if _, code := grant(ownerC, false, store.RoleModerator); code != 0 || len(r.idSt.Changes()) != 2 {
		t.Errorf("a no-op revocation: %v, %d changes", code, len(r.idSt.Changes()))
	}
}
