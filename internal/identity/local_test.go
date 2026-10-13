package identity_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/identity/identitytest"
	"github.com/gravel-project/gravel/internal/store"
)

// localFixture is newFixture with local accounts on, cheap hashing and a policy the test sets.
func localFixture(t *testing.T) (*fixture, *identity.LocalPolicy) {
	t.Helper()
	f := newFixture(t)
	policy := &identity.LocalPolicy{}
	f.svc = identity.New(f.st, orgID, []identity.Registration{{Provider: f.discord, Login: true}}, 10*time.Minute,
		slog.New(slog.NewTextHandler(io.Discard, nil)), identity.WithClock(func() time.Time { return *f.now }),
		identity.WithLocalAccounts(f.st, func(context.Context) (identity.LocalPolicy, error) { return *policy, nil }),
		identity.WithPasswordParams(cheap))
	return f, policy
}

func TestCanonicalUsername(t *testing.T) {
	for in, want := range map[string]string{"Joe": "joe", "  neckbeard ": "neckbeard", "ＶＰＳ.dad": "vps.dad", "k8s_bro-1": "k8s_bro-1"} {
		if got, err := identity.CanonicalUsername(in); err != nil || got != want {
			t.Errorf("CanonicalUsername(%q) = %q %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "jo", strings.Repeat("a", 33), "_jo", ".jo", "jo smith", "jö", "jo@home", "Admin", "OWNER", "gravel", "ｇｒａｖｅｌ"} {
		if got, err := identity.CanonicalUsername(bad); !errors.Is(err, identity.ErrInvalidUsername) {
			t.Errorf("CanonicalUsername(%q) = %q %v", bad, got, err)
		}
	}
}

func TestLocalRegisterAndLogin(t *testing.T) {
	f, policy := localFixture(t)
	ctx := context.Background()
	if _, err := f.svc.LocalRegister(ctx, "Neckbeard", "a long enough password"); !errors.Is(err, identity.ErrRegistrationClosed) {
		t.Fatalf("closed registration: %v", err)
	}
	policy.RegistrationOpen = true
	for pw, why := range map[string]string{"short": "too short", "neckbeard": "too short"} {
		if _, err := f.svc.LocalRegister(ctx, "Neckbeard", pw); !errors.Is(err, identity.ErrWeakPassword) {
			t.Errorf("%s: %v", why, err)
		}
	}
	policy.MinPasswordLength = 9
	if _, err := f.svc.LocalRegister(ctx, "neckbeard1", "neckbeard1"); !errors.Is(err, identity.ErrWeakPassword) {
		t.Errorf("password == username: %v", err)
	}
	done, err := f.svc.LocalRegister(ctx, "Neckbeard", "a long enough password")
	if err != nil || !done.Registered || done.User.DisplayName != "neckbeard" || done.Identity.Provider != "local" || done.Identity.Subject != "neckbeard" || done.Identity.VerificationMethod != "password" {
		t.Fatalf("register: %v %+v", err, done)
	}
	if c := f.st.Locals["neckbeard"]; !strings.HasPrefix(c.PasswordHash, "$argon2id$") || strings.Contains(c.PasswordHash, "long enough") || f.st.Identities["local/neckbeard"].Credential != nil {
		t.Errorf("stored: %+v", c)
	}
	if _, err := f.svc.LocalRegister(ctx, "NECKBEARD", "another long password"); !errors.Is(err, identity.ErrIdentityTaken) {
		t.Errorf("taken: %v", err)
	}

	*f.now = f.now.Add(time.Hour)
	in, err := f.svc.LocalLogin(ctx, "  NeckBeard", "a long enough password")
	if err != nil || in.Registered || in.User.ID != done.User.ID || !in.User.LastLoginAt.Equal(*f.now) {
		t.Fatalf("login: %v %+v", err, in)
	}
	var events []string
	for _, e := range f.st.Events {
		events = append(events, e.Event)
	}
	if strings.Join(events, ",") != "registered,login" {
		t.Errorf("events: %v", events)
	}
}

// An unknown username, a wrong password and a locked account give the same answer.
func TestLocalLoginAnswersAlike(t *testing.T) {
	f, policy := localFixture(t)
	ctx := context.Background()
	policy.RegistrationOpen = true
	if _, err := f.svc.LocalRegister(ctx, "joe", "the right password"); err != nil {
		t.Fatal(err)
	}
	unknown, err1 := f.svc.LocalLogin(ctx, "nobody", "the right password")
	wrong, err2 := f.svc.LocalLogin(ctx, "joe", "the wrong password")
	invalid, err3 := f.svc.LocalLogin(ctx, "x", "anything")
	for i, err := range []error{err1, err2, err3} {
		if !errors.Is(err, identity.ErrBadCredentials) || err.Error() != identity.ErrBadCredentials.Error() {
			t.Errorf("refusal %d: %v", i, err)
		}
	}
	if unknown.User.ID != wrong.User.ID || wrong.User.ID != invalid.User.ID {
		t.Error("a refusal returned a member")
	}
	if c := f.st.Locals["joe"]; c.FailedAttempts != 1 {
		t.Errorf("one failure counted, got %+v", c)
	}
}

// Five failures lock the account for 15 minutes, the next lockout 30, and a good login clears it.
func TestLocalLockoutGrowsAndClears(t *testing.T) {
	f, policy := localFixture(t)
	ctx := context.Background()
	policy.RegistrationOpen = true
	if _, err := f.svc.LocalRegister(ctx, "joe", "the right password"); err != nil {
		t.Fatal(err)
	}
	fail := func(n int) error {
		var err error
		for range n {
			_, err = f.svc.LocalLogin(ctx, "joe", "wrong")
		}
		return err
	}
	if err := fail(4); errors.Is(err, identity.ErrLocked) {
		t.Fatal("locked after 4")
	}
	if err := fail(1); !errors.Is(err, identity.ErrLocked) || !errors.Is(err, identity.ErrBadCredentials) {
		t.Fatalf("5th failure: %v", err)
	}
	c := f.st.Locals["joe"]
	if c.LockedUntil == nil || !c.LockedUntil.Equal(f.now.Add(15*time.Minute)) || c.Lockouts != 1 || c.FailedAttempts != 0 {
		t.Fatalf("first lockout: %+v", c)
	}
	// Locked: even the right password is refused, and nothing more is counted.
	if _, err := f.svc.LocalLogin(ctx, "joe", "the right password"); !errors.Is(err, identity.ErrLocked) {
		t.Errorf("right password while locked: %v", err)
	}
	if got := f.st.Locals["joe"]; got.FailedAttempts != 0 || got.Lockouts != 1 {
		t.Errorf("counted while locked: %+v", got)
	}
	*f.now = f.now.Add(16 * time.Minute)
	if err := fail(5); !errors.Is(err, identity.ErrLocked) {
		t.Fatalf("second lockout: %v", err)
	}
	if c := f.st.Locals["joe"]; !c.LockedUntil.Equal(f.now.Add(30*time.Minute)) || c.Lockouts != 2 {
		t.Errorf("second lockout doubles: %+v", c)
	}
	*f.now = f.now.Add(31 * time.Minute)
	if _, err := f.svc.LocalLogin(ctx, "joe", "the right password"); err != nil {
		t.Fatalf("after the lockout: %v", err)
	}
	if c := f.st.Locals["joe"]; c.LockedUntil != nil || c.Lockouts != 0 || c.FailedAttempts != 0 {
		t.Errorf("a good login clears: %+v", c)
	}
}

// A hash made with old parameters is replaced at the next good login.
func TestLocalRehashOnParameterChange(t *testing.T) {
	f, policy := localFixture(t)
	ctx := context.Background()
	policy.RegistrationOpen = true
	if _, err := f.svc.LocalRegister(ctx, "joe", "the right password"); err != nil {
		t.Fatal(err)
	}
	before := f.st.Locals["joe"].PasswordHash
	stronger := cheap
	stronger.MemoryKiB = 128
	svc := identity.New(f.st, orgID, nil, time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)),
		identity.WithLocalAccounts(f.st, func(context.Context) (identity.LocalPolicy, error) { return *policy, nil }), identity.WithPasswordParams(stronger))
	if _, err := svc.LocalLogin(ctx, "joe", "the wrong password"); !errors.Is(err, identity.ErrBadCredentials) || f.st.Locals["joe"].PasswordHash != before {
		t.Fatalf("a wrong password rehashed: %v", err)
	}
	if _, err := svc.LocalLogin(ctx, "joe", "the right password"); err != nil {
		t.Fatal(err)
	}
	after := f.st.Locals["joe"].PasswordHash
	if after == before || !strings.HasPrefix(after, "$argon2id$v=19$m=128,") {
		t.Errorf("rehash: %q -> %q", before, after)
	}
	if _, err := svc.LocalLogin(ctx, "joe", "the right password"); err != nil {
		t.Errorf("login after the rehash: %v", err)
	}
}

// A member who signed in with Discord adds a username and password, then logs in with either;
// the last identity can't be unlinked, and unlinking the local one removes its credential.
func TestLocalLinkBesideDiscord(t *testing.T) {
	f, _ := localFixture(t)
	ctx := context.Background()
	jo := f.login(t, "jo")
	if _, err := f.svc.LocalLink(ctx, jo.User.ID, "admin", "a long enough password"); !errors.Is(err, identity.ErrInvalidUsername) {
		t.Errorf("reserved: %v", err)
	}
	linked, err := f.svc.LocalLink(ctx, jo.User.ID, "Jo.Plays", "a long enough password")
	if err != nil || linked.Intent != identity.IntentLink || linked.Identity.Subject != "jo.plays" || linked.User.ID != jo.User.ID {
		t.Fatalf("link: %v %+v", err, linked)
	}
	if _, err := f.svc.LocalLink(ctx, jo.User.ID, "jo.again", "a long enough password"); !errors.Is(err, identity.ErrAlreadyLocal) {
		t.Errorf("a second local account: %v", err)
	}
	sam := f.login(t, "sam")
	if _, err := f.svc.LocalLink(ctx, sam.User.ID, "jo.plays", "a long enough password"); !errors.Is(err, identity.ErrIdentityTaken) {
		t.Errorf("someone else's username: %v", err)
	}
	if in, err := f.svc.LocalLogin(ctx, "jo.plays", "a long enough password"); err != nil || in.User.ID != jo.User.ID {
		t.Fatalf("login with the password: %v %+v", err, in)
	}
	if again := f.login(t, "jo"); again.User.ID != jo.User.ID {
		t.Error("Discord login lands elsewhere")
	}
	if err := f.svc.Unlink(ctx, jo.User.ID, "local", "jo.plays"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.st.Locals["jo.plays"]; ok {
		t.Error("the credential outlived its identity")
	}
	if err := f.svc.Unlink(ctx, jo.User.ID, "discord", "1"); !errors.Is(err, identity.ErrLastIdentity) {
		t.Errorf("last identity: %v", err)
	}
}

func TestLocalDisabled(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if f.svc.LocalEnabled() {
		t.Error("enabled without the option")
	}
	if _, err := f.svc.LocalLogin(ctx, "jo", "x"); !errors.Is(err, identity.ErrLocalDisabled) {
		t.Errorf("login: %v", err)
	}
	if _, err := f.svc.LocalRegister(ctx, "jo", "x"); !errors.Is(err, identity.ErrLocalDisabled) {
		t.Errorf("register: %v", err)
	}
	var _ identity.LocalStore = identitytest.NewFakeStore()
	var _ identity.LocalStore = (*store.Store)(nil)
}
