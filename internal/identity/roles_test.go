package identity_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/identity/identitytest"
	"github.com/gravel-project/gravel/internal/store"
)

type rolesFixture struct {
	svc     *identity.Service
	st      *identitytest.FakeStore
	discord *identitytest.FakePublisher
	steam   *identitytest.FakeProvider
}

func newRolesFixture(t *testing.T, discordLogin bool) *rolesFixture {
	t.Helper()
	f := &rolesFixture{
		st: identitytest.NewFakeStore(),
		discord: &identitytest.FakePublisher{FakeProvider: identitytest.FakeProvider{ProviderName: "discord", Accounts: map[string]identity.Account{
			"jo": {Subject: "1", DisplayName: "Jo", Method: identity.MethodOAuth2},
		}}},
		steam: &identitytest.FakeProvider{ProviderName: "steam", Accounts: map[string]identity.Account{
			"jo-steam": {Subject: "76561198000000001", DisplayName: "JoOnSteam", Method: identity.MethodOpenID},
		}},
	}
	now := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	f.svc = identity.New(f.st, orgID, []identity.Registration{{Provider: f.discord, Login: discordLogin}, {Provider: f.steam}}, 10*time.Minute,
		slog.New(slog.NewTextHandler(io.Discard, nil)), identity.WithClock(func() time.Time { return now }), identity.WithOrganizationName("Hidden Token Gaming"))
	return f
}

// verifyJo runs the roles flow for Jo's Discord account.
func (f *rolesFixture) verifyJo(t *testing.T) (identity.Completed, error) {
	t.Helper()
	ctx := context.Background()
	b, err := f.svc.Begin(ctx, "discord", identity.IntentRoles, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.AuthURL, "publish=1") {
		t.Errorf("the roles flow begins as a publish: %s", b.AuthURL)
	}
	return f.svc.Complete(ctx, b.AttemptToken, "discord", identitytest.CallbackParams("jo", f.discord.LastState()), nil)
}

func TestRolesRegistersLogsInAndPublishes(t *testing.T) {
	f := newRolesFixture(t, true)
	ctx := context.Background()

	// A first visit registers the member, like a login, and publishes Discord alone.
	done, err := f.verifyJo(t)
	if err != nil || done.Intent != identity.IntentRoles || !done.Registered || done.User.DisplayName != "Jo" || done.PublishErr != nil || done.Published == nil {
		t.Fatalf("first: %v %+v", err, done)
	}
	want := identity.Profile{Organization: "Hidden Token Gaming", DisplayName: "Jo", Providers: []string{"discord"}}
	if got := f.discord.Published(); len(got) != 1 || !profileEqual(got[0], want) {
		t.Errorf("published %+v", got)
	}

	// After a Steam link, the next verification publishes it.
	link, err := f.svc.Begin(ctx, "steam", identity.IntentLink, &done.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Complete(ctx, link.AttemptToken, "steam", identitytest.CallbackParams("jo-steam", f.steam.LastState()), &done.User.ID); err != nil {
		t.Fatal(err)
	}
	again, err := f.verifyJo(t)
	if err != nil || again.Registered || again.User.ID != done.User.ID {
		t.Fatalf("second: %v %+v", err, again)
	}
	got := f.discord.Published()
	if len(got) != 2 || !slices.Equal(got[1].Providers, []string{"discord", "steam"}) {
		t.Errorf("after the link: %+v", got)
	}
	events, _, _ := f.svc.ListEvents(ctx, 0, 100)
	if events[len(events)-1].Event != store.IdentityEventLogin {
		t.Errorf("a verification records a login: %+v", events[len(events)-1])
	}
}

func TestRolesPublishFailureKeepsTheLogin(t *testing.T) {
	f := newRolesFixture(t, true)
	f.discord.PublishErr = errors.New("discord says no")
	done, err := f.verifyJo(t)
	if err != nil || !done.Registered || done.Published != nil || !errors.Is(done.PublishErr, identity.ErrProviderFailed) {
		t.Fatalf("a failed publish: %v %+v", err, done)
	}
	f.st.ErrOn = "ListIdentities"
	f.discord.PublishErr = nil
	done, err = f.verifyJo(t)
	if err != nil || done.Published != nil || done.PublishErr == nil {
		t.Errorf("a store failure before publishing: %v %+v", err, done)
	}
}

func TestRolesRefusals(t *testing.T) {
	ctx := context.Background()
	f := newRolesFixture(t, false)
	if _, err := f.svc.Begin(ctx, "discord", identity.IntentRoles, nil); !errors.Is(err, identity.ErrLoginNotAllowed) {
		t.Errorf("a link-only Discord cannot verify: %v", err)
	}
	f = newRolesFixture(t, true)
	if _, err := f.svc.Begin(ctx, "steam", identity.IntentRoles, nil); !errors.Is(err, identity.ErrNotPublisher) {
		t.Errorf("Steam publishes nothing: %v", err)
	}
	if _, err := f.svc.Begin(ctx, "nope", identity.IntentRoles, nil); !errors.Is(err, identity.ErrUnknownProvider) {
		t.Errorf("unknown provider: %v", err)
	}
	// A login attempt completed through the roles callback publishes nothing: the attempt's
	// intent decides, not the route.
	b, err := f.svc.Begin(ctx, "discord", identity.IntentLogin, nil)
	if err != nil {
		t.Fatal(err)
	}
	done, err := f.svc.Complete(ctx, b.AttemptToken, "discord", identitytest.CallbackParams("jo", f.discord.LastState()), nil)
	if err != nil || done.Intent != identity.IntentLogin || len(f.discord.Published()) != 0 {
		t.Errorf("login: %v %+v %v", err, done, f.discord.Published())
	}
	// A denial at Discord fails the attempt like a login's.
	b, _ = f.svc.Begin(ctx, "discord", identity.IntentRoles, nil)
	params := identitytest.CallbackParams("jo", f.discord.LastState())
	params.Set("error", "access_denied")
	if _, err := f.svc.Complete(ctx, b.AttemptToken, "discord", params, nil); !errors.Is(err, identity.ErrProviderDenied) {
		t.Errorf("denied: %v", err)
	}
}

func profileEqual(a, b identity.Profile) bool {
	return a.Organization == b.Organization && a.DisplayName == b.DisplayName && slices.Equal(a.Providers, b.Providers)
}
