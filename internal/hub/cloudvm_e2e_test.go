package hub_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/gravel-project/gravel/cloud"
	"github.com/gravel-project/gravel/cloud/fake"
	"github.com/gravel-project/gravel/drivers"
	"github.com/gravel-project/gravel/drivers/cloudvm"
	"github.com/gravel-project/gravel/drivers/cloudvm/cloudvmtest"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/hub"
	"github.com/gravel-project/gravel/internal/servers"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

// A cloud-VM server in the manifest becomes a running host the hub made, its status and join
// address served, its log push accepted only from its host, and parked, unparked and resized from
// the API by the owner, every request in the audit log and on the host's transitions.
func TestHubCloudVMLifecycle(t *testing.T) {
	cfg := testConfig(t)
	cfg.Server.ClientIPHeader = "X-Test-Client" // the Tunnel's CF-Connecting-IP, in a test
	// The test polls the host's state; rate limits are not what it is about.
	cfg.RateLimit.PerIP.RequestsPerMinute, cfg.RateLimit.PerIP.Burst = 100000, 100000
	cfg.RateLimit.PerUser.RequestsPerMinute, cfg.RateLimit.PerUser.Burst = 100000, 100000
	cfg.Leases.Key = "0123456789abcdef0123456789abcdef"
	st := storetest.Open(t)
	storetest.Reset(t, st)
	t.Cleanup(func() { storetest.Reset(t, st) })
	ctx := context.Background()

	dir := t.TempDir()
	file := func(name, v string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	p := fake.New()
	remote := cloudvmtest.NewRemote()
	remote.BootAll()
	game := cloudvmtest.NewGame()
	game.Up(true)
	h, err := hub.New(ctx, hub.Options{Config: cfg, Logger: quiet(), Version: "test", Providers: fakeProviders(),
		CloudProviders: func(context.Context, cloudvm.Location) (cloud.Provider, error) { return p, nil },
		Remote:         remote,
		Egress:         func(context.Context) ([]string, error) { return []string{"203.0.113.4"}, nil },
		Games:          map[string]cloudvm.GameFactory{"cs2": game.Factory()},
	})
	if err != nil {
		t.Fatal(err)
	}
	o, err := st.GetBuiltinOrganization(ctx)
	if err != nil {
		t.Fatal(err)
	}
	m := servers.Manifest{
		Games: []servers.GameRef{{ID: "cs2"}},
		Locations: []servers.Location{{ID: "do-sfo3", Provider: "digitalocean", Region: "fake-1",
			TokenFile: file("token", "do-token"), SSHKeyFile: file("key", string(pem.EncodeToMemory(block)))}},
		Servers: []servers.Server{{ID: "cs2-1", Name: "HTG CS2", Game: "cs2", Driver: "cloudvm", Location: "do-sfo3",
			CredentialFile: file("rcon", "rcon-pw"), PollInterval: servers.Duration(5 * time.Second), Trust: servers.TrustOfficial,
			Feed: &servers.Feed{URL: "https://ingest.example.com", TokenFile: file("feed", "feed-token")},
			VM: &servers.VMSpec{Size: "dedicated-2", VolumeGiB: 100, Address: servers.AddressReserved,
				Lease: &servers.LeaseSpec{Pool: "pug", Settings: []string{"start_map"}}}}},
	}
	if _, err := servers.New(st, o.ID, hub.DriverNames(), quiet()).WithProviders(hub.CloudProviders()...).Apply(ctx, m, servers.ApplyOptions{CheckCredentials: true}); err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- h.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	<-h.Started()
	base := "http://" + h.PublicAddr().String()

	// The owner: log in, claim.
	c := browser(t)
	cookie := loginAs(t, c, base)
	owned := &http.Client{Transport: cookieTransport{next: http.DefaultTransport, cookie: cookie}}
	if _, err := hubv1connect.NewOrganizationServiceClient(owned, base).ClaimOwnership(ctx, connect.NewRequest(&hubv1.ClaimOwnershipRequest{Token: h.OwnerClaimToken()})); err != nil {
		t.Fatal(err)
	}
	hosts := hubv1connect.NewServerHostServiceClient(owned, base)
	status := hubv1connect.NewServerServiceClient(owned, base) // the owner's budget, not the address's

	waitHost := func(state string) *hubv1.ServerHost {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			got, err := hosts.GetServerHost(ctx, connect.NewRequest(&hubv1.GetServerHostRequest{ServerId: "cs2-1"}))
			if err == nil && got.Msg.GetHost().GetState() == state {
				return got.Msg.GetHost()
			}
			if time.Now().After(deadline) {
				t.Fatalf("the host never reached %s: %v %v", state, got, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	waitStatus := func(state string) *hubv1.Server {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			got, err := status.GetServerStatus(ctx, connect.NewRequest(&hubv1.GetServerStatusRequest{ServerId: "cs2-1"}))
			if err == nil && got.Msg.GetServer().GetStatus().GetState() == state {
				return got.Msg.GetServer()
			}
			if time.Now().After(deadline) {
				t.Fatalf("the status never reached %s: %v %v", state, got, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	host := waitHost(cloudvm.StateRunning)
	if host.GetSize() != "dedicated-2" || host.GetAddress() == "" || !host.GetAddressReserved() || host.GetHourlyPriceUsd() <= 0 || host.GetVolumeGib() != 100 || len(host.GetSizes()) != 1 {
		t.Errorf("host %v", host)
	}
	srv := waitStatus(servers.StateOK)
	if srv.GetDriver() != "cloudvm" || srv.GetStatus().GetJoin().GetValue() != host.GetAddress()+":27015" || srv.GetStatus().GetJoin().GetMethod() != drivers.JoinByAddress {
		t.Errorf("server %v", srv)
	}
	if env := remote.Env; len(env) != 1 {
		t.Errorf("env pushed to %d hosts", len(env))
	}

	// The log push: refused from anywhere but the host, stored from the host.
	insts, _ := p.Instances(ctx, cloud.Filter{Tag: cloudvm.Tag("cs2-1")})
	if len(insts) != 1 {
		t.Fatalf("instances %v", insts)
	}
	pushBody := func(from, body string) int {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/ingest/log/feed-token", strings.NewReader(body))
		req.Header.Set("X-Test-Client", from)
		req.Header.Set("X-Server-Instance-Token", "abc")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	push := func(from string) int {
		return pushBody(from, "10/12/2026 - 19:00:00.000 - World triggered \"Round_Start\"\n")
	}
	if code := push("198.51.100.9"); code != http.StatusUnauthorized {
		t.Errorf("a push from elsewhere: %d", code)
	}
	if code := push(insts[0].PublicIPv4); code != http.StatusOK {
		t.Errorf("a push from the host: %d", code)
	}

	// Proof in game (gravel#5): the owner's code, typed in the server's chat, links the Steam
	// account the log names, and the hub answers in chat.
	idc := hubv1connect.NewIdentityServiceClient(owned, base)
	pr, err := idc.StartProof(ctx, connect.NewRequest(&hubv1.StartProofRequest{ServerId: "cs2-1"}))
	if err != nil || pr.Msg.GetProvider() != "steam" {
		t.Fatalf("start a proof: %v %v", pr, err)
	}
	said := "chat line " + pr.Msg.GetCode()
	game.OnChat(said, cloudvm.ChatLine{Player: cloudvm.GamePlayer{UserID: 2, Name: "Owner in game",
		Identity: drivers.Identity{Provider: "steam", Subject: "76561197960287930"}}, Text: "!link " + pr.Msg.GetCode()})
	if code := pushBody(insts[0].PublicIPv4, said); code != http.StatusOK {
		t.Fatalf("the chat line's push: %d", code)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		got, err := idc.GetProof(ctx, connect.NewRequest(&hubv1.GetProofRequest{ProofId: pr.Msg.GetProofId()}))
		if err == nil && got.Msg.GetState() == "proven" && slices.ContainsFunc(game.Commands(), func(c string) bool { return strings.HasPrefix(c, "say Code accepted:") }) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never proven: %v %v, commands %v", got, err, game.Commands())
		}
	}
	// A bad code is answered, at most once a minute per server.
	for i := range 2 {
		bad := "bad code " + strconv.Itoa(i)
		game.OnChat(bad, cloudvm.ChatLine{Player: cloudvm.GamePlayer{UserID: 3, Identity: drivers.Identity{Provider: "steam", Subject: "76561197960287931"}}, Text: "!link 0000-0000"})
		if code := pushBody(insts[0].PublicIPv4, bad); code != http.StatusOK {
			t.Fatalf("a bad code's push: %d", code)
		}
	}
	unknown := func() int {
		n := 0
		for _, c := range game.Commands() {
			if strings.HasPrefix(c, "say That code is unknown") {
				n++
			}
		}
		return n
	}
	for deadline := time.Now().Add(5 * time.Second); unknown() == 0 && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
	}
	time.Sleep(200 * time.Millisecond) // the second answer, were it coming
	if n := unknown(); n != 1 {
		t.Errorf("%d answers to two bad codes, want 1: %v", n, game.Commands())
	}
	me, err := idc.GetMe(ctx, connect.NewRequest(&hubv1.GetMeRequest{}))
	if err != nil || !slices.ContainsFunc(me.Msg.GetUser().GetIdentities(), func(i *hubv1.Identity) bool {
		return i.GetProvider() == "steam" && i.GetSubject() == "76561197960287930" && i.GetVerificationMethod() == "game-chat"
	}) {
		t.Errorf("the owner's identities: %v %v", me, err)
	}

	// Anyone not signed in reads no host.
	if _, err := hubv1connect.NewServerHostServiceClient(http.DefaultClient, base).GetServerHost(ctx, connect.NewRequest(&hubv1.GetServerHostRequest{ServerId: "cs2-1"})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("anonymous: %v", err)
	}
	if _, err := hosts.DestroyServerHost(ctx, connect.NewRequest(&hubv1.DestroyServerHostRequest{ServerId: "cs2-1"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("destroying a running host: %v", err)
	}
	if _, err := hosts.GetServerHost(ctx, connect.NewRequest(&hubv1.GetServerHostRequest{ServerId: "nope"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("an unknown server: %v", err)
	}

	// Park, then unpark: the same address, a new instance.
	parked, err := hosts.SetServerHostDesired(ctx, connect.NewRequest(&hubv1.SetServerHostDesiredRequest{ServerId: "cs2-1", Desired: "parked", Reason: "test"}))
	if err != nil || parked.Msg.GetAuditId() == 0 {
		t.Fatalf("park: %v %v", parked, err)
	}
	waitHost(cloudvm.StateParked)
	waitStatus(servers.StateParked)
	if insts, _ := p.Instances(ctx, cloud.Filter{}); len(insts) != 0 {
		t.Errorf("a parked host keeps %d instances", len(insts))
	}
	// Parked, its volume can be copied into a snapshot a new server starts from (#166).
	snap, err := hosts.SnapshotServerHostVolume(ctx, connect.NewRequest(&hubv1.SnapshotServerHostVolumeRequest{ServerId: "cs2-1", Name: "cs2-install"}))
	if err != nil || snap.Msg.GetSnapshot().GetName() != "cs2-install" || snap.Msg.GetSnapshot().GetRegion() == "" || snap.Msg.GetAuditId() == 0 {
		t.Fatalf("snapshot: %v %v", snap, err)
	}
	if got, err := p.FindVolumeSnapshot(ctx, "cs2-install", snap.Msg.GetSnapshot().GetRegion()); err != nil || got.ID != snap.Msg.GetSnapshot().GetId() {
		t.Errorf("in the cloud: %+v %v", got, err)
	}
	if _, err := hosts.SetServerHostDesired(ctx, connect.NewRequest(&hubv1.SetServerHostDesiredRequest{ServerId: "cs2-1", Desired: "running"})); err != nil {
		t.Fatal(err)
	}
	again := waitHost(cloudvm.StateRunning)
	if _, err := hosts.SnapshotServerHostVolume(ctx, connect.NewRequest(&hubv1.SnapshotServerHostVolumeRequest{ServerId: "cs2-1"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("a snapshot of a running host's volume: %v", err)
	}
	if again.GetAddress() != host.GetAddress() {
		t.Errorf("the address changed across a park: %s → %s", host.GetAddress(), again.GetAddress())
	}

	// Resize: the owner, to the size the host lists.
	if _, err := hosts.ResizeServerHost(ctx, connect.NewRequest(&hubv1.ResizeServerHostRequest{ServerId: "cs2-1", Size: "medium"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a size of another family: %v", err)
	}
	if _, err := hosts.ResizeServerHost(ctx, connect.NewRequest(&hubv1.ResizeServerHostRequest{ServerId: "cs2-1", Size: "dedicated-4", Force: true})); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		got, err := hosts.GetServerHost(ctx, connect.NewRequest(&hubv1.GetServerHostRequest{ServerId: "cs2-1"}))
		if err == nil && got.Msg.GetHost().GetState() == cloudvm.StateRunning && got.Msg.GetHost().GetSize() == "dedicated-4" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never resized: %v %v", got, err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Pin the size it is at (#37), then unpin it; a second unpin has nothing to undo.
	pinned, err := hosts.ResizeServerHost(ctx, connect.NewRequest(&hubv1.ResizeServerHostRequest{ServerId: "cs2-1", Size: "dedicated-4", Pin: true}))
	if err != nil || pinned.Msg.GetHost().GetPinnedSize() != "dedicated-4" || pinned.Msg.GetHost().GetTargetSize() != "" {
		t.Fatalf("pin: %v %v", pinned, err)
	}
	unpinned, err := hosts.UnpinServerHostSize(ctx, connect.NewRequest(&hubv1.UnpinServerHostSizeRequest{ServerId: "cs2-1"}))
	if err != nil || unpinned.Msg.GetHost().GetPinnedSize() != "" {
		t.Fatalf("unpin: %v %v", unpinned, err)
	}
	if _, err := hosts.UnpinServerHostSize(ctx, connect.NewRequest(&hubv1.UnpinServerHostSizeRequest{ServerId: "cs2-1"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("unpin with nothing pinned: %v", err)
	}
	// An expected population (#172): set, read back, cleared; one in the past is refused.
	until := time.Now().Add(time.Hour).Truncate(time.Second)
	exp, err := hosts.SetServerHostExpectation(ctx, connect.NewRequest(&hubv1.SetServerHostExpectationRequest{ServerId: "cs2-1", Players: 10, Until: timestamppb.New(until), Reason: "PUG 3 filled"}))
	if err != nil || exp.Msg.GetHost().GetExpectedPlayers() != 10 || !exp.Msg.GetHost().GetExpectedUntil().AsTime().Equal(until) || exp.Msg.GetHost().GetExpectedReason() != "PUG 3 filled" {
		t.Fatalf("expect: %v %v", exp, err)
	}
	if got, err := hosts.SetServerHostExpectation(ctx, connect.NewRequest(&hubv1.SetServerHostExpectationRequest{ServerId: "cs2-1"})); err != nil || got.Msg.GetHost().GetExpectedPlayers() != 0 || got.Msg.GetHost().GetExpectedUntil() != nil {
		t.Fatalf("clear: %v %v", got, err)
	}
	if _, err := hosts.SetServerHostExpectation(ctx, connect.NewRequest(&hubv1.SetServerHostExpectationRequest{ServerId: "cs2-1", Players: 10, Until: timestamppb.New(time.Now().Add(-time.Minute))})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("an expectation in the past: %v", err)
	}
	// The month's cost comes from the events, each run at its own size's price, on Postgres.
	if c := unpinned.Msg.GetHost().GetMonthCostUsd(); c <= 0 {
		t.Errorf("month cost %v after running at two sizes", c)
	}

	ev, err := hosts.ListServerHostEvents(ctx, connect.NewRequest(&hubv1.ListServerHostEventsRequest{ServerId: "cs2-1", PageSize: 3}))
	if err != nil || len(ev.Msg.GetEvents()) != 3 || ev.Msg.GetNextPageToken() == "" {
		t.Fatalf("events %v %v", ev, err)
	}
	all, err := hosts.ListServerHostEvents(ctx, connect.NewRequest(&hubv1.ListServerHostEventsRequest{ServerId: "cs2-1"}))
	if err != nil {
		t.Fatal(err)
	}
	linked := false
	for _, e := range all.Msg.GetEvents() {
		linked = linked || (e.GetToState() == cloudvm.StateParking && e.GetAuditId() == parked.Msg.GetAuditId())
	}
	if !linked {
		t.Errorf("the park request is not on its transition: %v", all.Msg.GetEvents())
	}
	audit, err := hubv1connect.NewModerationServiceClient(owned, base).ListAuditLog(ctx, connect.NewRequest(&hubv1.ListAuditLogRequest{ServerId: "cs2-1"}))
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, e := range audit.Msg.GetEntries() {
		actions = append(actions, e.GetAction()+":"+e.GetOutcome())
	}
	if strings.Join(actions, ",") != "host_expect:invalid,host_expect:accepted,host_expect:accepted,host_unpin:invalid,host_unpin:accepted,host_resize:accepted,host_resize:accepted,host_resize:invalid,host_snapshot:not_parked,host_unpark:accepted,host_snapshot:accepted,host_park:accepted,host_destroy:not_parked" {
		t.Errorf("audit %v", actions)
	}

	// A lease (#20): the owner takes the pool's server for a match, watches it get ready over the
	// stream, and gives it back; the server is parked, and its own password is its own again.
	if _, err := hosts.SetServerHostDesired(ctx, connect.NewRequest(&hubv1.SetServerHostDesiredRequest{ServerId: "cs2-1", Desired: "parked"})); err != nil {
		t.Fatal(err)
	}
	waitHost(cloudvm.StateParked)
	leases := hubv1connect.NewServerLeaseServiceClient(owned, base)
	got, err := leases.RequestServerLease(ctx, connect.NewRequest(&hubv1.RequestServerLeaseRequest{Pool: "pug", HolderRef: "pug-1",
		ExpectedPlayers: 10, Settings: map[string]string{"start_map": "de_nuke"}, Reason: "PUG 1"}))
	if err != nil || got.Msg.GetLease().GetState() != "starting" || len(got.Msg.GetJoin().GetPassword()) != 10 {
		t.Fatalf("request: %v %v", got, err)
	}
	leaseID := got.Msg.GetLease().GetId()
	watchCtx, stopWatch := context.WithTimeout(ctx, 60*time.Second)
	defer stopWatch()
	stream, err := leases.WatchServerLease(watchCtx, connect.NewRequest(&hubv1.WatchServerLeaseRequest{LeaseId: leaseID}))
	if err != nil {
		t.Fatal(err)
	}
	var states []string
	for stream.Receive() {
		if l := stream.Msg().GetLease(); l != nil {
			states = append(states, l.GetState())
			if l.GetState() == "ready" {
				break
			}
		}
	}
	_ = stream.Close()
	if len(states) == 0 || states[len(states)-1] != "ready" {
		t.Fatalf("watched states %v (%v)", states, stream.Err())
	}
	waitHost(cloudvm.StateRunning)
	running, _ := p.Instances(ctx, cloud.Filter{Tag: cloudvm.Tag("cs2-1")})
	if len(running) != 1 {
		t.Fatalf("instances %v", running)
	}
	if env, _ := remote.EnvOn(running[0].PublicIPv4); !strings.Contains(env, "CS2_PW="+got.Msg.GetJoin().GetPassword()+"\n") ||
		!strings.Contains(env, "CS2_STARTMAP=de_nuke\n") {
		t.Errorf("the leased server's env:\n%s", env)
	}
	if again, err := leases.RequestServerLease(ctx, connect.NewRequest(&hubv1.RequestServerLeaseRequest{Pool: "pug", HolderRef: "pug-2"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("a second lease of a pool of one: %v %v", again, err)
	}
	released, err := leases.ReleaseServerLease(ctx, connect.NewRequest(&hubv1.ReleaseServerLeaseRequest{LeaseId: leaseID, Reason: "match over"}))
	if err != nil || released.Msg.GetLease().GetState() != "released" {
		t.Fatalf("release: %v %v", released, err)
	}
	waitHost(cloudvm.StateParked)
	list, err := leases.ListServerLeases(ctx, connect.NewRequest(&hubv1.ListServerLeasesRequest{ServerId: "cs2-1"}))
	if err != nil || len(list.Msg.GetLeases()) != 1 || list.Msg.GetLeases()[0].GetReason() != "released by its holder: match over" {
		t.Errorf("list: %v %v", list, err)
	}

	resp, err := http.Get("http://" + h.InternalAddr().String() + "/metrics") //nolint:noctx // a test
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	for _, want := range []string{
		`gravel_cloudvm_host_state{server="cs2-1",state="parked"} 1`, // a lease's release parks it
		`gravel_cloudvm_operations_total{op="park",result="ok",server="cs2-1"}`,
		`gravel_leases_total{pool="pug",result="requested"} 1`,
		`gravel_leases_total{pool="pug",result="released"} 1`,
		`gravel_ingest_batches_total{result="stored",server="cs2-1",source="cs2"} 4`, // a round start and three chat lines
		`gravel_identity_proofs_total{channel="game-chat",result="started"} 1`,
		`gravel_identity_proofs_total{channel="game-chat",result="linked"} 1`,
		`gravel_identity_proofs_total{channel="game-chat",result="bad_code"} 2`,
		`gravel_ingest_batches_total{result="address_mismatch",server="cs2-1",source="cs2"} 1`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics lack %s", want)
		}
	}
}
