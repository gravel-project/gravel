package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/store"
)

// Store is what the service needs from the database.
type Store interface {
	GetUser(ctx context.Context, id uuid.UUID) (store.User, error)
	RegisterUser(ctx context.Context, u store.User, ident store.Identity) (store.User, error)
	LinkIdentity(ctx context.Context, ident store.Identity) error
	GetIdentity(ctx context.Context, provider, subject string) (store.Identity, error)
	ListIdentities(ctx context.Context, userID uuid.UUID) ([]store.Identity, error)
	RecordLogin(ctx context.Context, provider, subject, displayName, avatarURL string, at time.Time) (store.Identity, error)
	RefreshIdentity(ctx context.Context, provider, subject, displayName, avatarURL string) (store.Identity, error)
	UnlinkIdentity(ctx context.Context, userID uuid.UUID, provider, subject string, at time.Time) error
	CreateAuthAttempt(ctx context.Context, a store.AuthAttempt) error
	ConsumeAuthAttempt(ctx context.Context, hash []byte, now time.Time) (store.AuthAttempt, error)
	DeleteExpiredAuthAttempts(ctx context.Context, now time.Time) (int64, error)
}

// Registration is a provider and what it may be used for.
type Registration struct {
	Provider Provider
	// Login says whether a member can sign in (and register) with it. A provider that is only
	// linked, never logged in with, proves an identity without owning the account.
	Login bool
}

// Service is the identity domain: the login and link flows, the account view, and lookups.
type Service struct {
	st         Store
	orgID      uuid.UUID
	providers  []Registration
	byName     map[string]Registration
	attemptTTL time.Duration
	logger     *slog.Logger

	now  func() time.Time
	rand io.Reader
}

// Option tunes a Service.
type Option func(*Service)

// WithClock replaces the clock; tests use it to expire attempts.
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// New builds a service. orgID is the organization new users belong to (the built-in one);
// attemptTTL bounds how long a member has to come back from a provider.
func New(st Store, orgID uuid.UUID, providers []Registration, attemptTTL time.Duration, logger *slog.Logger, opts ...Option) *Service {
	s := &Service{st: st, orgID: orgID, attemptTTL: attemptTTL, logger: logger, byName: map[string]Registration{}, now: time.Now, rand: rand.Reader}
	for _, r := range providers {
		s.byName[r.Provider.Name()] = r
		s.providers = append(s.providers, r)
	}
	sort.Slice(s.providers, func(i, j int) bool { return s.providers[i].Provider.Name() < s.providers[j].Provider.Name() })
	for _, o := range opts {
		o(s)
	}
	return s
}

// Providers lists the registered providers, sorted by name.
func (s *Service) Providers() []Registration { return s.providers }

// Provider looks a provider up by name.
func (s *Service) Provider(name string) (Registration, bool) {
	r, ok := s.byName[name]
	return r, ok
}

// Begun is a started attempt: where to send the browser, and the token to bind it with.
type Begun struct {
	AuthURL      string
	AttemptToken string // for the auth cookie; the callback presents it
	ExpiresAt    time.Time
}

// Begin starts a login or link attempt with a provider. For IntentLink, userID is the logged-in
// user the identity will be added to. ErrUnknownProvider, ErrLoginNotAllowed for a link-only
// provider used to log in, ErrProviderFailed when the provider cannot start.
func (s *Service) Begin(ctx context.Context, provider string, intent Intent, userID *uuid.UUID) (Begun, error) {
	reg, ok := s.byName[provider]
	if !ok {
		return Begun{}, ErrUnknownProvider
	}
	switch intent {
	case IntentLogin:
		if !reg.Login {
			return Begun{}, ErrLoginNotAllowed
		}
		userID = nil
	case IntentLink:
		if userID == nil {
			return Begun{}, errors.New("identity: a link attempt needs a user")
		}
	default:
		return Begun{}, fmt.Errorf("identity: unknown intent %q", intent)
	}
	state, err := s.random()
	if err != nil {
		return Begun{}, err
	}
	token, err := s.random()
	if err != nil {
		return Begun{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Begun{}, fmt.Errorf("identity: new id: %w", err)
	}
	authURL, psess, err := reg.Provider.Begin(ctx, state)
	if err != nil {
		if errors.Is(err, ErrProviderFailed) {
			return Begun{}, err
		}
		return Begun{}, fmt.Errorf("%w: %s: %w", ErrProviderFailed, provider, err)
	}
	now := s.now()
	a := store.AuthAttempt{
		ID: id, TokenHash: hashToken(token), Provider: provider, Intent: string(intent), UserID: userID,
		State: state, ProviderSession: psess, CreatedAt: now, ExpiresAt: now.Add(s.attemptTTL),
	}
	if err := s.st.CreateAuthAttempt(ctx, a); err != nil {
		return Begun{}, err
	}
	return Begun{AuthURL: authURL, AttemptToken: token, ExpiresAt: a.ExpiresAt}, nil
}

// Completed is the outcome of a finished attempt.
type Completed struct {
	Intent     Intent
	User       store.User
	Identity   store.Identity
	Registered bool // a login that created the user
}

// Complete finishes the attempt the browser's auth cookie names, with the provider's callback
// parameters. sessionUser is the logged-in user, if any; a link attempt must complete in the
// session that started it. The attempt is consumed before the provider is asked, so a replayed
// callback finds nothing (ErrAttemptInvalid). Other errors: ErrStateMismatch, ErrWrongUser,
// ErrUnknownProvider, ErrLoginNotAllowed, ErrProviderDenied, ErrProviderFailed, ErrIdentityTaken.
func (s *Service) Complete(ctx context.Context, attemptToken, provider string, params url.Values, sessionUser *uuid.UUID) (Completed, error) {
	if attemptToken == "" {
		return Completed{}, ErrAttemptInvalid
	}
	now := s.now()
	a, err := s.st.ConsumeAuthAttempt(ctx, hashToken(attemptToken), now)
	if errors.Is(err, store.ErrNotFound) {
		return Completed{}, ErrAttemptInvalid
	}
	if err != nil {
		return Completed{}, err
	}
	if a.Provider != provider {
		return Completed{}, ErrAttemptInvalid
	}
	if got := params.Get("state"); got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(a.State)) != 1 {
		return Completed{}, ErrStateMismatch
	}
	reg, ok := s.byName[provider]
	if !ok {
		return Completed{}, ErrUnknownProvider
	}
	intent := Intent(a.Intent)
	if intent == IntentLink && (sessionUser == nil || a.UserID == nil || *sessionUser != *a.UserID) {
		return Completed{}, ErrWrongUser
	}
	if intent == IntentLogin && !reg.Login {
		return Completed{}, ErrLoginNotAllowed
	}

	acct, err := reg.Provider.Complete(ctx, a.ProviderSession, params)
	if err != nil {
		if errors.Is(err, ErrProviderDenied) || errors.Is(err, ErrProviderFailed) {
			return Completed{}, err
		}
		return Completed{}, fmt.Errorf("%w: %s: %w", ErrProviderFailed, provider, err)
	}
	if acct.Subject == "" || acct.Provider != provider {
		return Completed{}, fmt.Errorf("%w: %s returned no usable account", ErrProviderFailed, provider)
	}

	switch intent {
	case IntentLink:
		return s.link(ctx, *a.UserID, acct)
	default:
		return s.login(ctx, acct, now)
	}
}

func (s *Service) login(ctx context.Context, acct Account, now time.Time) (Completed, error) {
	existing, err := s.st.GetIdentity(ctx, acct.Provider, acct.Subject)
	switch {
	case err == nil:
		return s.recordLogin(ctx, existing.UserID, acct, now)
	case !errors.Is(err, store.ErrNotFound):
		return Completed{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Completed{}, fmt.Errorf("identity: new id: %w", err)
	}
	name := acct.DisplayName
	if name == "" {
		name = acct.Subject
	}
	u, err := s.st.RegisterUser(ctx, store.User{ID: id, OrganizationID: s.orgID, DisplayName: name}, s.identity(acct, now))
	if errors.Is(err, store.ErrIdentityExists) {
		// A concurrent first login won the race: this one is a login of that user.
		existing, err := s.st.GetIdentity(ctx, acct.Provider, acct.Subject)
		if err != nil {
			return Completed{}, err
		}
		return s.recordLogin(ctx, existing.UserID, acct, now)
	}
	if err != nil {
		return Completed{}, err
	}
	ident, err := s.st.GetIdentity(ctx, acct.Provider, acct.Subject)
	if err != nil {
		return Completed{}, err
	}
	s.logger.InfoContext(ctx, "user registered", "user_id", u.ID, "provider", acct.Provider)
	return Completed{Intent: IntentLogin, User: u, Identity: ident, Registered: true}, nil
}

func (s *Service) recordLogin(ctx context.Context, userID uuid.UUID, acct Account, now time.Time) (Completed, error) {
	ident, err := s.st.RecordLogin(ctx, acct.Provider, acct.Subject, acct.DisplayName, acct.AvatarURL, now)
	if err != nil {
		return Completed{}, err
	}
	u, err := s.st.GetUser(ctx, userID)
	if err != nil {
		return Completed{}, err
	}
	s.logger.InfoContext(ctx, "user logged in", "user_id", u.ID, "provider", acct.Provider)
	return Completed{Intent: IntentLogin, User: u, Identity: ident}, nil
}

func (s *Service) link(ctx context.Context, userID uuid.UUID, acct Account) (Completed, error) {
	now := s.now()
	existing, err := s.st.GetIdentity(ctx, acct.Provider, acct.Subject)
	switch {
	case err == nil && existing.UserID != userID:
		return Completed{}, ErrIdentityTaken
	case err == nil:
		ident, err := s.st.RefreshIdentity(ctx, acct.Provider, acct.Subject, acct.DisplayName, acct.AvatarURL)
		if err != nil {
			return Completed{}, err
		}
		u, err := s.st.GetUser(ctx, userID)
		if err != nil {
			return Completed{}, err
		}
		return Completed{Intent: IntentLink, User: u, Identity: ident}, nil
	case !errors.Is(err, store.ErrNotFound):
		return Completed{}, err
	}
	ident := s.identity(acct, now)
	ident.UserID = userID
	if err := s.st.LinkIdentity(ctx, ident); err != nil {
		if errors.Is(err, store.ErrIdentityExists) {
			return Completed{}, ErrIdentityTaken
		}
		return Completed{}, err
	}
	linked, err := s.st.GetIdentity(ctx, acct.Provider, acct.Subject)
	if err != nil {
		return Completed{}, err
	}
	u, err := s.st.GetUser(ctx, userID)
	if err != nil {
		return Completed{}, err
	}
	s.logger.InfoContext(ctx, "identity linked", "user_id", u.ID, "provider", acct.Provider)
	return Completed{Intent: IntentLink, User: u, Identity: linked}, nil
}

func (s *Service) identity(acct Account, now time.Time) store.Identity {
	return store.Identity{
		Provider: acct.Provider, Subject: acct.Subject, DisplayName: acct.DisplayName, AvatarURL: acct.AvatarURL,
		VerificationMethod: acct.Method, VerifiedAt: now,
	}
}

// User is a user with their linked identities.
type User struct {
	store.User
	Identities []store.Identity
}

// Me returns a user and their identities; store.ErrNotFound for an unknown id.
func (s *Service) Me(ctx context.Context, userID uuid.UUID) (User, error) {
	u, err := s.st.GetUser(ctx, userID)
	if err != nil {
		return User{}, err
	}
	ids, err := s.st.ListIdentities(ctx, userID)
	if err != nil {
		return User{}, err
	}
	return User{User: u, Identities: ids}, nil
}

// Lookup resolves a (provider, subject) pair to its user and their identities;
// store.ErrNotFound when the pair is not linked. This is what role sync asks.
func (s *Service) Lookup(ctx context.Context, provider, subject string) (User, error) {
	ident, err := s.st.GetIdentity(ctx, provider, subject)
	if err != nil {
		return User{}, err
	}
	return s.Me(ctx, ident.UserID)
}

// Unlink removes one of a user's identities. ErrLastIdentity when it is the only one (a
// provider-only account would be unreachable), ErrNotLinked when the pair is not the user's.
func (s *Service) Unlink(ctx context.Context, userID uuid.UUID, provider, subject string) error {
	err := s.st.UnlinkIdentity(ctx, userID, provider, subject, s.now())
	switch {
	case errors.Is(err, store.ErrLastIdentity):
		return ErrLastIdentity
	case errors.Is(err, store.ErrNotFound):
		return ErrNotLinked
	case err != nil:
		return err
	}
	s.logger.InfoContext(ctx, "identity unlinked", "user_id", userID, "provider", provider)
	return nil
}

// Prune deletes expired attempts and reports how many.
func (s *Service) Prune(ctx context.Context) (int64, error) {
	return s.st.DeleteExpiredAuthAttempts(ctx, s.now())
}

func (s *Service) random() (string, error) {
	raw := make([]byte, 32)
	if _, err := io.ReadFull(s.rand, raw); err != nil {
		return "", fmt.Errorf("identity: random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
