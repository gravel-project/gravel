package identity_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/identity/identitytest"
	"github.com/gravel-project/gravel/internal/store"
)

var orgID = uuid.MustParse("00000000-0000-7000-8000-00000000000a")

type fixture struct {
	svc     *identity.Service
	st      *identitytest.FakeStore
	discord *identitytest.FakeProvider
	steam   *identitytest.FakeProvider
	now     *time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	now := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	f := &fixture{
		st: identitytest.NewFakeStore(),
		discord: &identitytest.FakeProvider{ProviderName: "discord", Accounts: map[string]identity.Account{
			"jo":  {Subject: "1", DisplayName: "Jo", AvatarURL: "https://a/jo", Method: identity.MethodOAuth2},
			"sam": {Subject: "2", DisplayName: "Sam", Method: identity.MethodOAuth2},
		}},
		steam: &identitytest.FakeProvider{ProviderName: "steam", Accounts: map[string]identity.Account{
			"jo-steam": {Subject: "76561198000000001", DisplayName: "JoOnSteam", Method: identity.MethodOpenID},
		}},
		now: &now,
	}
	f.svc = f.service(f.st)
	return f
}

func (f *fixture) service(st identity.Store) *identity.Service {
	return identity.New(st, orgID, []identity.Registration{{Provider: f.discord, Login: true}, {Provider: f.steam, Login: false}}, 10*time.Minute,
		slog.New(slog.NewTextHandler(io.Discard, nil)), identity.WithClock(func() time.Time { return *f.now }))
}

func (f *fixture) login(t *testing.T, code string) identity.Completed {
	t.Helper()
	b, err := f.svc.Begin(context.Background(), "discord", identity.IntentLogin, nil)
	if err != nil {
		t.Fatal(err)
	}
	done, err := f.svc.Complete(context.Background(), b.AttemptToken, "discord", identitytest.CallbackParams(code, f.discord.LastState()), nil)
	if err != nil {
		t.Fatal(err)
	}
	return done
}

func TestLoginRegistersThenLogsIn(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	b, err := f.svc.Begin(ctx, "discord", identity.IntentLogin, nil)
	if err != nil {
		t.Fatal(err)
	}
	state := f.discord.LastState()
	if state == "" || !strings.Contains(b.AuthURL, url.QueryEscape(state)) || b.AttemptToken == "" || !b.ExpiresAt.Equal(f.now.Add(10*time.Minute)) {
		t.Fatalf("begun: %+v (state %q)", b, state)
	}
	done, err := f.svc.Complete(ctx, b.AttemptToken, "discord", identitytest.CallbackParams("jo", state), nil)
	if err != nil || !done.Registered || done.Intent != identity.IntentLogin || done.User.DisplayName != "Jo" || done.User.OrganizationID != orgID {
		t.Fatalf("first login: %v %+v", err, done)
	}
	if done.Identity.Provider != "discord" || done.Identity.Subject != "1" || done.Identity.VerificationMethod != identity.MethodOAuth2 || done.Identity.UserID != done.User.ID {
		t.Errorf("identity: %+v", done.Identity)
	}

	f.discord.Accounts["jo"] = identity.Account{Subject: "1", DisplayName: "Jo!", AvatarURL: "https://a/jo2", Method: identity.MethodOAuth2}
	*f.now = f.now.Add(time.Hour)
	again := f.login(t, "jo")
	if again.Registered || again.User.ID != done.User.ID || again.Identity.DisplayName != "Jo!" || again.Identity.AvatarURL != "https://a/jo2" || !again.User.LastLoginAt.Equal(*f.now) {
		t.Errorf("second login: %+v %+v", again.User, again.Identity)
	}
	me, err := f.svc.Me(ctx, done.User.ID)
	if err != nil || len(me.Identities) != 1 || me.DisplayName != "Jo" {
		t.Errorf("me: %v %+v", err, me)
	}
	var events []string
	for _, e := range f.st.Events {
		events = append(events, e.Event)
	}
	if strings.Join(events, ",") != "registered,login" {
		t.Errorf("events: %v", events)
	}
}

func TestStateMismatchConsumesTheAttempt(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	b, _ := f.svc.Begin(ctx, "discord", identity.IntentLogin, nil)
	if _, err := f.svc.Complete(ctx, b.AttemptToken, "discord", identitytest.CallbackParams("jo", "forged"), nil); !errors.Is(err, identity.ErrStateMismatch) {
		t.Fatalf("forged state: %v", err)
	}
	if _, err := f.svc.Complete(ctx, b.AttemptToken, "discord", url.Values{"code": {"jo"}}, nil); !errors.Is(err, identity.ErrAttemptInvalid) {
		t.Errorf("the attempt is gone after a mismatch: %v", err)
	}
	if len(f.st.Users) != 0 {
		t.Error("no user may be created from a mismatched callback")
	}
}

func TestCallbackReplayAndExpiry(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	b, _ := f.svc.Begin(ctx, "discord", identity.IntentLogin, nil)
	params := identitytest.CallbackParams("jo", f.discord.LastState())
	if _, err := f.svc.Complete(ctx, b.AttemptToken, "discord", params, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Complete(ctx, b.AttemptToken, "discord", params, nil); !errors.Is(err, identity.ErrAttemptInvalid) {
		t.Errorf("replay: %v", err)
	}
	if _, err := f.svc.Complete(ctx, "", "discord", params, nil); !errors.Is(err, identity.ErrAttemptInvalid) {
		t.Errorf("no cookie: %v", err)
	}
	if _, err := f.svc.Complete(ctx, "never-issued", "discord", params, nil); !errors.Is(err, identity.ErrAttemptInvalid) {
		t.Errorf("unknown token: %v", err)
	}

	b2, _ := f.svc.Begin(ctx, "discord", identity.IntentLogin, nil)
	*f.now = f.now.Add(11 * time.Minute)
	if _, err := f.svc.Complete(ctx, b2.AttemptToken, "discord", identitytest.CallbackParams("jo", f.discord.LastState()), nil); !errors.Is(err, identity.ErrAttemptInvalid) {
		t.Errorf("expired: %v", err)
	}

	b3, _ := f.svc.Begin(ctx, "discord", identity.IntentLogin, nil)
	if _, err := f.svc.Complete(ctx, b3.AttemptToken, "steam", identitytest.CallbackParams("jo", f.discord.LastState()), nil); !errors.Is(err, identity.ErrAttemptInvalid) {
		t.Errorf("callback at another provider: %v", err)
	}
}

func TestProviderRules(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Begin(ctx, "steam", identity.IntentLogin, nil); !errors.Is(err, identity.ErrLoginNotAllowed) {
		t.Errorf("steam is link-only: %v", err)
	}
	if _, err := f.svc.Begin(ctx, "xbox", identity.IntentLogin, nil); !errors.Is(err, identity.ErrUnknownProvider) {
		t.Errorf("unknown: %v", err)
	}
	if _, err := f.svc.Begin(ctx, "steam", identity.IntentLink, nil); err == nil {
		t.Error("a link needs a user")
	}
	b, _ := f.svc.Begin(ctx, "discord", identity.IntentLogin, nil)
	if _, err := f.svc.Complete(ctx, b.AttemptToken, "discord", url.Values{"error": {"access_denied"}, "state": {f.discord.LastState()}}, nil); !errors.Is(err, identity.ErrProviderDenied) {
		t.Errorf("denied: %v", err)
	}
	b, _ = f.svc.Begin(ctx, "discord", identity.IntentLogin, nil)
	if _, err := f.svc.Complete(ctx, b.AttemptToken, "discord", identitytest.CallbackParams("unknown-code", f.discord.LastState()), nil); !errors.Is(err, identity.ErrProviderFailed) {
		t.Errorf("provider failure: %v", err)
	}
	f.discord.BeginErr = errors.New("discord is down")
	if _, err := f.svc.Begin(ctx, "discord", identity.IntentLogin, nil); !errors.Is(err, identity.ErrProviderFailed) {
		t.Errorf("begin failure: %v", err)
	}
	if got := f.svc.Providers(); len(got) != 2 || got[0].Provider.Name() != "discord" || got[1].Provider.Name() != "steam" || got[1].Login {
		t.Errorf("providers: %+v", got)
	}
}

func TestLinkAndConflicts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	jo := f.login(t, "jo").User
	sam := f.login(t, "sam").User

	b, err := f.svc.Begin(ctx, "steam", identity.IntentLink, &jo.ID)
	if err != nil {
		t.Fatal(err)
	}
	params := identitytest.CallbackParams("jo-steam", f.steam.LastState())
	if _, err := f.svc.Complete(ctx, b.AttemptToken, "steam", params, nil); !errors.Is(err, identity.ErrWrongUser) {
		t.Errorf("link callback with no session: %v", err)
	}
	b, _ = f.svc.Begin(ctx, "steam", identity.IntentLink, &jo.ID)
	params = identitytest.CallbackParams("jo-steam", f.steam.LastState())
	if _, err := f.svc.Complete(ctx, b.AttemptToken, "steam", params, &sam.ID); !errors.Is(err, identity.ErrWrongUser) {
		t.Errorf("link callback in another user's session: %v", err)
	}
	b, _ = f.svc.Begin(ctx, "steam", identity.IntentLink, &jo.ID)
	params = identitytest.CallbackParams("jo-steam", f.steam.LastState())
	done, err := f.svc.Complete(ctx, b.AttemptToken, "steam", params, &jo.ID)
	if err != nil || done.Intent != identity.IntentLink || done.User.ID != jo.ID || done.Identity.Provider != "steam" || done.Identity.VerificationMethod != identity.MethodOpenID || done.Identity.LastLoginAt != nil {
		t.Fatalf("link: %v %+v", err, done)
	}
	me, _ := f.svc.Me(ctx, jo.ID)
	if len(me.Identities) != 2 || me.Identities[0].Provider != "discord" || me.Identities[1].Provider != "steam" {
		t.Errorf("after link: %+v", me.Identities)
	}

	// Sam tries to link the same Steam account.
	b, _ = f.svc.Begin(ctx, "steam", identity.IntentLink, &sam.ID)
	if _, err := f.svc.Complete(ctx, b.AttemptToken, "steam", identitytest.CallbackParams("jo-steam", f.steam.LastState()), &sam.ID); !errors.Is(err, identity.ErrIdentityTaken) {
		t.Errorf("linking an identity linked to another user: %v", err)
	}

	// Jo links it again: refreshed, no new event.
	f.steam.Accounts["jo-steam"] = identity.Account{Subject: "76561198000000001", DisplayName: "Renamed", Method: identity.MethodOpenID}
	events := len(f.st.Events)
	b, _ = f.svc.Begin(ctx, "steam", identity.IntentLink, &jo.ID)
	done, err = f.svc.Complete(ctx, b.AttemptToken, "steam", identitytest.CallbackParams("jo-steam", f.steam.LastState()), &jo.ID)
	if err != nil || done.Identity.DisplayName != "Renamed" || len(f.st.Events) != events {
		t.Errorf("re-link: %v %+v events %d→%d", err, done.Identity, events, len(f.st.Events))
	}

	// Lookup is what role sync asks.
	found, err := f.svc.Lookup(ctx, "steam", "76561198000000001")
	if err != nil || found.ID != jo.ID || len(found.Identities) != 2 {
		t.Errorf("lookup: %v %+v", err, found)
	}
	if _, err := f.svc.Lookup(ctx, "steam", "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("lookup unknown: %v", err)
	}
}

func TestUnlink(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	jo := f.login(t, "jo").User
	b, _ := f.svc.Begin(ctx, "steam", identity.IntentLink, &jo.ID)
	if _, err := f.svc.Complete(ctx, b.AttemptToken, "steam", identitytest.CallbackParams("jo-steam", f.steam.LastState()), &jo.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Unlink(ctx, jo.ID, "steam", "someone-else"); !errors.Is(err, identity.ErrNotLinked) {
		t.Errorf("not linked: %v", err)
	}
	if err := f.svc.Unlink(ctx, jo.ID, "steam", "76561198000000001"); err != nil {
		t.Errorf("unlink: %v", err)
	}
	if err := f.svc.Unlink(ctx, jo.ID, "discord", "1"); !errors.Is(err, identity.ErrLastIdentity) {
		t.Errorf("last identity: %v", err)
	}
	if me, _ := f.svc.Me(ctx, jo.ID); len(me.Identities) != 1 {
		t.Errorf("after unlink: %+v", me.Identities)
	}
}

// racingStore makes the first registration lose to a concurrent one: GetIdentity says "not
// linked", RegisterUser says "linked meanwhile", and the login must then proceed as a login.
type racingStore struct {
	*identitytest.FakeStore
	primed bool
}

func (r *racingStore) GetIdentity(ctx context.Context, provider, subject string) (store.Identity, error) {
	if !r.primed {
		r.primed = true
		return store.Identity{}, store.ErrNotFound
	}
	return r.FakeStore.GetIdentity(ctx, provider, subject)
}

func TestRegistrationRaceBecomesLogin(t *testing.T) {
	f := newFixture(t)
	jo := f.login(t, "jo").User
	f.svc = f.service(&racingStore{FakeStore: f.st})
	done := f.login(t, "jo")
	if done.Registered || done.User.ID != jo.ID {
		t.Errorf("the loser of the race must log in as the existing user: %+v", done)
	}
	if len(f.st.Users) != 1 {
		t.Errorf("users: %d", len(f.st.Users))
	}
}

func TestMergeOnSharedVerifiedEmail(t *testing.T) {
	t.Skip("absorb-don't-delete merge of two users on a shared verified email is not in v1: Discord's identify scope carries no email and Steam has none; it returns with the proof-of-control work (gravel#5)")
}

func TestStoreErrorsPropagate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.st.ErrOn = "CreateAuthAttempt"
	if _, err := f.svc.Begin(ctx, "discord", identity.IntentLogin, nil); err == nil || errors.Is(err, identity.ErrProviderFailed) {
		t.Errorf("store error must not masquerade as a provider error: %v", err)
	}
	f.st.ErrOn = ""
	b, _ := f.svc.Begin(ctx, "discord", identity.IntentLogin, nil)
	f.st.ErrOn = "ConsumeAuthAttempt"
	if _, err := f.svc.Complete(ctx, b.AttemptToken, "discord", identitytest.CallbackParams("jo", f.discord.LastState()), nil); err == nil || errors.Is(err, identity.ErrAttemptInvalid) {
		t.Errorf("store error must not read as an invalid attempt: %v", err)
	}
}

func TestPrune(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Begin(ctx, "discord", identity.IntentLogin, nil); err != nil {
		t.Fatal(err)
	}
	if n, err := f.svc.Prune(ctx); err != nil || n != 0 {
		t.Errorf("nothing expired yet: %v %d", err, n)
	}
	*f.now = f.now.Add(time.Hour)
	if n, err := f.svc.Prune(ctx); err != nil || n != 1 {
		t.Errorf("prune: %v %d", err, n)
	}
}
