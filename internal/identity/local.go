package identity

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/text/unicode/norm"

	"github.com/gravel-project/gravel/internal/store"
)

// Local accounts (ADR-0026): a username and a password are one more provider. The identity is
// (ProviderLocal, the canonical username), with the uniqueness, events and unlink rules of any
// other; the password is a credential beside it. Unlike an OAuth provider there is no redirect:
// the login form posts here.

// ProviderLocal is a local account's provider name.
const ProviderLocal = store.ProviderLocal

// MethodPassword is how a local account proves control: its password.
const MethodPassword = "password"

// Username limits: 3 to 32 of a-z, 0-9, '.', '_' and '-', starting with a letter or a digit,
// after NFKC and lower case.
const (
	MinUsername = 3
	MaxUsername = 32
)

// Password limits. The host's minimum (LocalPolicy.MinPasswordLength) is between these.
const (
	DefaultMinPassword = 12
	MinMinPassword     = 8
	MaxMinPassword     = 128
)

// ReservedUsernames can't be registered or set: they read as the hub speaking.
var ReservedUsernames = []string{"admin", "administrator", "gravel", "moderator", "operator", "owner", "root", "staff", "support", "system"}

// DefaultLockout locks a local account after 5 failed logins in a row, for 15 minutes, doubling
// with each lockout since the last good login, up to a day.
var DefaultLockout = store.Lockout{Threshold: 5, Base: 15 * time.Minute, Max: 24 * time.Hour}

// Errors of local accounts. ErrBadCredentials is the one answer to an unknown username, a wrong
// password and a locked account alike, so a caller learns nothing about which; ErrLocked is
// ErrBadCredentials too, for the hub's own metrics and logs.
var (
	ErrLocalDisabled      = errors.New("local accounts are not enabled on this hub")
	ErrBadCredentials     = errors.New("that username and password don't match an account that may log in now")
	ErrLocked             = fmt.Errorf("%w (locked)", ErrBadCredentials)
	ErrRegistrationClosed = errors.New("this hub does not take registrations; ask its staff for an account")
	ErrInvalidUsername    = errors.New("invalid username")
	ErrWeakPassword       = errors.New("password not accepted")
	ErrAlreadyLocal       = errors.New("this member already has a username and password")
)

// LocalStore is what local accounts need from the database beyond Store.
type LocalStore interface {
	GetLocalCredential(ctx context.Context, subject string) (store.LocalCredential, error)
	RecordLocalFailure(ctx context.Context, subject string, now time.Time, l store.Lockout) (store.LocalCredential, error)
	RecordLocalSuccess(ctx context.Context, subject, rehash string) error
}

// LocalPolicy is the host's choice for local accounts, from the Organization settings.
type LocalPolicy struct {
	RegistrationOpen  bool
	MinPasswordLength int // 0 is DefaultMinPassword
}

func (p LocalPolicy) minPassword() int {
	if p.MinPasswordLength == 0 {
		return DefaultMinPassword
	}
	return p.MinPasswordLength
}

type localAccounts struct {
	st      LocalStore
	policy  func(context.Context) (LocalPolicy, error)
	lockout store.Lockout

	dummyOnce sync.Once
	dummy     string
}

// WithLocalAccounts turns local accounts on (hub.yaml auth.local.enabled) over st; policy reads
// the host's choices at each call, so a settings change applies at once.
func WithLocalAccounts(st LocalStore, policy func(context.Context) (LocalPolicy, error)) Option {
	return func(s *Service) {
		s.local = &localAccounts{st: st, policy: policy, lockout: DefaultLockout}
	}
}

// WithPasswordParams replaces argon2id's costs (tests use cheap ones).
func WithPasswordParams(p PasswordParams) Option { return func(s *Service) { s.pwParams = p } }

// LocalEnabled reports whether the hub takes local accounts.
func (s *Service) LocalEnabled() bool { return s.local != nil }

// LocalPolicy is the host's current choice; ErrLocalDisabled when local accounts are off.
func (s *Service) LocalPolicy(ctx context.Context) (LocalPolicy, error) {
	if s.local == nil {
		return LocalPolicy{}, ErrLocalDisabled
	}
	p, err := s.local.policy(ctx)
	if err != nil {
		return LocalPolicy{}, err
	}
	p.MinPasswordLength = p.minPassword()
	return p, nil
}

// CanonicalUsername is the username as stored and compared: NFKC (a fullwidth Ａ is A), lower
// case, trimmed; then checked against the charset, the length and the reserved names.
func CanonicalUsername(raw string) (string, error) {
	u := strings.ToLower(norm.NFKC.String(strings.TrimSpace(raw)))
	switch {
	case u == "":
		return "", fmt.Errorf("%w: choose a username", ErrInvalidUsername)
	case len(u) < MinUsername || len(u) > MaxUsername:
		return "", fmt.Errorf("%w: a username has %d to %d characters", ErrInvalidUsername, MinUsername, MaxUsername)
	}
	for i, r := range u {
		alnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if i == 0 && !alnum {
			return "", fmt.Errorf("%w: a username starts with a letter or a digit", ErrInvalidUsername)
		}
		if !alnum && r != '.' && r != '_' && r != '-' {
			return "", fmt.Errorf("%w: a username has only letters a-z, digits and . _ -", ErrInvalidUsername)
		}
	}
	if slices.Contains(ReservedUsernames, u) {
		return "", fmt.Errorf("%w: %q is reserved", ErrInvalidUsername, u)
	}
	return u, nil
}

// checkPassword applies the password rules: the host's minimum length in characters after NFKC,
// MaxPasswordBytes at most, and never the username itself.
func checkPassword(username, pw string, minLen int) error {
	n := normalizePassword(pw)
	switch {
	case utf8.RuneCountInString(n) < minLen:
		return fmt.Errorf("%w: a password has at least %d characters", ErrWeakPassword, minLen)
	case len(n) > MaxPasswordBytes:
		return fmt.Errorf("%w: a password has at most %d bytes", ErrWeakPassword, MaxPasswordBytes)
	case strings.EqualFold(n, username):
		return fmt.Errorf("%w: a password is not the username", ErrWeakPassword)
	}
	return nil
}

// dummyHash is a hash nobody's password matches, verified for an unknown username so it costs
// what a known one does.
func (l *localAccounts) dummyHash(s *Service) string {
	l.dummyOnce.Do(func() {
		h, err := HashPassword(uuid.NewString(), s.pwParams, s.rand)
		if err != nil {
			h = "$argon2id$v=19$m=1,t=1,p=1$AAAA$AAAA"
		}
		l.dummy = h
	})
	return l.dummy
}

// LocalLogin logs a member in with a username and password. Every refusal is ErrBadCredentials
// (ErrLocked when the account is locked) and takes one hash verification, whether the username
// exists or not. A wrong password counts toward the lockout; one while locked doesn't.
func (s *Service) LocalLogin(ctx context.Context, username, password string) (Completed, error) {
	l := s.local
	if l == nil {
		return Completed{}, ErrLocalDisabled
	}
	if len(password) > MaxPasswordBytes {
		password = password[:MaxPasswordBytes] // can't match: longer ones are never stored
	}
	subject, uerr := CanonicalUsername(username)
	var cred store.LocalCredential
	found := false
	if uerr == nil {
		c, err := l.st.GetLocalCredential(ctx, subject)
		switch {
		case err == nil:
			cred, found = c, true
		case !errors.Is(err, store.ErrNotFound):
			return Completed{}, err
		}
	}
	hash := l.dummyHash(s)
	if found {
		hash = cred.PasswordHash
	}
	ok, rehash, err := VerifyPassword(hash, password, s.pwParams)
	if err != nil && found {
		s.logger.ErrorContext(ctx, "local credential unreadable", "subject", subject, "error", err.Error())
	}
	now := s.now()
	if !found {
		return Completed{}, ErrBadCredentials
	}
	if cred.LockedUntil != nil && now.Before(*cred.LockedUntil) {
		return Completed{}, ErrLocked
	}
	if !ok {
		c, err := l.st.RecordLocalFailure(ctx, subject, now, l.lockout)
		if err != nil {
			return Completed{}, err
		}
		if c.LockedUntil != nil && now.Before(*c.LockedUntil) {
			s.logger.WarnContext(ctx, "local account locked", "subject", subject, "until", c.LockedUntil.UTC().Format(time.RFC3339), "lockouts", c.Lockouts)
			return Completed{}, ErrLocked
		}
		return Completed{}, ErrBadCredentials
	}
	newHash := ""
	if rehash {
		if newHash, err = HashPassword(password, s.pwParams, s.rand); err != nil {
			return Completed{}, err
		}
	}
	if err := l.st.RecordLocalSuccess(ctx, subject, newHash); err != nil {
		return Completed{}, err
	}
	ident, err := s.st.GetIdentity(ctx, ProviderLocal, subject)
	if err != nil {
		return Completed{}, err
	}
	return s.recordLogin(ctx, ident.UserID, Account{Provider: ProviderLocal, Subject: subject, DisplayName: ident.DisplayName, Method: MethodPassword}, now)
}

// LocalRegister makes a new member with a local account, when the host has opened registration.
// ErrRegistrationClosed, ErrInvalidUsername, ErrWeakPassword, ErrIdentityTaken (the username).
func (s *Service) LocalRegister(ctx context.Context, username, password string) (Completed, error) {
	policy, err := s.LocalPolicy(ctx)
	if err != nil {
		return Completed{}, err
	}
	if !policy.RegistrationOpen {
		return Completed{}, ErrRegistrationClosed
	}
	ident, err := s.newLocalIdentity(ctx, username, password, policy)
	if err != nil {
		return Completed{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Completed{}, fmt.Errorf("identity: new id: %w", err)
	}
	u, err := s.st.RegisterUser(ctx, store.User{ID: id, OrganizationID: s.orgID, DisplayName: ident.Subject}, ident)
	if errors.Is(err, store.ErrIdentityExists) {
		return Completed{}, ErrIdentityTaken
	}
	if err != nil {
		return Completed{}, err
	}
	got, err := s.st.GetIdentity(ctx, ProviderLocal, ident.Subject)
	if err != nil {
		return Completed{}, err
	}
	s.logger.InfoContext(ctx, "user registered", "user_id", u.ID, "provider", ProviderLocal)
	return Completed{Intent: IntentLogin, User: u, Identity: got, Registered: true}, nil
}

// LocalLink gives a logged-in member a username and password, as linking a provider does: one
// local account per member. ErrAlreadyLocal, ErrInvalidUsername, ErrWeakPassword, ErrIdentityTaken.
func (s *Service) LocalLink(ctx context.Context, userID uuid.UUID, username, password string) (Completed, error) {
	policy, err := s.LocalPolicy(ctx)
	if err != nil {
		return Completed{}, err
	}
	ident, err := s.newLocalIdentity(ctx, username, password, policy)
	if err != nil {
		return Completed{}, err
	}
	have, err := s.st.ListIdentities(ctx, userID)
	if err != nil {
		return Completed{}, err
	}
	if slices.ContainsFunc(have, func(i store.Identity) bool { return i.Provider == ProviderLocal }) {
		return Completed{}, ErrAlreadyLocal
	}
	ident.UserID = userID
	if err := s.st.LinkIdentity(ctx, ident); err != nil {
		if errors.Is(err, store.ErrIdentityExists) {
			return Completed{}, ErrIdentityTaken
		}
		return Completed{}, err
	}
	linked, err := s.st.GetIdentity(ctx, ProviderLocal, ident.Subject)
	if err != nil {
		return Completed{}, err
	}
	u, err := s.st.GetUser(ctx, userID)
	if err != nil {
		return Completed{}, err
	}
	s.logger.InfoContext(ctx, "identity linked", "user_id", userID, "provider", ProviderLocal)
	return Completed{Intent: IntentLink, User: u, Identity: linked}, nil
}

// newLocalIdentity checks a username and password and hashes the password: the identity to
// register or link, its credential on it.
func (s *Service) newLocalIdentity(ctx context.Context, username, password string, policy LocalPolicy) (store.Identity, error) {
	subject, err := CanonicalUsername(username)
	if err != nil {
		return store.Identity{}, err
	}
	if err := checkPassword(subject, password, policy.minPassword()); err != nil {
		return store.Identity{}, err
	}
	if _, err := s.st.GetIdentity(ctx, ProviderLocal, subject); err == nil {
		return store.Identity{}, ErrIdentityTaken
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.Identity{}, err
	}
	hash, err := HashPassword(password, s.pwParams, s.rand)
	if err != nil {
		return store.Identity{}, err
	}
	now := s.now()
	return store.Identity{Provider: ProviderLocal, Subject: subject, DisplayName: subject, VerificationMethod: MethodPassword, VerifiedAt: now,
		Credential: &store.LocalCredential{PasswordHash: hash, PasswordUpdatedAt: now}}, nil
}
