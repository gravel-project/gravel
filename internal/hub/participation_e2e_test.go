package hub_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/apps"
	"github.com/gravel-project/gravel/internal/config"
	"github.com/gravel-project/gravel/internal/hub"
	"github.com/gravel-project/gravel/internal/org"
	"github.com/gravel-project/gravel/internal/store/storetest"
)

// gravel#19's "done when": a first-party app posts an attendance event and it appears on the
// member's profile and on the participation board, through the real middleware chain; it never
// reaches a combat board; and the app is rate-limited as an app.
func TestParticipationFromAnApp(t *testing.T) {
	cfg := testConfig(t)
	cfg.Stats.PseudonymKey = "a test pseudonym key, at least 32 bytes long"
	cfg.RateLimit.PerApp = config.Limit{RequestsPerMinute: 1, Burst: 3}
	st := storetest.Open(t)
	storetest.Reset(t, st)
	t.Cleanup(func() { storetest.Reset(t, st) })
	ctx := context.Background()
	h, err := hub.New(ctx, hub.Options{Config: cfg, Logger: quiet(), Version: "test", Providers: fakeProviders()})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	public := newH2CServer(h.Handler())
	defer public.Close()
	internal := httptest.NewServer(h.InternalHandler())
	defer internal.Close()
	o, err := st.GetBuiltinOrganization(ctx)
	if err != nil {
		t.Fatal(err)
	}
	asApp := func(name string, scopes ...string) hubv1connect.StatsServiceClient {
		t.Helper()
		svc := apps.New(st, o.ID, time.Hour, quiet())
		c, err := svc.Create(ctx, name, scopes)
		if err != nil {
			t.Fatal(err)
		}
		tok, err := svc.Issue(ctx, c.App.ClientID, c.Secret, nil)
		if err != nil {
			t.Fatal(err)
		}
		return hubv1connect.NewStatsServiceClient(&http.Client{Transport: bearerTransport{next: h2cClient().Transport, token: tok.Value}}, public.URL)
	}
	muster := asApp("muster", apps.ScopeParticipationWrite)

	// Jo is a member (Discord 1) who has not chosen to show their name.
	b := browser(t)
	cookie := loginAs(t, b, public.URL)
	asJo := hubv1connect.NewStatsServiceClient(&http.Client{Transport: cookieTransport{next: h2cClient().Transport, cookie: cookie}}, public.URL)
	attended := &hubv1.ParticipationEvent{Key: "muster-event-7/jo", Kind: "attendance", Identity: &hubv1.PlayerIdentity{Provider: "discord", Subject: "1"},
		Session: "muster-event-7", OccurredAt: timestamppb.New(time.Now().Add(-2 * time.Hour)), DurationSeconds: 5400}
	record := func(c hubv1connect.StatsServiceClient) (string, error) {
		resp, err := c.RecordParticipation(ctx, connect.NewRequest(&hubv1.RecordParticipationRequest{Events: []*hubv1.ParticipationEvent{attended}}))
		if err != nil {
			return "", err
		}
		return resp.Msg.GetResults()[0].GetOutcome(), nil
	}
	if got, err := record(muster); err != nil || got != "recorded" {
		t.Fatalf("the app's event: %q %v", got, err)
	}
	if got, err := record(muster); err != nil || got != "duplicate" {
		t.Errorf("sent again: %q %v", got, err)
	}
	if _, err := record(asJo); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a member recording participation: %v", err)
	}

	// On Jo's profile.
	p, err := asJo.GetMemberProfile(ctx, connect.NewRequest(&hubv1.GetMemberProfileRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if pp := p.Msg.GetParticipation(); len(pp) < 1 || pp[0].GetWindow() != "all" || pp[0].GetKind() != "attendance" || pp[0].GetEvents() != 1 || pp[0].GetSeconds() != 5400 {
		t.Errorf("Jo's participation: %v", pp)
	}

	// On the participation board, the owner's until stats are public, then anyone's, under a
	// pseudonym; and on no combat board.
	anon := hubv1connect.NewStatsServiceClient(h2cClient(), public.URL)
	if _, err := anon.GetParticipationBoard(ctx, connect.NewRequest(&hubv1.GetParticipationBoardRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("a private board, anonymously: %v", err)
	}
	if _, err := org.New(st, cfg.Claim.TokenTTL, quiet()).UpdateSettings(ctx, org.Settings{Stats: org.Stats{Public: true}}); err != nil {
		t.Fatal(err)
	}
	board, err := anon.GetParticipationBoard(ctx, connect.NewRequest(&hubv1.GetParticipationBoardRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	es := board.Msg.GetEntries()
	if board.Msg.GetKind() != "attendance" || len(board.Msg.GetKinds()) != 1 || len(es) != 1 || !es[0].GetPseudonymous() || es[0].GetSeconds() != 5400 ||
		len(strings.Fields(es[0].GetName())) != 2 || strings.Contains(es[0].GetName(), "Jo") {
		t.Errorf("the participation board: %v", board.Msg)
	}
	combat, err := anon.GetBoard(ctx, connect.NewRequest(&hubv1.GetBoardRequest{}))
	if err != nil || len(combat.Msg.GetEntries()) != 0 {
		t.Errorf("participation reached a combat board: %v %v", combat, err)
	}

	// The pages: the board, and Jo's profile with its Participation section.
	page := func(path string) string {
		t.Helper()
		resp, err := b.Get(public.URL + path) //nolint:noctx // a test
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
		return string(body)
	}
	if body := page("/participation"); !strings.Contains(body, es[0].GetName()) || !strings.Contains(body, "1h 30m") || !strings.Contains(body, "Attendance") {
		t.Errorf("/participation: %s", body)
	}
	if body := page("/members/" + p.Msg.GetUserId()); !strings.Contains(body, `id="participation-heading"`) || !strings.Contains(body, "Attendance") || !strings.Contains(body, "1h 30m") {
		t.Errorf("Jo's profile page: %s", body)
	}

	// Rate limits are per app: muster's third call fits its burst of three, the fourth does not;
	// another app has its own bucket.
	if _, err := record(muster); err != nil {
		t.Errorf("third call: %v", err)
	}
	if _, err := record(muster); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Errorf("fourth call: %v, want resource_exhausted", err)
	}
	if got, err := record(asApp("seeder", apps.ScopeParticipationWrite)); err != nil || got != "recorded" {
		t.Errorf("another app: %q %v", got, err)
	}

	resp, err := http.Get(internal.URL + "/metrics") //nolint:noctx // a test
	if err != nil {
		t.Fatal(err)
	}
	metrics, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	for _, want := range []string{
		`gravel_participation_events_total{kind="attendance",outcome="recorded"} 2`,
		`gravel_participation_events_total{kind="attendance",outcome="duplicate"} 2`,
		`gravel_rate_limited_total{scope="app"} 1`,
	} {
		if !strings.Contains(string(metrics), want) {
			t.Errorf("metrics lack %s", want)
		}
	}
}
