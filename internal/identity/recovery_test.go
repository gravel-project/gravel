package identity_test

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/identity"
	"github.com/gravel-project/gravel/internal/store"
)

// fakeMailer keeps every message; err, when set, fails every send.
type fakeMailer struct {
	mu   sync.Mutex
	sent []mailed
	err  error
}

type mailed struct{ to, subject, body string }

func (m *fakeMailer) Mail(_ context.Context, to, subject, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.sent = append(m.sent, mailed{to, subject, body})
	return nil
}

func (m *fakeMailer) take() []mailed {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.sent
	m.sent = nil
	return out
}

var linkInMail = regexp.MustCompile(`https://hub\.example\.com(/auth/local/(?:verify|reset)/)([A-Za-z0-9_-]+)`)

// tokenIn is the token of the one link in a mail, checking its path.
func tokenIn(t *testing.T, m mailed, path string) string {
	t.Helper()
	got := linkInMail.FindStringSubmatch(m.body)
	if got == nil || got[1] != path {
		t.Fatalf("no %s link in the mail:\n%s", path, m.body)
	}
	return got[2]
}

type mailFixture struct {
	*fixture
	logs    *strings.Builder
	results *[]string
	mailer  *fakeMailer
	userID  uuid.UUID
}

// mailFixture is a hub that mails members, with the local account vps.dad whose password is set.
func newMailFixture(t *testing.T) *mailFixture {
	t.Helper()
	f := newFixture(t)
	logs := &strings.Builder{}
	var results []string
	var mu sync.Mutex
	m := &fakeMailer{}
	f.svc = identity.New(f.st, orgID, []identity.Registration{{Provider: f.discord, Login: true}}, 10*time.Minute,
		slog.New(slog.NewTextHandler(&lockedWriter{w: logs}, nil)), identity.WithClock(func() time.Time { return *f.now }),
		identity.WithOrganizationName("HTG"),
		identity.WithLocalAccounts(f.st, func(context.Context) (identity.LocalPolicy, error) { return identity.LocalPolicy{}, nil }),
		identity.WithPasswordLinks(f.st, func(r string) { mu.Lock(); results = append(results, r); mu.Unlock() }),
		identity.WithMail(m, f.st, "https://hub.example.com/"),
		identity.WithPasswordParams(cheap))
	ctx := context.Background()
	u, link, err := f.svc.CreateLocal(ctx, "vps.dad", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.UsePasswordLink(ctx, link.Token, "a dashboard and no cluster"); err != nil {
		t.Fatal(err)
	}
	results = nil
	return &mailFixture{fixture: f, logs: logs, results: &results, mailer: m, userID: u.ID}
}

type lockedWriter struct {
	mu sync.Mutex
	w  *strings.Builder
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

const dadPassword = "a dashboard and no cluster"

func (f *mailFixture) events() []string {
	var out []string
	for _, e := range f.st.Events {
		out = append(out, e.Event)
	}
	return out
}

// verify sets addr and uses the link mailed to it.
func (f *mailFixture) verify(t *testing.T, addr string) {
	t.Helper()
	ctx := context.Background()
	if got, err := f.svc.SetRecoveryEmail(ctx, f.userID, addr, dadPassword); err != nil || got != identity.RecoveryMailed {
		t.Fatalf("set %s: %v %v", addr, got, err)
	}
	f.svc.WaitMail()
	sent := f.mailer.take()
	if len(sent) != 1 || sent[0].to != addr {
		t.Fatalf("verification mails: %+v", sent)
	}
	if _, err := f.svc.VerifyRecoveryEmail(ctx, tokenIn(t, sent[0], "/auth/local/verify/")); err != nil {
		t.Fatal(err)
	}
}

// The whole path: a member proves an address on the account page, forgets their password, asks for
// a link by username, and sets a new password from the mail with no staff involved.
func TestMailedReset(t *testing.T) {
	f := newMailFixture(t)
	ctx := context.Background()
	if !f.svc.MailEnabled() {
		t.Fatal("mail is on")
	}
	if _, err := f.svc.SetRecoveryEmail(ctx, f.userID, "dad@example.com", "wrong password"); !errors.Is(err, identity.ErrBadPassword) {
		t.Errorf("wrong password: %v", err)
	}
	if c := f.st.Locals["vps.dad"]; c.FailedAttempts != 1 || c.Email != nil {
		t.Errorf("a wrong password counts like a failed login and changes nothing: %+v", c)
	}
	if _, err := f.svc.SetRecoveryEmail(ctx, f.userID, "Dad <dad@example.com>", dadPassword); !errors.Is(err, identity.ErrInvalidEmail) {
		t.Errorf("bad address: %v", err)
	}
	got, err := f.svc.SetRecoveryEmail(ctx, f.userID, " dad@example.com ", dadPassword)
	if err != nil || got != identity.RecoveryMailed {
		t.Fatalf("set: %v %v", got, err)
	}
	if c := f.st.Locals["vps.dad"]; c.FailedAttempts != 0 || c.Email == nil || *c.Email != "dad@example.com" || c.EmailVerifiedAt != nil {
		t.Errorf("set, unproved, failures cleared: %+v", c)
	}
	f.svc.WaitMail()
	sent := f.mailer.take()
	if len(sent) != 1 || sent[0].to != "dad@example.com" || sent[0].subject != "HTG: confirm your recovery address" || !strings.Contains(sent[0].body, "vps.dad on HTG") {
		t.Fatalf("verification mail: %+v", sent)
	}
	verify := tokenIn(t, sent[0], "/auth/local/verify/")
	if a, err := f.svc.RecoveryAddressOf(ctx, f.userID); err != nil || a.Email != "dad@example.com" || a.VerifiedAt != nil {
		t.Errorf("address before the link: %+v %v", a, err)
	}

	// Unproved, the address gets nothing.
	if err := f.svc.RequestPasswordMail(ctx, "vps.dad"); err != nil {
		t.Fatal(err)
	}
	f.svc.WaitMail()
	if sent := f.mailer.take(); len(sent) != 0 {
		t.Fatalf("an unproved address was mailed: %+v", sent)
	}

	if v, err := f.svc.VerificationFor(ctx, verify); err != nil || v.Username != "vps.dad" || v.Email != "dad@example.com" {
		t.Fatalf("read the verification: %+v %v", v, err)
	}
	if _, err := f.svc.VerifyRecoveryEmail(ctx, verify); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.VerifyRecoveryEmail(ctx, verify); !errors.Is(err, identity.ErrLinkInvalid) {
		t.Errorf("second use: %v", err)
	}
	if a, _ := f.svc.RecoveryAddressOf(ctx, f.userID); a.VerifiedAt == nil || !a.VerifiedAt.Equal(*f.now) {
		t.Errorf("verified: %+v", a)
	}
	if got, err := f.svc.SetRecoveryEmail(ctx, f.userID, "dad@example.com", dadPassword); err != nil || got != identity.RecoveryVerified {
		t.Errorf("the same proved address again: %v %v", got, err)
	}

	// Forgotten: the link goes to the proved address, by any spelling of the username.
	if err := f.svc.RequestPasswordMail(ctx, "VPS.Dad"); err != nil {
		t.Fatal(err)
	}
	f.svc.WaitMail()
	sent = f.mailer.take()
	if len(sent) != 1 || sent[0].to != "dad@example.com" || sent[0].subject != "HTG: reset your password" {
		t.Fatalf("reset mail: %+v", sent)
	}
	reset := tokenIn(t, sent[0], "/auth/local/reset/")
	if u, _, err := f.svc.UsePasswordLink(ctx, reset, "a new sentence for a new day"); err != nil || u.ID != f.userID {
		t.Fatalf("use the mailed link: %v", err)
	}
	if _, _, err := f.svc.UsePasswordLink(ctx, reset, "a new sentence for a new day"); !errors.Is(err, identity.ErrLinkInvalid) {
		t.Errorf("the mailed link works once: %v", err)
	}
	if _, err := f.svc.LocalLogin(ctx, "vps.dad", "a new sentence for a new day"); err != nil {
		t.Errorf("login with the new password: %v", err)
	}
	if want := []string{"mailed", "used", "invalid"}; !slices.Equal(*f.results, want) {
		t.Errorf("results %q, want %q", *f.results, want)
	}
	ev := f.events()
	for _, want := range []string{store.IdentityEventEmailVerified, store.IdentityEventPasswordResetMailed, store.IdentityEventPasswordReset} {
		if !slices.Contains(ev, want) {
			t.Errorf("identity log lacks %s: %q", want, ev)
		}
	}
	for _, secret := range []string{verify, reset, "dad@example.com"} {
		if strings.Contains(f.logs.String(), secret) {
			t.Errorf("the log holds %q:\n%s", secret, f.logs)
		}
	}
}

// RequestPasswordMail answers the same for everything the asker may not learn.
func TestRequestPasswordMailSameAnswer(t *testing.T) {
	f := newMailFixture(t)
	ctx := context.Background()
	if _, _, err := f.svc.CreateLocal(ctx, "neckbeard", "neck@example.com", nil); err != nil { // an address staff typed: unproved
		t.Fatal(err)
	}
	for _, name := range []string{"nobody", "x", "", "vps.dad", "neckbeard"} {
		if err := f.svc.RequestPasswordMail(ctx, name); err != nil {
			t.Errorf("%q: %v", name, err)
		}
	}
	f.svc.WaitMail()
	if sent := f.mailer.take(); len(sent) != 0 {
		t.Errorf("mailed: %+v", sent)
	}
	off := identity.New(f.st, orgID, nil, time.Minute, slog.New(slog.DiscardHandler),
		identity.WithLocalAccounts(f.st, func(context.Context) (identity.LocalPolicy, error) { return identity.LocalPolicy{}, nil }),
		identity.WithPasswordLinks(f.st, nil))
	if off.MailEnabled() {
		t.Error("no mailer, no mail")
	}
	if err := off.RequestPasswordMail(ctx, "vps.dad"); !errors.Is(err, identity.ErrMailDisabled) {
		t.Errorf("no mailer: %v", err)
	}
	if _, err := off.SetRecoveryEmail(ctx, f.userID, "dad@example.com", dadPassword); !errors.Is(err, identity.ErrMailDisabled) {
		t.Errorf("no mailer: %v", err)
	}
	if _, err := off.VerificationFor(ctx, "anything"); !errors.Is(err, identity.ErrLinkInvalid) {
		t.Errorf("no mailer: %v", err)
	}
}

func TestMailLimits(t *testing.T) {
	f := newMailFixture(t)
	ctx := context.Background()
	f.verify(t, "dad@example.com")
	for range identity.MailsPerHour + 2 {
		if err := f.svc.RequestPasswordMail(ctx, "vps.dad"); err != nil {
			t.Fatal(err)
		}
	}
	f.svc.WaitMail()
	if sent := f.mailer.take(); len(sent) != identity.MailsPerHour {
		t.Errorf("mailed %d, want %d", len(sent), identity.MailsPerHour)
	}
	if n := strings.Count(strings.Join(*f.results, ","), "mail_limited"); n != 2 {
		t.Errorf("results %q", *f.results)
	}
	*f.now = f.now.Add(time.Hour)
	if err := f.svc.RequestPasswordMail(ctx, "vps.dad"); err != nil {
		t.Fatal(err)
	}
	f.svc.WaitMail()
	if sent := f.mailer.take(); len(sent) != 1 {
		t.Errorf("an hour later: %d mails", len(sent))
	}

	// Verification mails have their own limit, full again an hour after verify spent one.
	for i := range identity.MailsPerHour + 1 {
		_, err := f.svc.SetRecoveryEmail(ctx, f.userID, "other"+string(rune('a'+i))+"@example.com", dadPassword)
		if want := i < identity.MailsPerHour; (err == nil) != want {
			t.Errorf("change %d: %v", i, err)
		}
		if i == identity.MailsPerHour && !errors.Is(err, identity.ErrMailLimited) {
			t.Errorf("past the limit: %v", err)
		}
	}
}

// Changing the address unproves it and ends the open link; removing it ends recovery by mail.
func TestChangeRecoveryEmail(t *testing.T) {
	f := newMailFixture(t)
	ctx := context.Background()
	f.verify(t, "dad@example.com")
	if _, err := f.svc.SetRecoveryEmail(ctx, f.userID, "b@example.com", dadPassword); err != nil {
		t.Fatal(err)
	}
	f.svc.WaitMail()
	first := tokenIn(t, f.mailer.take()[0], "/auth/local/verify/")
	if a, _ := f.svc.RecoveryAddressOf(ctx, f.userID); a.Email != "b@example.com" || a.VerifiedAt != nil {
		t.Errorf("a change unproves: %+v", a)
	}
	if err := f.svc.RequestPasswordMail(ctx, "vps.dad"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SetRecoveryEmail(ctx, f.userID, "c@example.com", dadPassword); err != nil {
		t.Fatal(err)
	}
	f.svc.WaitMail()
	for _, m := range f.mailer.take() {
		if strings.Contains(m.subject, "reset") {
			t.Errorf("an unproved address got a reset: %+v", m)
		}
	}
	if _, err := f.svc.VerificationFor(ctx, first); !errors.Is(err, identity.ErrLinkInvalid) {
		t.Errorf("a link for an address since changed: %v", err)
	}
	if got, err := f.svc.SetRecoveryEmail(ctx, f.userID, "", dadPassword); err != nil || got != identity.RecoveryRemoved {
		t.Errorf("remove: %v %v", got, err)
	}
	if a, _ := f.svc.RecoveryAddressOf(ctx, f.userID); a.Email != "" || len(f.st.Verifications) != 0 {
		t.Errorf("removed: %+v, %d open links", a, len(f.st.Verifications))
	}
	// Expired links go at the prune.
	*f.now = f.now.Add(time.Hour) // past the verification mails' limit
	if _, err := f.svc.SetRecoveryEmail(ctx, f.userID, "d@example.com", dadPassword); err != nil {
		t.Fatal(err)
	}
	f.svc.WaitMail()
	verify := tokenIn(t, f.mailer.take()[0], "/auth/local/verify/")
	*f.now = f.now.Add(identity.EmailVerificationTTL + time.Second)
	if _, err := f.svc.VerifyRecoveryEmail(ctx, verify); !errors.Is(err, identity.ErrLinkInvalid) {
		t.Errorf("expired: %v", err)
	}
	if _, err := f.svc.Prune(ctx); err != nil || len(f.st.Verifications) != 0 {
		t.Errorf("prune: %v, %d left", err, len(f.st.Verifications))
	}
}

func TestRecoveryEmailRefusals(t *testing.T) {
	f := newMailFixture(t)
	ctx := context.Background()
	if _, err := f.svc.SetRecoveryEmail(ctx, uuid.New(), "a@example.com", dadPassword); !errors.Is(err, identity.ErrNoLocalLogin) {
		t.Errorf("no local account: %v", err)
	}
	for range 4 {
		_, _ = f.svc.SetRecoveryEmail(ctx, f.userID, "a@example.com", "wrong")
	}
	if _, err := f.svc.SetRecoveryEmail(ctx, f.userID, "a@example.com", "wrong"); !errors.Is(err, identity.ErrLocked) {
		t.Errorf("the fifth wrong password locks: %v", err)
	}
	if _, err := f.svc.SetRecoveryEmail(ctx, f.userID, "a@example.com", dadPassword); !errors.Is(err, identity.ErrLocked) {
		t.Errorf("locked, even with the right password: %v", err)
	}
}

func TestMailFailure(t *testing.T) {
	f := newMailFixture(t)
	ctx := context.Background()
	f.verify(t, "dad@example.com")
	f.mailer.err = errors.New("smtp down")
	if err := f.svc.RequestPasswordMail(ctx, "vps.dad"); err != nil {
		t.Fatalf("a mail server's failure is never the asker's answer: %v", err)
	}
	f.svc.WaitMail()
	if !slices.Equal(*f.results, []string{"mail_failed"}) {
		t.Errorf("results %q", *f.results)
	}
	if !strings.Contains(f.logs.String(), "password link not mailed") {
		t.Errorf("the failure is logged:\n%s", f.logs)
	}
}
