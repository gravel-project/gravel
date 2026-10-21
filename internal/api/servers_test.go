package api_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/gravel-project/gravel/drivers"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/store"
)

const (
	endpoint = "http://203.0.113.10:7789"
	credFile = "/run/secrets/wd-rcon"
)

// withServer applies a manifest with one War Dogs server and an observation of it.
func (r *rig) withServer(t *testing.T) time.Time {
	t.Helper()
	m := servers.Manifest{
		Games: []servers.GameRef{{ID: "wardogs"}},
		Servers: []servers.Server{{ID: "wd-1", Name: "War Dogs #1", Game: "wardogs", Driver: "wardogs", Location: "slc",
			Endpoint: endpoint, CredentialFile: credFile, Trust: servers.TrustOfficial}},
	}
	if _, err := r.srvs.Apply(context.Background(), m, servers.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)
	r.obs.obs["wd-1"] = servers.Observation{
		ServerID: "wd-1", State: servers.StateOK, Reachable: true, ObservedAt: at, Build: "++Wardogs+Live-CL-509546",
		Capabilities: []string{drivers.CapKick, drivers.CapPlayers, drivers.CapStatus},
		Status: drivers.Status{Name: "War Dogs #1", Map: "Ozeti", Experiences: []string{"Madrid_KOTH_01"}, Lighting: "DayClear",
			Players: 2, MaxPlayers: 100, ScoreTick: 24, RotationIndex: 0, NextRotationIndex: 1, RotationMap: "Europe", NextRotationMap: "NorthAmerica",
			Join:  drivers.Join{Method: drivers.JoinByID, Value: "a968511e-a098-48ec-9bdb-909255478f15", Hint: "Paste it into Join by ID."},
			Teams: []drivers.TeamScore{{Name: "Valkyra", Color: "#FA503E", Score: 120}}},
		Players: []drivers.Player{
			{Name: "Linked", Identity: drivers.Identity{Provider: "steam", Subject: "76561190000000001"}, Team: "Valkyra", Kills: 3},
			{Name: "Stranger", Identity: drivers.Identity{Provider: "steam", Subject: "76561190000000002"}, Team: "Lonestar"},
		},
	}
	return at
}

// A server-less game enables with no servers and lists with its sources, no combat boards and no
// driver (gravel#2); a server for it is still refused.
func TestServerLessGameIsListed(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if _, err := r.srvs.Apply(ctx, servers.Manifest{Games: []servers.GameRef{{ID: "seaofthieves"}}}, servers.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	anon := hubv1connect.NewServerServiceClient(http.DefaultClient, r.srv.URL)
	games, err := anon.ListGames(ctx, connect.NewRequest(&hubv1.ListGamesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	g := games.Msg.GetGames()
	if len(g) != 1 || g[0].GetId() != "seaofthieves" || g[0].GetIdentityProvider() != "xbox" || !g[0].GetServerLess() || g[0].GetCombatBoards() ||
		!slices.Equal(g[0].GetSources(), []string{"first_party", "self_report"}) {
		t.Errorf("games = %v", g)
	}
	list, err := anon.ListServers(ctx, connect.NewRequest(&hubv1.ListServersRequest{}))
	if err != nil || len(list.Msg.GetServers()) != 0 {
		t.Errorf("servers = %v, %v", list.Msg.GetServers(), err)
	}
	_, err = r.srvs.Apply(ctx, servers.Manifest{Games: []servers.GameRef{{ID: "seaofthieves"}},
		Servers: []servers.Server{{ID: "sot-1", Name: "SoT", Game: "seaofthieves", Driver: "wardogs", Location: "slc", Endpoint: endpoint,
			CredentialFile: credFile, Trust: servers.TrustOfficial}}}, servers.ApplyOptions{})
	if err == nil || !strings.Contains(err.Error(), "Sea of Thieves has no servers") {
		t.Errorf("a server for a server-less game: %v", err)
	}
}

// A team's players are counted from the server's players at the same poll, by the team's key in any
// case (#159); unset when the hub can't tell.
func TestServerTeamCounts(t *testing.T) {
	r := newRig(t)
	r.withServer(t)
	anon := hubv1connect.NewServerServiceClient(http.DefaultClient, r.srv.URL)
	base := r.obs.obs["wd-1"]
	ct := []drivers.TeamScore{{Name: "CT"}, {Name: "T", Key: "TERRORIST"}}
	cases := []struct {
		name string
		edit func(o *servers.Observation)
		want map[string]int32 // team → players; a missing team is unset
	}{
		{"by name, in any case", func(o *servers.Observation) {
			o.Players = append(o.Players, drivers.Player{Name: "Shouty", Team: "VALKYRA"})
		}, map[string]int32{"Valkyra": 2}},
		{"by key", func(o *servers.Observation) {
			o.Status.Teams = ct
			o.Players = []drivers.Player{{Team: "CT"}, {Team: "TERRORIST"}, {Team: "TERRORIST"}, {Team: "Spectator"}}
		}, map[string]int32{"CT": 1, "T": 2}},
		{"nobody on counts zero", func(o *servers.Observation) {
			o.Status.Teams, o.Players = ct, nil
		}, map[string]int32{"CT": 0, "T": 0}},
		{"players without teams are unknown", func(o *servers.Observation) {
			o.Players = []drivers.Player{{Name: "A"}, {Name: "B"}}
		}, map[string]int32{}},
		{"players not read are unknown", func(o *servers.Observation) {
			o.Capabilities = []string{drivers.CapStatus}
			o.Players = nil
		}, map[string]int32{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := base
			o.Players = slices.Clone(base.Players)
			c.edit(&o)
			r.obs.obs["wd-1"] = o
			got, err := anon.GetServerStatus(context.Background(), connect.NewRequest(&hubv1.GetServerStatusRequest{ServerId: "wd-1"}))
			if err != nil {
				t.Fatal(err)
			}
			for _, team := range got.Msg.GetServer().GetStatus().GetTeams() {
				want, counted := c.want[team.GetName()]
				if (team.Players != nil) != counted || team.GetPlayers() != want {
					t.Errorf("%s: players %v, want %d (counted %v)", team.GetName(), team.Players, want, counted)
				}
			}
		})
	}
}

func TestServerReadsArePublicAndCarryNoControlDetails(t *testing.T) {
	r := newRig(t)
	at := r.withServer(t)
	anon := hubv1connect.NewServerServiceClient(http.DefaultClient, r.srv.URL)
	ctx := context.Background()

	games, err := anon.ListGames(ctx, connect.NewRequest(&hubv1.ListGamesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	g := games.Msg.GetGames()
	if len(g) != 1 || g[0].GetId() != "wardogs" || g[0].GetIdentityProvider() != "steam" || len(g[0].GetBands()) != 5 || g[0].GetNoPaidPerks() {
		t.Errorf("games = %v", g)
	}
	if !slices.Equal(g[0].GetSources(), []string{"server_adapter", "first_party"}) || !g[0].GetCombatBoards() || g[0].GetServerLess() {
		t.Errorf("War Dogs' sources = %v, combat boards %v, server-less %v", g[0].GetSources(), g[0].GetCombatBoards(), g[0].GetServerLess())
	}
	for _, b := range g[0].GetBands() {
		if (b.GetKey() == "ServerImageURL" && len(b.GetHosts()) != 4) || (b.GetKey() == "ServerName" && b.GetMaxLength() != 64) ||
			(b.GetKey() == "MaxReservedSlots" && (b.GetUnlessSet() != "DefaultReservedPlayerIds" || b.GetMax() != 0 || b.Max == nil)) {
			t.Errorf("band = %v", b)
		}
	}

	list, err := anon.ListServers(ctx, connect.NewRequest(&hubv1.ListServersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Msg.GetServers()) != 1 {
		t.Fatalf("servers = %v", list.Msg.GetServers())
	}
	got, err := anon.GetServerStatus(ctx, connect.NewRequest(&hubv1.GetServerStatusRequest{ServerId: "wd-1"}))
	if err != nil {
		t.Fatal(err)
	}
	s := got.Msg.GetServer()
	st := s.GetStatus()
	if s.GetName() != "War Dogs #1" || s.GetTrust() != "official" || s.GetLocation() != "slc" {
		t.Errorf("server = %v", s)
	}
	if st.GetState() != "ok" || !st.GetReachable() || !st.GetObservedAt().AsTime().Equal(at) || st.GetPlayers() != 2 || st.GetMaxPlayers() != 100 ||
		st.GetMap() != "Ozeti" || st.GetScoreTick() != 24 || st.GetNextRotationIndex() != 1 || st.GetRotationMap() != "Europe" || st.GetNextRotationMap() != "NorthAmerica" || st.GetTeams()[0].GetColor() != "#FA503E" || len(st.GetCapabilities()) != 3 ||
		st.GetJoin().GetMethod() != "id" || st.GetJoin().GetValue() != "a968511e-a098-48ec-9bdb-909255478f15" || st.GetJoin().GetHint() != "Paste it into Join by ID." {
		t.Errorf("status = %v", st)
	}
	for _, msg := range []proto.Message{list.Msg, got.Msg} {
		b, _ := protojson.Marshal(msg)
		for _, secret := range []string{endpoint, "203.0.113.10", credFile, "765611900"} {
			if strings.Contains(string(b), secret) {
				t.Errorf("a public answer carries %q: %s", secret, b)
			}
		}
	}

	if _, err := anon.GetServerStatus(ctx, connect.NewRequest(&hubv1.GetServerStatusRequest{ServerId: "nope"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("unknown server = %v", err)
	}
	if _, err := anon.GetServerStatus(ctx, connect.NewRequest(&hubv1.GetServerStatusRequest{})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("no id = %v", err)
	}
}

// A server the monitor has not polled reads as unknown, with no data.
func TestAnUnpolledServerIsUnknown(t *testing.T) {
	r := newRig(t)
	r.withServer(t)
	delete(r.obs.obs, "wd-1")
	got, err := hubv1connect.NewServerServiceClient(http.DefaultClient, r.srv.URL).GetServerStatus(context.Background(), connect.NewRequest(&hubv1.GetServerStatusRequest{ServerId: "wd-1"}))
	if err != nil {
		t.Fatal(err)
	}
	st := got.Msg.GetServer().GetStatus()
	if st.GetState() != servers.StateUnknown || st.GetReachable() || st.GetObservedAt() != nil || st.GetRotationIndex() != -1 || st.GetRotationMap() != "" || st.GetJoin() != nil {
		t.Errorf("status = %v", st)
	}
}

func TestListServerPlayersNeedsAMemberOrTheScope(t *testing.T) {
	r := newRig(t)
	at := r.withServer(t)
	ctx := context.Background()
	member := r.user(t, "Member", "111")
	if err := r.idSt.LinkIdentity(ctx, store.Identity{UserID: member.ID, Provider: "steam", Subject: "76561190000000001", DisplayName: "Linked",
		VerificationMethod: identity.MethodOpenID, VerifiedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	req := func() *connect.Request[hubv1.ListServerPlayersRequest] {
		return connect.NewRequest(&hubv1.ListServerPlayersRequest{ServerId: "wd-1"})
	}

	if _, err := hubv1connect.NewServerServiceClient(http.DefaultClient, r.srv.URL).ListServerPlayers(ctx, req()); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("anonymous = %v", err)
	}
	scopeless := hubv1connect.NewServerServiceClient(bearerClient(http.DefaultClient, r.bearer(t, "scopeless", "identity:read")), r.srv.URL)
	if _, err := scopeless.ListServerPlayers(ctx, req()); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("an app without servers:read = %v", err)
	}
	for name, c := range map[string]hubv1connect.ServerServiceClient{
		"member": hubv1connect.NewServerServiceClient(cookieClient(http.DefaultClient, r.login(t, member.ID)), r.srv.URL),
		"app":    hubv1connect.NewServerServiceClient(bearerClient(http.DefaultClient, r.bearer(t, "card", "servers:read")), r.srv.URL),
	} {
		got, err := c.ListServerPlayers(ctx, req())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		p := got.Msg.GetPlayers()
		if len(p) != 2 || !got.Msg.GetObservedAt().AsTime().Equal(at) {
			t.Fatalf("%s: players = %v", name, p)
		}
		if p[0].GetSubject() != "76561190000000001" || p[0].GetUserId() != member.ID.String() || p[0].GetKills() != 3 || p[1].GetUserId() != "" {
			t.Errorf("%s: resolved = %v", name, p)
		}
	}
	member2 := hubv1connect.NewServerServiceClient(cookieClient(http.DefaultClient, r.login(t, member.ID)), r.srv.URL)
	if _, err := member2.ListServerPlayers(ctx, connect.NewRequest(&hubv1.ListServerPlayersRequest{ServerId: "nope"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("unknown server = %v", err)
	}
	var ce *connect.Error
	if _, err := member2.ListServerPlayers(ctx, connect.NewRequest(&hubv1.ListServerPlayersRequest{})); !errors.As(err, &ce) || ce.Code() != connect.CodeInvalidArgument {
		t.Errorf("no id = %v", err)
	}
}

// A shared join password (vm.share_password, #180) reaches only an app whose token carries
// servers:read, and only while the server is up: never an anonymous caller, a member, an app
// without the scope or a member's token an app holds (ADR-0010 §1).
func TestSharedJoinPasswordOnlyForAnAppWithServersRead(t *testing.T) {
	r := newRig(t)
	r.withServer(t)
	r.pws.pw["wd-1"] = "hunter2"
	ctx := context.Background()
	member := r.user(t, "Member", "111")
	status := func(c hubv1connect.ServerServiceClient) (get, list string) {
		t.Helper()
		g, err := c.GetServerStatus(ctx, connect.NewRequest(&hubv1.GetServerStatusRequest{ServerId: "wd-1"}))
		if err != nil {
			t.Fatal(err)
		}
		l, err := c.ListServers(ctx, connect.NewRequest(&hubv1.ListServersRequest{}))
		if err != nil || len(l.Msg.GetServers()) != 1 {
			t.Fatalf("list = %v %v", l, err)
		}
		return g.Msg.GetServer().GetStatus().GetJoin().GetPassword(), l.Msg.GetServers()[0].GetStatus().GetJoin().GetPassword()
	}
	for name, c := range map[string]hubv1connect.ServerServiceClient{
		"anonymous":             hubv1connect.NewServerServiceClient(http.DefaultClient, r.srv.URL),
		"member":                hubv1connect.NewServerServiceClient(cookieClient(http.DefaultClient, r.login(t, member.ID)), r.srv.URL),
		"app without the scope": hubv1connect.NewServerServiceClient(bearerClient(http.DefaultClient, r.bearer(t, "scopeless", "identity:read")), r.srv.URL),
		"member's token":        hubv1connect.NewServerServiceClient(bearerClient(http.DefaultClient, r.memberToken(t, member.ID, []string{"servers:read"}, "servers:read")), r.srv.URL),
	} {
		if get, list := status(c); get != "" || list != "" {
			t.Errorf("%s got the password: %q %q", name, get, list)
		}
	}
	card := hubv1connect.NewServerServiceClient(bearerClient(http.DefaultClient, r.bearer(t, "card", "servers:read")), r.srv.URL)
	if get, list := status(card); get != "hunter2" || list != "hunter2" {
		t.Errorf("an app with servers:read: %q %q", get, list)
	}
	// Down, or sharing none: no password, though the join stays.
	o := r.obs.obs["wd-1"]
	o.State, o.Reachable = servers.StateUnreachable, false
	r.obs.obs["wd-1"] = o
	if get, _ := status(card); get != "" {
		t.Errorf("an unreachable server's password: %q", get)
	}
	o.State, o.Reachable = servers.StateOK, true
	r.obs.obs["wd-1"] = o
	delete(r.pws.pw, "wd-1")
	if get, _ := status(card); get != "" {
		t.Errorf("a server that shares none: %q", get)
	}
}
