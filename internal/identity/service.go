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
	"slices"
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
	ListUsers(ctx context.Context, orgID uuid.UUID, after *store.UserCursor, limit int) ([]store.User, error)
	ListIdentitiesForUsers(ctx context.Context, userIDs []uuid.UUID) ([]store.Identity, error)
	ListIdentityEventsAfter(ctx context.Context, afterID int64, limit int) ([]store.IdentityEvent, error)
	IdentityEventsHead(ctx context.Context) (int64, error)
	SetUserRole(ctx context.Context, userID uuid.UUID, role string, granted bool, by *uuid.UUID, at time.Time) (store.User, error)
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
	orgName    string

	now  func() time.Time
	rand io.Reader
}

// Option tunes a Service.
type Option func(*Service)

// WithClock replaces the clock; tests use it to expire attempts.
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithOrganizationName is the name a Publisher shows as the platform (Discord's connection on a
// member's profile).
func WithOrganizationName(name string) Option { return func(s *Service) { s.orgName = name } }

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

// Begin starts a login, link or roles attempt with a provider. For IntentLink, userID is the
// logged-in user the identity will be added to. IntentRoles is a login that also publishes, so
// the provider must allow login and be a Publisher. ErrUnknownProvider, ErrLoginNotAllowed for a
// link-only provider used to log in, ErrNotPublisher, ErrProviderFailed when the provider cannot
// start.
func (s *Service) Begin(ctx context.Context, provider string, intent Intent, userID *uuid.UUID) (Begun, error) {
	reg, ok := s.byName[provider]
	if !ok {
		return Begun{}, ErrUnknownProvider
	}
	begin := reg.Provider.Begin
	switch intent {
	case IntentLogin:
		if !reg.Login {
			return Begun{}, ErrLoginNotAllowed
		}
		userID = nil
	case IntentRoles:
		pub, ok := reg.Provider.(Publisher)
		if !ok {
			return Begun{}, ErrNotPublisher
		}
		if !reg.Login {
			return Begun{}, ErrLoginNotAllowed
		}
		begin, userID = pub.BeginPublish, nil
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
	authURL, psess, err := begin(ctx, state)
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
	// For IntentRoles: what was published, or why it was not. The login stands either way.
	Published  *Profile
	PublishErr error
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
	if (intent == IntentLogin || intent == IntentRoles) && !reg.Login {
		return Completed{}, ErrLoginNotAllowed
	}

	var acct Account
	var publish func(context.Context, Profile) error
	if intent == IntentRoles {
		pub, ok := reg.Provider.(Publisher)
		if !ok {
			return Completed{}, ErrNotPublisher
		}
		acct, publish, err = pub.CompletePublish(ctx, a.ProviderSession, params)
	} else {
		acct, err = reg.Provider.Complete(ctx, a.ProviderSession, params)
	}
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
	case IntentRoles:
		done, err := s.login(ctx, acct, now)
		if err != nil {
			return Completed{}, err
		}
		done.Intent = IntentRoles
		s.publish(ctx, &done, publish)
		return done, nil
	default:
		return s.login(ctx, acct, now)
	}
}

// publish writes the member's Profile with the grant the roles flow just obtained. A failure is
// recorded on done, not returned: the member did log in.
func (s *Service) publish(ctx context.Context, done *Completed, publish func(context.Context, Profile) error) {
	idents, err := s.st.ListIdentities(ctx, done.User.ID)
	if err != nil {
		done.PublishErr = err
		return
	}
	p := Profile{Organization: s.orgName, DisplayName: done.User.DisplayName}
	for _, i := range idents {
		p.Providers = append(p.Providers, i.Provider)
	}
	if publish == nil {
		done.PublishErr = fmt.Errorf("%w: %s returned nothing to publish with", ErrProviderFailed, done.Identity.Provider)
		return
	}
	if err := publish(ctx, p); err != nil {
		if !errors.Is(err, ErrProviderFailed) {
			err = fmt.Errorf("%w: %s: %w", ErrProviderFailed, done.Identity.Provider, err)
		}
		done.PublishErr = err
		s.logger.WarnContext(ctx, "linked accounts not published", "user_id", done.User.ID, "provider", done.Identity.Provider, "error", err.Error())
		return
	}
	done.Published = &p
	s.logger.InfoContext(ctx, "linked accounts published", "user_id", done.User.ID, "provider", done.Identity.Provider, "providers", len(p.Providers))
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

// ErrUnknownRole is a role the hub does not have (store.Roles lists them).
var ErrUnknownRole = errors.New("identity: no such role")

// SetRole grants or revokes a member's role (ADR-0013); by is who did it, recorded with the
// change. The API allows it to the owner only.
func (s *Service) SetRole(ctx context.Context, userID uuid.UUID, role string, granted bool, by uuid.UUID) (store.User, error) {
	if !slices.Contains(store.Roles, role) {
		return store.User{}, ErrUnknownRole
	}
	return s.st.SetUserRole(ctx, userID, role, granted, &by, s.now())
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

// ListUsers pages through the organization's members with their identities, oldest first;
// after is the previous page's last user, nil for the first page. This is the full pass a
// role-sync reconciler makes (ADR-0008).
func (s *Service) ListUsers(ctx context.Context, after *store.UserCursor, limit int) ([]User, error) {
	users, err := s.st.ListUsers(ctx, s.orgID, after, limit)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, len(users))
	for i, u := range users {
		ids[i] = u.ID
	}
	idents, err := s.st.ListIdentitiesForUsers(ctx, ids)
	if err != nil {
		return nil, err
	}
	byUser := map[uuid.UUID][]store.Identity{}
	for _, id := range idents {
		byUser[id.UserID] = append(byUser[id.UserID], id)
	}
	out := make([]User, len(users))
	for i, u := range users {
		out[i] = User{User: u, Identities: byUser[u.ID]}
	}
	return out, nil
}

// ListEvents returns identity events after a position in the log, oldest first, and the log's
// newest id: the incremental pass of role sync. A reader remembers the last id it saw, and one
// that starts with a full pass starts reading at the head.
func (s *Service) ListEvents(ctx context.Context, afterID int64, limit int) (events []store.IdentityEvent, head int64, err error) {
	// The head first: an event appended between the two reads is then in the page or after
	// the head, never skipped by a reader that jumps to the head.
	if head, err = s.st.IdentityEventsHead(ctx); err != nil {
		return nil, 0, err
	}
	if events, err = s.st.ListIdentityEventsAfter(ctx, afterID, limit); err != nil {
		return nil, 0, err
	}
	return events, head, nil
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
