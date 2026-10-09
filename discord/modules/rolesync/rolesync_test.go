package rolesync_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"

	"github.com/gravel-project/gravel/discord/bot"
	"github.com/gravel-project/gravel/discord/hubclient"
	"github.com/gravel-project/gravel/discord/modules/rolesync"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
)

const (
	appID    = "1558123590786748516"
	guildID  = "1558121195340042350"
	linked   = snowflake.ID(1300000000000000001)
	steam    = snowflake.ID(1300000000000000002)
	founders = snowflake.ID(1300000000000000003)
	other    = snowflake.ID(1300000000000000009) // a role the mapping does not name
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func mapping() *hubv1.DiscordSettings {
	return &hubv1.DiscordSettings{GuildId: guildID,
		Roles:       &hubv1.DiscordRoles{Linked: linked.String(), Providers: map[string]string{"steam": steam.String()}},
		Recognition: []*hubv1.DiscordRecognition{{Role: founders.String(), Rule: "first_members", Count: 2}}}
}

func user(name string, ids ...string) *hubv1.User {
	u := &hubv1.User{Id: "u-" + name, DisplayName: name}
	for i := 0; i+1 < len(ids); i += 2 {
		u.Identities = append(u.Identities, &hubv1.Identity{Provider: ids[i], Subject: ids[i+1]})
	}
	return u
}

func member(id uint64, roles ...snowflake.ID) discord.Member {
	return discord.Member{User: discord.User{ID: snowflake.ID(id)}, RoleIDs: roles}
}

func TestDesiredAndPlan(t *testing.T) {
	m, err := rolesync.MappingFrom(mapping())
	if err != nil {
		t.Fatal(err)
	}
	users := []*hubv1.User{
		user("steamonly", "steam", "7656"), // no Discord: no member to role, still the first registration
		user("ana", "discord", "11", "steam", "7657"),
		user("ben", "discord", "12"),
		user("cy", "discord", "13", "steam", "7658"),
	}
	d := rolesync.Desired(m, users)
	if len(d) != 3 {
		t.Fatalf("one entry per Discord identity: %v", d)
	}
	if !d[11][linked] || !d[11][steam] || !d[11][founders] {
		t.Errorf("ana links Steam and is the second registration: %v", d[11])
	}
	if len(d[12]) != 0 {
		t.Errorf("ben has Discord only and registered third: %v", d[12])
	}
	if !d[13][linked] || !d[13][steam] || d[13][founders] {
		t.Errorf("cy links Steam, registered fourth: %v", d[13])
	}

	members := []discord.Member{
		member(11, steam, other),         // gains linked + founders, keeps other
		member(12, linked, founders),     // loses linked (no other identity), keeps founders (recognition is kept)
		member(13, linked, steam),        // already right
		member(14, linked, steam, other), // unknown to the hub: loses linked + steam
		{User: discord.User{ID: 15, Bot: true}, RoleIDs: []snowflake.ID{linked}}, // bots are left alone
	}
	got := rolesync.Plan(m, d, members)
	want := []rolesync.Change{
		{User: 11, Role: linked, Add: true}, {User: 11, Role: founders, Add: true},
		{User: 12, Role: linked},
		{User: 14, Role: linked}, {User: 14, Role: steam},
	}
	if !slices.Equal(got, want) {
		t.Errorf("plan:\n got %v\nwant %v", got, want)
	}

	// The provider roles follow any provider, Discord included: a role for every registered member.
	m.Providers["discord"] = other
	if d := rolesync.Desired(m, users); !d[12][other] {
		t.Errorf("a discord-mapped role is every registered member's: %v", d[12])
	}
}

func TestMappingFrom(t *testing.T) {
	if m, err := rolesync.MappingFrom(nil); err != nil || !m.Idle() {
		t.Errorf("no section: %+v %v", m, err)
	}
	if m, err := rolesync.MappingFrom(&hubv1.DiscordSettings{GuildId: guildID}); err != nil || !m.Idle() {
		t.Errorf("a guild and no roles: %+v %v", m, err)
	}
	if m, err := rolesync.MappingFrom(&hubv1.DiscordSettings{Roles: &hubv1.DiscordRoles{Linked: linked.String()}}); err != nil || !m.Idle() {
		t.Errorf("roles and no guild: %+v %v", m, err)
	}
	if m, err := rolesync.MappingFrom(mapping()); err != nil || m.Idle() || m.Guild.String() != guildID || m.Recognition[0].Count != 2 {
		t.Errorf("full: %+v %v", m, err)
	}
	if _, err := rolesync.MappingFrom(&hubv1.DiscordSettings{GuildId: "nope"}); err == nil || !strings.Contains(err.Error(), "guild_id") {
		t.Errorf("a bad id: %v", err)
	}
	if _, err := rolesync.MappingFrom(&hubv1.DiscordSettings{GuildId: guildID, Roles: &hubv1.DiscordRoles{Providers: map[string]string{"steam": "x"}}}); err == nil || !strings.Contains(err.Error(), "roles.providers.steam") {
		t.Errorf("a bad provider role: %v", err)
	}
	if _, err := rolesync.MappingFrom(&hubv1.DiscordSettings{GuildId: guildID, Recognition: []*hubv1.DiscordRecognition{{Role: founders.String(), Rule: "member_since", Count: 1}}}); err == nil || !strings.Contains(err.Error(), "member_since") {
		t.Errorf("an unknown rule is refused, not ignored: %v", err)
	}
}

// fakeDiscord is Discord's REST API for one guild: the member list and role changes, which it
// applies so a second pass sees the result.
type fakeDiscord struct {
	srv *httptest.Server

	mu        sync.Mutex
	members   map[snowflake.ID][]snowflake.ID // user → roles
	bots      map[snowflake.ID]bool
	calls     []string // "+user/role", "-user/role"
	lists     int
	forbidden map[snowflake.ID]bool // roles above the bot
	gone      map[snowflake.ID]bool // members who leave before their change
	listCode  int                   // non-zero: the member list fails with this Discord error code (403)
	failAll   bool                  // every role change answers 500
	limited   int                   // answer this many role changes with 429 first
}

func newFakeDiscord(t *testing.T) *fakeDiscord {
	t.Helper()
	f := &fakeDiscord{members: map[snowflake.ID][]snowflake.ID{}, bots: map[snowflake.ID]bool{}, forbidden: map[snowflake.ID]bool{}, gone: map[snowflake.ID]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/guilds/{guild}/members", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.PathValue("guild") != guildID {
			discordError(w, http.StatusNotFound, 10004, "Unknown Guild")
			return
		}
		if f.listCode != 0 {
			discordError(w, http.StatusForbidden, f.listCode, "Missing Access")
			return
		}
		f.lists++
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
		ids := slices.Sorted(func(yield func(snowflake.ID) bool) {
			for id := range f.members {
				if !yield(id) {
					return
				}
			}
		})
		out := []map[string]any{}
		for _, id := range ids {
			if uint64(id) <= after {
				continue
			}
			if len(out) == limit {
				break
			}
			roles := []string{}
			for _, r := range f.members[id] {
				roles = append(roles, r.String())
			}
			out = append(out, map[string]any{"user": map[string]any{"id": id.String(), "username": "u", "bot": f.bots[id]}, "roles": roles, "joined_at": "2026-10-09T12:00:00Z"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	change := func(add bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			u, _ := strconv.ParseUint(r.PathValue("user"), 10, 64)
			role, _ := strconv.ParseUint(r.PathValue("role"), 10, 64)
			uid, rid := snowflake.ID(u), snowflake.ID(role)
			switch {
			case f.limited > 0:
				f.limited--
				w.Header().Set("Retry-After", "0")
				w.Header().Set("X-RateLimit-Limit", "10")
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.Header().Set("X-RateLimit-Reset-After", "0.05")
				w.Header().Set("X-RateLimit-Bucket", "roles")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"message":"You are being rate limited.","retry_after":0.05,"global":false}`))
				return
			case f.failAll:
				discordError(w, http.StatusInternalServerError, 0, "boom")
				return
			case f.forbidden[rid]:
				discordError(w, http.StatusForbidden, 50013, "Missing Permissions")
				return
			case f.gone[uid]:
				discordError(w, http.StatusNotFound, 10007, "Unknown Member")
				return
			}
			sign := "-"
			if add {
				sign = "+"
				f.members[uid] = append(f.members[uid], rid)
			} else {
				f.members[uid] = slices.DeleteFunc(f.members[uid], func(x snowflake.ID) bool { return x == rid })
			}
			f.calls = append(f.calls, fmt.Sprintf("%s%d/%d", sign, uid, rid))
			w.WriteHeader(http.StatusNoContent)
		}
	}
	mux.HandleFunc("PUT /api/guilds/{guild}/members/{user}/roles/{role}", change(true))
	mux.HandleFunc("DELETE /api/guilds/{guild}/members/{user}/roles/{role}", change(false))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected Discord call %s %s", r.Method, r.URL)
		http.Error(w, "unexpected", http.StatusNotFound)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func discordError(w http.ResponseWriter, status, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": msg})
}

func (f *fakeDiscord) setMember(id uint64, roles ...snowflake.ID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members[snowflake.ID(id)] = roles
}

func (f *fakeDiscord) takeCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.calls
	f.calls = nil
	return c
}

func (f *fakeDiscord) roles(id uint64) []snowflake.ID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(slices.Values(f.members[snowflake.ID(id)]))
}

// fakeHub is the hub as the bot's app sees it: a token, the members, the identity log and the
// settings.
type fakeHub struct {
	hubv1connect.UnimplementedIdentityServiceHandler
	hubv1connect.UnimplementedOrganizationServiceHandler
	srv *httptest.Server

	mu       sync.Mutex
	users    []*hubv1.User
	events   []*hubv1.IdentityEvent
	discord  *hubv1.DiscordSettings
	settings int // GetOrganizationSettings calls: one per pass
	pageSize int // ListUsers pages actually served
	pages    int
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	f := &fakeHub{discord: mapping()}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"Bearer","expires_in":3600}`))
	})
	path, h := hubv1connect.NewIdentityServiceHandler(f)
	mux.Handle(path, h)
	path, h = hubv1connect.NewOrganizationServiceHandler(f)
	mux.Handle(path, h)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHub) GetOrganizationSettings(context.Context, *connect.Request[hubv1.GetOrganizationSettingsRequest]) (*connect.Response[hubv1.GetOrganizationSettingsResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settings++
	return connect.NewResponse(&hubv1.GetOrganizationSettingsResponse{Settings: &hubv1.OrganizationSettings{Version: 1, Discord: f.discord}}), nil
}

func (f *fakeHub) ListUsers(_ context.Context, req *connect.Request[hubv1.ListUsersRequest]) (*connect.Response[hubv1.ListUsersResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pages++
	size := int(req.Msg.GetPageSize())
	if f.pageSize > 0 {
		size = f.pageSize
	}
	start, _ := strconv.Atoi(req.Msg.GetPageToken())
	end := min(start+size, len(f.users))
	resp := &hubv1.ListUsersResponse{Users: f.users[start:end]}
	if end < len(f.users) {
		resp.NextPageToken = strconv.Itoa(end)
	}
	return connect.NewResponse(resp), nil
}

func (f *fakeHub) ListIdentityEvents(_ context.Context, req *connect.Request[hubv1.ListIdentityEventsRequest]) (*connect.Response[hubv1.ListIdentityEventsResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	resp := &hubv1.ListIdentityEventsResponse{NextAfterId: req.Msg.GetAfterId()}
	for _, e := range f.events {
		if e.GetId() > req.Msg.GetAfterId() && len(resp.Events) < int(req.Msg.GetLimit()) {
			resp.Events = append(resp.Events, e)
			resp.NextAfterId = e.GetId()
		}
	}
	if n := len(f.events); n > 0 {
		resp.HeadId = f.events[n-1].GetId()
	}
	return connect.NewResponse(resp), nil
}

func (f *fakeHub) setUsers(users ...*hubv1.User) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users = users
}

func (f *fakeHub) appendEvent(kind string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, &hubv1.IdentityEvent{Id: int64(len(f.events) + 1), Event: kind})
}

func (f *fakeHub) settingsCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.settings
}

type rig struct {
	discord *fakeDiscord
	hub     *fakeHub
	mod     *rolesync.Module
	rt      *bot.Runtime
}

func newRig(t *testing.T, cfg rolesync.Config) *rig {
	t.Helper()
	r := &rig{discord: newFakeDiscord(t), hub: newFakeHub(t)}
	hc, err := hubclient.New(hubclient.Config{URL: r.hub.srv.URL, ClientID: "gravel_bot", ClientSecret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bc := bot.Default()
	bc.Discord.ApplicationID = appID
	bc.Discord.PublicKey = hex.EncodeToString(pub)
	bc.Discord.Token = base64.RawStdEncoding.EncodeToString([]byte(appID)) + ".Xa1b2c.not-a-real-token-signature"
	bc.Discord.Gateway = false
	r.mod = rolesync.New(hc, cfg)
	r.rt, err = bot.New(bot.Options{Config: bc, Logger: quiet(), RESTURL: r.discord.srv.URL + "/api"}, r.mod)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.rt.Close(context.Background()) })
	return r
}

func (r *rig) metrics(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	r.rt.InternalHandler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func TestPass(t *testing.T) {
	r := newRig(t, rolesync.Config{Interval: time.Hour, PollInterval: time.Hour})
	ctx := context.Background()
	r.hub.setUsers(user("ana", "discord", "11", "steam", "7657"), user("ben", "discord", "12"), user("cy", "discord", "13", "steam", "7658"))
	r.discord.setMember(11, other)
	r.discord.setMember(12, linked)
	r.discord.setMember(13)
	r.discord.setMember(14, linked, founders) // not a hub member: loses linked, keeps founders
	r.discord.mu.Lock()
	r.discord.members[99] = []snowflake.ID{linked}
	r.discord.bots[99] = true
	r.discord.mu.Unlock()

	res, err := r.mod.Pass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Users != 3 || res.Members != 5 || res.Added != 6 || res.Removed != 2 || res.Forbidden != 0 || res.Gone != 0 {
		t.Errorf("first pass: %+v", res)
	}
	if got := r.discord.roles(11); !slices.Equal(got, []snowflake.ID{linked, steam, founders, other}) {
		t.Errorf("ana: %v", got)
	}
	if got := r.discord.roles(12); !slices.Equal(got, []snowflake.ID{founders}) {
		t.Errorf("ben: %v", got)
	}
	if got := r.discord.roles(14); !slices.Equal(got, []snowflake.ID{founders}) {
		t.Errorf("a stranger keeps recognition, loses the rest: %v", got)
	}
	if got := r.discord.roles(99); !slices.Equal(got, []snowflake.ID{linked}) {
		t.Errorf("the bot is untouched: %v", got)
	}
	r.discord.takeCalls()

	// Idempotent: nothing left to do.
	if res, err := r.mod.Pass(ctx); err != nil || res.Added+res.Removed != 0 {
		t.Errorf("second pass: %+v %v", res, err)
	}
	if calls := r.discord.takeCalls(); len(calls) != 0 {
		t.Errorf("a settled guild gets no calls: %v", calls)
	}
	if m := r.metrics(t); !strings.Contains(m, `gravel_bot_rolesync_changes_total{action="add",result="ok"} 6`) || !strings.Contains(m, `gravel_bot_rolesync_changes_total{action="remove",result="ok"} 2`) {
		t.Errorf("change metrics:\n%s", rolesyncLines(m))
	}
}

func TestPassSkipsWhatItMayNotDo(t *testing.T) {
	r := newRig(t, rolesync.Config{Interval: time.Hour, PollInterval: time.Hour})
	ctx := context.Background()
	r.hub.setUsers(user("ana", "discord", "11", "steam", "1"), user("ben", "discord", "12", "steam", "2"), user("cy", "discord", "13", "steam", "3"))
	for _, id := range []uint64{11, 12, 13} {
		r.discord.setMember(id)
	}
	r.discord.mu.Lock()
	r.discord.forbidden[founders] = true // above the bot
	r.discord.gone[12] = true            // leaves mid-pass
	r.discord.mu.Unlock()
	res, err := r.mod.Pass(ctx)
	if err != nil {
		t.Fatalf("forbidden and gone are not failures: %v", err)
	}
	// ana: linked + steam ok, founders forbidden; ben: linked + steam gone, founders forbidden
	// (Discord answers the permission first); cy: linked + steam (third, no founders).
	if res.Added != 4 || res.Forbidden != 2 || res.Gone != 2 {
		t.Errorf("result: %+v", res)
	}
	if m := r.metrics(t); !strings.Contains(m, `gravel_bot_rolesync_changes_total{action="add",result="forbidden"} 2`) || !strings.Contains(m, `gravel_bot_rolesync_changes_total{action="add",result="gone"} 2`) {
		t.Errorf("metrics:\n%s", rolesyncLines(m))
	}

	r.discord.mu.Lock()
	r.discord.failAll = true
	r.discord.members[13] = nil
	r.discord.mu.Unlock()
	if _, err := r.mod.Pass(ctx); err == nil || !strings.Contains(err.Error(), "add role") {
		t.Errorf("any other Discord failure ends the pass: %v", err)
	}

	r.discord.mu.Lock()
	r.discord.listCode = 50001
	r.discord.mu.Unlock()
	if _, err := r.mod.Pass(ctx); err == nil || !strings.Contains(err.Error(), "Server Members intent") {
		t.Errorf("a refused member list says what to check: %v", err)
	}
}

func TestPassResumesAfterARateLimit(t *testing.T) {
	r := newRig(t, rolesync.Config{Interval: time.Hour, PollInterval: time.Hour})
	r.hub.setUsers(user("ana", "discord", "11", "steam", "1"))
	r.discord.setMember(11)
	r.discord.mu.Lock()
	r.discord.limited = 2
	r.discord.mu.Unlock()
	res, err := r.mod.Pass(context.Background())
	if err != nil || res.Added != 3 {
		t.Fatalf("a 429 is waited out and retried, not a failure: %+v %v", res, err)
	}
	if got := r.discord.roles(11); !slices.Equal(got, []snowflake.ID{linked, steam, founders}) {
		t.Errorf("roles after the limit: %v", got)
	}
	r.discord.mu.Lock()
	defer r.discord.mu.Unlock()
	if r.discord.limited != 0 {
		t.Errorf("both 429s were served: %d left", r.discord.limited)
	}
}

func TestDryRun(t *testing.T) {
	r := newRig(t, rolesync.Config{Interval: time.Hour, PollInterval: time.Hour, DryRun: true})
	r.hub.setUsers(user("ana", "discord", "11", "steam", "1"))
	r.discord.setMember(11)
	r.discord.setMember(14, linked)
	res, err := r.mod.Pass(context.Background())
	if err != nil || res.Added != 3 || res.Removed != 1 {
		t.Errorf("a dry run counts what it would do: %+v %v", res, err)
	}
	if calls := r.discord.takeCalls(); len(calls) != 0 {
		t.Errorf("a dry run changes nothing: %v", calls)
	}
	if m := r.metrics(t); !strings.Contains(m, `gravel_bot_rolesync_changes_total{action="add",result="dry_run"} 3`) {
		t.Errorf("metrics:\n%s", rolesyncLines(m))
	}
}

func TestIdleWithoutAMapping(t *testing.T) {
	r := newRig(t, rolesync.Config{Interval: time.Hour, PollInterval: time.Hour})
	r.hub.discord = &hubv1.DiscordSettings{GuildId: guildID}
	r.discord.setMember(11, linked)
	res, err := r.mod.Pass(context.Background())
	if err != nil || !res.Idle {
		t.Errorf("no roles mapped: %+v %v", res, err)
	}
	r.discord.mu.Lock()
	lists := r.discord.lists
	r.discord.mu.Unlock()
	if lists != 0 || len(r.discord.takeCalls()) != 0 {
		t.Error("an idle pass does not touch Discord")
	}
}

func TestPaging(t *testing.T) {
	r := newRig(t, rolesync.Config{Interval: time.Hour, PollInterval: time.Hour})
	var users []*hubv1.User
	for i := range 1001 {
		id := 1_000_000 + i
		users = append(users, user(strconv.Itoa(i), "discord", strconv.Itoa(id), "steam", strconv.Itoa(i)))
		r.discord.setMember(uint64(id), linked, steam)
	}
	r.hub.setUsers(users...)
	r.hub.pageSize = 400
	res, err := r.mod.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Every member already has linked and steam; the first two earn founders.
	if res.Users != 1001 || res.Members != 1001 || res.Added != 2 || res.Removed != 0 {
		t.Errorf("result: %+v", res)
	}
	r.discord.mu.Lock()
	lists := r.discord.lists
	r.discord.mu.Unlock()
	if lists != 2 || r.hub.pages != 3 {
		t.Errorf("pages: %d member pages, %d user pages", lists, r.hub.pages)
	}
}

func TestRunFollowsTheIdentityLog(t *testing.T) {
	r := newRig(t, rolesync.Config{Interval: time.Hour, PollInterval: 10 * time.Millisecond})
	r.hub.appendEvent("registered") // history before the start: covered by the first pass
	r.hub.setUsers(user("ana", "discord", "11"))
	r.discord.setMember(11)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.mod.Run(ctx) }()

	waitFor(t, "the first pass", func() bool {
		return r.hub.settingsCalls() == 1 && slices.Equal(r.discord.roles(11), []snowflake.ID{founders})
	})
	// Polls of a quiet log, and a login, start no pass.
	r.hub.appendEvent("login")
	time.Sleep(100 * time.Millisecond)
	if n := r.hub.settingsCalls(); n != 1 {
		t.Errorf("a login started a pass: %d passes", n)
	}
	// A link does.
	r.hub.setUsers(user("ana", "discord", "11", "steam", "1"))
	r.hub.appendEvent("linked")
	waitFor(t, "the link's pass", func() bool { return slices.Equal(r.discord.roles(11), []snowflake.ID{linked, steam, founders}) })
	// A trigger (a member joining the guild) does too.
	before := r.hub.settingsCalls()
	r.mod.Trigger()
	waitFor(t, "the triggered pass", func() bool { return r.hub.settingsCalls() > before })

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returns nil on cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestRunSurvivesFailedPasses(t *testing.T) {
	r := newRig(t, rolesync.Config{Interval: 20 * time.Millisecond, PollInterval: 20 * time.Millisecond})
	r.discord.mu.Lock()
	r.discord.listCode = 50001
	r.discord.mu.Unlock()
	r.hub.setUsers(user("ana", "discord", "11"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.mod.Run(ctx) }()
	waitFor(t, "failed passes counted", func() bool {
		return strings.Contains(r.metrics(t), `gravel_bot_rolesync_passes_total{result="error"}`) && r.hub.settingsCalls() >= 2
	})
	r.discord.mu.Lock()
	r.discord.listCode = 0
	r.discord.members[11] = nil
	r.discord.mu.Unlock()
	waitFor(t, "a pass that recovers", func() bool { return slices.Equal(r.discord.roles(11), []snowflake.ID{founders}) })
	if m := r.metrics(t); !strings.Contains(m, `gravel_bot_rolesync_passes_total{result="ok"}`) || !strings.Contains(m, "gravel_bot_rolesync_last_success_timestamp_seconds") {
		t.Errorf("metrics:\n%s", rolesyncLines(m))
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run: %v", err)
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// rolesyncLines keeps the metrics output's role-sync lines, for a failure message.
func rolesyncLines(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, "rolesync") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
