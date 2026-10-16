package identity

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/ratelimit"
	"github.com/gravel-project/gravel/internal/store"
)

// A recovery address and the mailed reset (gravel#234, ADR-0026): a member with a local account
// may give it a recovery address, which the hub only ever mails a password link to, and only once
// the member proved it with a link mailed there. A member who forgot their password asks for that
// link by username alone, and the answer is the same whether anything was mailed or not.

// EmailVerificationTTL is how long a recovery address's verification link works: a day, since a
// mail may wait to be read.
const EmailVerificationTTL = 24 * time.Hour

// Mail limits a local account: at most three mails of each kind an hour, so the form can't flood a
// member's inbox nor the account page someone else's.
const (
	MailsPerHour = 3
	mailTimeout  = 30 * time.Second
)

// Errors of the recovery address.
var (
	ErrMailDisabled = errors.New("this hub sends no mail")
	ErrNoLocalLogin = errors.New("you have no username and password")
	ErrBadPassword  = errors.New("that is not your current password")
	ErrMailLimited  = errors.New("too many mails for this account; try again in an hour")
)

// Mailer sends one plain-text message to one address (notify.Mailer over hub.yaml's mail server).
type Mailer interface {
	Mail(ctx context.Context, to, subject, body string) error
}

// RecoveryStore is what the recovery address needs from the database.
type RecoveryStore interface {
	SetRecoveryEmail(ctx context.Context, subject string, email *string, v *store.EmailVerification) error
	GetEmailVerification(ctx context.Context, tokenHash []byte, now time.Time) (store.EmailVerification, error)
	VerifyRecoveryEmail(ctx context.Context, tokenHash []byte, now time.Time) (store.EmailVerification, uuid.UUID, error)
	DeleteExpiredEmailVerifications(ctx context.Context, cutoff time.Time) (int64, error)
}

type mailing struct {
	m       Mailer
	st      RecoveryStore
	baseURL string
	limit   *ratelimit.Limiter
	wg      sync.WaitGroup
}

// WithMail lets the hub mail members (hub.yaml notify.smtp): a recovery address's verification and
// a password link asked for by username. baseURL is the origin the links point at (auth.base_url).
// It needs local accounts and password links on.
func WithMail(m Mailer, st RecoveryStore, baseURL string) Option {
	return func(s *Service) {
		s.mail = &mailing{m: m, st: st, baseURL: strings.TrimSuffix(baseURL, "/")}
	}
}

// MailEnabled reports whether the hub mails members: local accounts, password links and a mail
// server.
func (s *Service) MailEnabled() bool { return s.mail != nil && s.local != nil && s.links != nil }

// WaitMail waits for the mail being sent: the hub's drain, and tests.
func (s *Service) WaitMail() {
	if s.mail != nil {
		s.mail.wg.Wait()
	}
}

func (s *Service) mailAllowed(kind, subject string) bool {
	if s.mail.limit == nil {
		s.mail.limit = ratelimit.NewEvery(MailsPerHour, time.Hour, MailsPerHour).WithClock(s.now)
	}
	ok, _ := s.mail.limit.Allow(kind + ":" + subject)
	return ok
}

// sendMail mails in the background, so a request answers in the same time whether it mailed or not
// and never waits for the mail server; done hears how it went.
func (s *Service) sendMail(ctx context.Context, to, subject, body string, done func(error)) {
	s.mail.wg.Add(1)
	go func() {
		defer s.mail.wg.Done()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mailTimeout)
		defer cancel()
		done(s.mail.m.Mail(ctx, to, subject, body))
	}()
}

// parseRecoveryEmail checks a recovery address: one bare address, at most MaxEmail.
func parseRecoveryEmail(e string) (string, error) {
	a, err := mail.ParseAddress(e)
	if err != nil || a.Address != e || a.Name != "" || len(e) > MaxEmail {
		return "", fmt.Errorf("%w: %q", ErrInvalidEmail, e)
	}
	return e, nil
}

// RecoveryAddress is a local account's recovery address and whether the member proved it.
type RecoveryAddress struct {
	Email      string // "" for none
	VerifiedAt *time.Time
}

// RecoveryAddressOf reads the recovery address of a member's local account; ErrNoLocalLogin when
// they have none.
func (s *Service) RecoveryAddressOf(ctx context.Context, userID uuid.UUID) (RecoveryAddress, error) {
	subject, err := s.localSubject(ctx, userID)
	if err != nil {
		return RecoveryAddress{}, err
	}
	c, err := s.local.st.GetLocalCredential(ctx, subject)
	if err != nil {
		return RecoveryAddress{}, err
	}
	a := RecoveryAddress{VerifiedAt: c.EmailVerifiedAt}
	if c.Email != nil {
		a.Email = *c.Email
	}
	return a, nil
}

func (s *Service) localSubject(ctx context.Context, userID uuid.UUID) (string, error) {
	if s.local == nil {
		return "", ErrNoLocalLogin
	}
	ids, err := s.st.ListIdentities(ctx, userID)
	if err != nil {
		return "", err
	}
	for _, i := range ids {
		if i.Provider == ProviderLocal {
			return i.Subject, nil
		}
	}
	return "", ErrNoLocalLogin
}

// RecoveryChange is what SetRecoveryEmail did.
type RecoveryChange string

// What SetRecoveryEmail did.
const (
	RecoveryMailed   RecoveryChange = "mailed"   // the address is set and its verification link mailed
	RecoveryVerified RecoveryChange = "verified" // the address was already set and proved: nothing to do
	RecoveryRemoved  RecoveryChange = "removed"  // the account has no recovery address now
	RecoveryNone     RecoveryChange = "none"     // there was no address to remove
)

// SetRecoveryEmail sets, changes or (email "") removes the recovery address of the member's local
// account, with their current password: a stolen session alone can't point recovery elsewhere. A
// new or unproved address is mailed a link that proves it; until it is used the address gets no
// password link. A wrong password counts toward the lockout like a failed login. ErrMailDisabled,
// ErrNoLocalLogin, ErrBadPassword, ErrLocked, ErrInvalidEmail, ErrMailLimited.
func (s *Service) SetRecoveryEmail(ctx context.Context, userID uuid.UUID, email, password string) (RecoveryChange, error) {
	if !s.MailEnabled() {
		return "", ErrMailDisabled
	}
	subject, err := s.localSubject(ctx, userID)
	if err != nil {
		return "", err
	}
	cred, err := s.local.st.GetLocalCredential(ctx, subject)
	if err != nil {
		return "", err
	}
	now := s.now()
	if cred.LockedUntil != nil && now.Before(*cred.LockedUntil) {
		return "", ErrLocked
	}
	if len(password) > MaxPasswordBytes {
		password = password[:MaxPasswordBytes]
	}
	ok, _, err := VerifyPassword(cred.PasswordHash, password, s.pwParams)
	if err != nil {
		s.logger.ErrorContext(ctx, "local credential unreadable", "subject", subject, "error", err.Error())
	}
	if !ok {
		c, err := s.local.st.RecordLocalFailure(ctx, subject, now, s.local.lockout)
		if err != nil {
			return "", err
		}
		if c.LockedUntil != nil && now.Before(*c.LockedUntil) {
			return "", ErrLocked
		}
		return "", ErrBadPassword
	}
	if err := s.local.st.RecordLocalSuccess(ctx, subject, ""); err != nil {
		return "", err
	}

	email = strings.TrimSpace(email)
	if email == "" {
		if err := s.mail.st.SetRecoveryEmail(ctx, subject, nil, nil); err != nil {
			return "", err
		}
		if cred.Email == nil {
			return RecoveryNone, nil
		}
		s.logger.InfoContext(ctx, "recovery address removed", "user_id", userID, "subject", subject)
		return RecoveryRemoved, nil
	}
	if email, err = parseRecoveryEmail(email); err != nil {
		return "", err
	}
	if cred.Email != nil && *cred.Email == email && cred.EmailVerifiedAt != nil {
		return RecoveryVerified, nil
	}
	if !s.mailAllowed("verify", subject) {
		return "", ErrMailLimited
	}
	raw := make([]byte, 32)
	if _, err := io.ReadFull(s.rand, raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	v := store.EmailVerification{TokenHash: hashToken(token), Subject: subject, Email: email, IssuedAt: now, ExpiresAt: now.Add(EmailVerificationTTL)}
	if err := s.mail.st.SetRecoveryEmail(ctx, subject, &email, &v); err != nil {
		return "", err
	}
	// The address and the token stay out of the log: the link proves the address for a day.
	s.logger.InfoContext(ctx, "recovery address set; verification mailed", "user_id", userID, "subject", subject)
	body := fmt.Sprintf("Confirm %s as the recovery address of the account %s on %s:\n\n%s\n\n"+
		"The link works for a day. Only a password link is ever mailed to this address, and only when you ask for one.\n"+
		"If you didn't ask for this, ignore this mail: the address won't be used.\n",
		email, subject, s.orgName, s.mail.baseURL+VerificationPath(token))
	s.sendMail(ctx, email, s.orgName+": confirm your recovery address", body, func(err error) {
		if err != nil {
			s.logger.ErrorContext(ctx, "verification mail not sent", "subject", subject, "error", err.Error())
		}
	})
	return RecoveryMailed, nil
}

// VerificationPath is a verification link's path on the hub.
func VerificationPath(token string) string { return "/auth/local/verify/" + token }

// Verification is an open verification link, for the page that confirms it.
type Verification struct {
	Username string
	Email    string
}

// VerificationFor reads an open verification link; it spends nothing. ErrLinkInvalid for an
// unknown or expired one, or one for an address the account no longer has.
func (s *Service) VerificationFor(ctx context.Context, token string) (Verification, error) {
	if !s.MailEnabled() || token == "" {
		return Verification{}, ErrLinkInvalid
	}
	v, err := s.mail.st.GetEmailVerification(ctx, hashToken(token), s.now())
	if errors.Is(err, store.ErrNotFound) {
		return Verification{}, ErrLinkInvalid
	}
	if err != nil {
		return Verification{}, err
	}
	return Verification{Username: v.Subject, Email: v.Email}, nil
}

// VerifyRecoveryEmail spends a verification link: the address it was mailed to is proved, and
// password links may be mailed to it from now on. ErrLinkInvalid.
func (s *Service) VerifyRecoveryEmail(ctx context.Context, token string) (Verification, error) {
	if !s.MailEnabled() || token == "" {
		return Verification{}, ErrLinkInvalid
	}
	v, userID, err := s.mail.st.VerifyRecoveryEmail(ctx, hashToken(token), s.now())
	if errors.Is(err, store.ErrNotFound) {
		return Verification{}, ErrLinkInvalid
	}
	if err != nil {
		return Verification{}, err
	}
	s.logger.InfoContext(ctx, "recovery address verified", "user_id", userID, "subject", v.Subject)
	return Verification{Username: v.Subject, Email: v.Email}, nil
}

// RequestPasswordMail mails a password link to the verified recovery address of the account with
// this username, if there is one and the account's limit allows. Its answer is the same for every
// username, address and limit, and it never waits for the mail server, so the caller learns
// nothing: only ErrMailDisabled, or a failure that is the hub's.
func (s *Service) RequestPasswordMail(ctx context.Context, username string) error {
	if !s.MailEnabled() {
		return ErrMailDisabled
	}
	subject, err := CanonicalUsername(username)
	if err != nil {
		return nil
	}
	cred, err := s.local.st.GetLocalCredential(ctx, subject)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil
	case err != nil:
		return err
	case cred.Email == nil || cred.EmailVerifiedAt == nil:
		s.logger.InfoContext(ctx, "password link not mailed: no verified recovery address", "subject", subject)
		return nil
	}
	if !s.mailAllowed("reset", subject) {
		s.linkObserved("mail_limited")
		s.logger.WarnContext(ctx, "password link not mailed: the account's mail limit", "subject", subject)
		return nil
	}
	link, err := s.issueLink(ctx, subject, store.PasswordLinkReset, nil, true)
	if err != nil {
		if errors.Is(err, ErrNoLocal) { // unlinked since
			return nil
		}
		return err
	}
	to := *cred.Email
	body := fmt.Sprintf("Someone, hopefully you, asked to reset the password of the account %s on %s.\n\n"+
		"Choose a new password here within the hour (until %s):\n\n%s\n\n"+
		"Using it logs out every other browser of yours. If it wasn't you, ignore this mail: your password stays as it is.\n",
		subject, s.orgName, link.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC"), s.mail.baseURL+link.Path())
	s.sendMail(ctx, to, s.orgName+": reset your password", body, func(err error) {
		if err != nil {
			s.linkObserved("mail_failed")
			s.logger.ErrorContext(ctx, "password link not mailed", "subject", subject, "error", err.Error())
			return
		}
		s.linkObserved("mailed")
	})
	return nil
}
