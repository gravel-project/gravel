package api_test

import (
	"context"
	"errors"
	"net/http"
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
			Endpoint: endpoint, CredentialFile: credFile, Trust: servers.TrustOfficial,
			Seeding: &servers.Seeding{Threshold: 20, Hours: "17:00-23:00", Timezone: "America/Chicago"}}},
	}
	if _, err := r.srvs.Apply(context.Background(), m, servers.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)
	r.obs.obs["wd-1"] = servers.Observation{
		ServerID: "wd-1", State: servers.StateOK, Reachable: true, ObservedAt: at, Build: "++Wardogs+Live-CL-509546",
		Capabilities: []string{drivers.CapKick, drivers.CapPlayers, drivers.CapStatus},
		Status: drivers.Status{Name: "War Dogs #1", Map: "Ozeti", Experiences: []string{"Madrid_KOTH_01"}, Lighting: "DayClear",
			Players: 2, MaxPlayers: 100, ScoreTick: 24, RotationIndex: 0, NextRotationIndex: 1,
			Teams: []drivers.TeamScore{{Name: "Valkyra", Color: "#FA503E", Score: 120}}},
		Players: []drivers.Player{
			{Name: "Linked", Identity: drivers.Identity{Provider: "steam", Subject: "76561190000000001"}, Team: "Valkyra", Kills: 3},
			{Name: "Stranger", Identity: drivers.Identity{Provider: "steam", Subject: "76561190000000002"}, Team: "Lonestar"},
		},
	}
	return at
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
	if s.GetName() != "War Dogs #1" || s.GetTrust() != "official" || s.GetSeeding().GetThreshold() != 20 || s.GetSeeding().GetCooldown().AsDuration() != time.Hour {
		t.Errorf("server = %v", s)
	}
	if st.GetState() != "ok" || !st.GetReachable() || !st.GetObservedAt().AsTime().Equal(at) || st.GetPlayers() != 2 || st.GetMaxPlayers() != 100 ||
		st.GetMap() != "Ozeti" || st.GetScoreTick() != 24 || st.GetNextRotationIndex() != 1 || st.GetTeams()[0].GetColor() != "#FA503E" || len(st.GetCapabilities()) != 3 {
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
	if st.GetState() != servers.StateUnknown || st.GetReachable() || st.GetObservedAt() != nil || st.GetRotationIndex() != -1 {
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
