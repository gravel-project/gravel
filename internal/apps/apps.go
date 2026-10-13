// Package apps is first-party app support (gravel#6). Registered apps authenticate with a client
// id and a secret, hold short-lived opaque bearer tokens and act within scopes: for themselves
// through client credentials (ADR-0008), or as a member who signed in to them through the
// authorization-code flow with PKCE (ADR-0015), and they check a token they are handed through
// introspection (RFC 7662). The secret, the codes and the tokens are stored as SHA-256 hashes,
// like session tokens, so a copy of the tables yields no credential, and the hub keeps no
// signing key.
package apps

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/authz"
	"github.com/gravel-project/gravel/internal/store"
)

// The capabilities an app may hold as scopes (ADR-0027): one vocabulary with members' roles,
// defined in internal/authz. These names are the ones apps were issued before capabilities.
const (
	ScopeIdentityRead       = string(authz.IdentityRead)
	ScopeServersRead        = string(authz.ServersRead)
	ScopeServersModerate    = string(authz.ServersModerate)
	ScopeServersLease       = string(authz.ServersLease)
	ScopeServersConfigure   = string(authz.ServersConfigure)
	ScopeServersOperate     = string(authz.ServersOperate)
	ScopeRolesGrant         = string(authz.RolesGrant)
	ScopeEventsRead         = string(authz.EventsRead)
	ScopeStatsRead          = string(authz.StatsRead)
	ScopeParticipationWrite = string(authz.ParticipationWrite)
)

// HostScopePrefix starts a host's own scope: a service beside the hub (a host's Muster, say)
// checks it in the tokens the hub issues and introspects; gravel admits nothing by it. The
// operator grants one with `apps create`, a member never does, and its shape is all gravel checks.
const HostScopePrefix = "host:"

// hostScope is a host scope's shape: "host:", then one to four lower-case segments joined by
// colons ("host:muster:write"), at most MaxHostScope characters.
var hostScope = regexp.MustCompile(`^host:[a-z0-9][a-z0-9_.-]*(:[a-z0-9][a-z0-9_.-]*){0,3}$`)

// MaxHostScope bounds a host scope's length.
const MaxHostScope = 64

// IsHostScope reports whether s is a well-formed host scope (HostScopePrefix).
func IsHostScope(s string) bool { return len(s) <= MaxHostScope && hostScope.MatchString(s) }

// Scopes is every scope an app may hold, with what it admits in the operator's words: the
// capabilities authz.Specs marks for apps. An app may also hold host scopes (IsHostScope), which
// gravel stores and carries and never checks.
var Scopes = func() map[string]string {
	m := map[string]string{}
	for _, s := range authz.Specs() {
		if s.Apps {
			m[string(s.Name)] = s.About
		}
	}
	return m
}()

// MemberScopes says what each scope lets an app do as a member, in the member's words: the
// consent page and the account page show these. A scope missing here is an app's own
// (participation:write) and is never granted by a member: the authorization endpoint refuses it.
var MemberScopes = map[string]string{
	ScopeIdentityRead:     "See the members of this hub and the accounts they linked",
	ScopeServersRead:      "See who is playing on this hub's servers",
	ScopeServersModerate:  "Moderate servers as you: kick, ban, message and move players, and read the moderation log, if you are a moderator",
	ScopeServersConfigure: "Change servers' configuration as you, if you may",
	ScopeServersOperate:   "Park and unpark servers as you, if you operate them",
	ScopeEventsRead:       "Read the hub's events log, as far as you may",
	ScopeStatsRead:        "Read leaderboards, match lists and members' stats",
}

// Limits.
const (
	MaxNameLength  = 64
	clientIDPrefix = "gravel_"
)

// Errors.
var (
	// ErrInvalidClient is an unknown client id, a revoked app, or a wrong secret: one error, so
	// a caller cannot tell which.
	ErrInvalidClient = errors.New("apps: invalid client")
	// ErrInvalidScope is a scope that does not exist or was not granted to the app.
	ErrInvalidScope = errors.New("apps: invalid scope")
	// ErrInvalidToken is a bearer token that is unknown, expired, or an app's that was revoked.
	ErrInvalidToken = errors.New("apps: invalid token")
	// ErrInvalidName is an empty or over-long app name.
	ErrInvalidName = errors.New("apps: invalid name")
	// ErrInvalidRedirectURI is a redirect URI an app may not register (ADR-0015), or one the app
	// did not register.
	ErrInvalidRedirectURI = errors.New("apps: invalid redirect uri")
)

// Store is what the service needs from the database.
type Store interface {
	CreateApp(ctx context.Context, a store.App) error
	GetApp(ctx context.Context, id uuid.UUID) (store.App, error)
	GetAppByClientID(ctx context.Context, clientID string) (store.App, error)
	ListApps(ctx context.Context, orgID uuid.UUID) ([]store.App, error)
	RevokeApp(ctx context.Context, id uuid.UUID, at time.Time) error
	CreateAppToken(ctx context.Context, t store.AppToken) error
	GetAppTokenByHash(ctx context.Context, hash []byte, now time.Time) (store.AppToken, store.App, error)
	DeleteExpiredAppTokens(ctx context.Context, now time.Time) (int64, error)

	CreateAuthorizationCode(ctx context.Context, c store.AuthorizationCode) error
	RedeemAuthorizationCode(ctx context.Context, hash []byte, now time.Time, issue func(store.AuthorizationCode) (store.AppToken, error)) (store.AuthorizationCode, error)
	DeleteExpiredAuthorizationCodes(ctx context.Context, before time.Time) (int64, error)
	GetAppGrant(ctx context.Context, userID, appID uuid.UUID) (store.AppGrant, error)
	SaveAppGrant(ctx context.Context, g store.AppGrant) error
	ListAppGrants(ctx context.Context, userID uuid.UUID) ([]store.AppGrant, error)
	DeleteAppGrant(ctx context.Context, userID, appID uuid.UUID) (int64, error)
}

// Service registers apps, issues their tokens and resolves a token to its app.
type Service struct {
	st       Store
	orgID    uuid.UUID
	tokenTTL time.Duration
	logger   *slog.Logger
	metrics  *metrics

	now  func() time.Time
	rand io.Reader
}

// Option tunes a Service.
type Option func(*Service)

// WithClock replaces the clock; tests use it to expire tokens.
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// New builds a service for the organization's apps; tokenTTL is how long an issued token lives.
func New(st Store, orgID uuid.UUID, tokenTTL time.Duration, logger *slog.Logger, opts ...Option) *Service {
	s := &Service{st: st, orgID: orgID, tokenTTL: tokenTTL, logger: logger, now: time.Now, rand: rand.Reader}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Created is a new registration. Secret is shown once and never stored.
type Created struct {
	App    store.App
	Secret string
}

// Create registers an app with a name, scopes and the redirect URIs its web pages sign members
// in through (none for a service that only uses client credentials). The client id and the
// secret are random; the secret comes back once, in Created, and only its hash is kept.
func (s *Service) Create(ctx context.Context, name string, scopes []string, redirectURIs ...string) (Created, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > MaxNameLength {
		return Created{}, fmt.Errorf("%w: a name is required, at most %d characters", ErrInvalidName, MaxNameLength)
	}
	scopes, err := normalizeScopes(scopes, nil)
	if err != nil {
		return Created{}, err
	}
	var uris []string
	for _, u := range redirectURIs {
		if err := ValidateRedirectURI(u); err != nil {
			return Created{}, err
		}
		if !slices.Contains(uris, u) {
			uris = append(uris, u)
		}
	}
	if len(uris) > maxRedirectURIs {
		return Created{}, fmt.Errorf("%w: at most %d redirect URIs", ErrInvalidRedirectURI, maxRedirectURIs)
	}
	idBytes := make([]byte, 12)
	if _, err := io.ReadFull(s.rand, idBytes); err != nil {
		return Created{}, fmt.Errorf("apps: random: %w", err)
	}
	secret, err := s.random()
	if err != nil {
		return Created{}, err
	}
	a := store.App{
		ID: uuid.New(), OrganizationID: s.orgID, Name: name, ClientID: clientIDPrefix + hex.EncodeToString(idBytes),
		SecretHash: hash(secret), Scopes: scopes, RedirectURIs: uris, CreatedAt: s.now(),
	}
	if err := s.st.CreateApp(ctx, a); err != nil {
		return Created{}, err
	}
	s.logger.Info("app registered", "app", a.Name, "client_id", a.ClientID, "scopes", a.Scopes, "redirect_uris", a.RedirectURIs)
	return Created{App: a, Secret: secret}, nil
}

// List returns every app, oldest first, revoked ones included.
func (s *Service) List(ctx context.Context) ([]store.App, error) {
	return s.st.ListApps(ctx, s.orgID)
}

// Revoke ends an app: its tokens (its own and its members'), the grants members gave it and its
// unexchanged codes are deleted, and no new token is issued. store.ErrNotFound for an unknown
// client id or one already revoked.
func (s *Service) Revoke(ctx context.Context, clientID string) error {
	a, err := s.st.GetAppByClientID(ctx, clientID)
	if err != nil {
		return err
	}
	if err := s.st.RevokeApp(ctx, a.ID, s.now()); err != nil {
		return err
	}
	s.logger.Info("app revoked", "app", a.Name, "client_id", a.ClientID)
	return nil
}

// Token is an issued bearer token. Value is shown to the client once; only its hash is kept.
type Token struct {
	Value     string
	IssuedAt  time.Time
	ExpiresAt time.Time
	Scopes    []string
}

// Issue authenticates a client and issues a token for the requested scopes, or for all of the
// app's scopes when none are requested. ErrInvalidClient for an unknown or revoked app or a
// wrong secret; ErrInvalidScope for a scope the app does not hold.
func (s *Service) Issue(ctx context.Context, clientID, secret string, requested []string) (Token, error) {
	a, err := s.authenticateClient(ctx, clientID, secret)
	if err != nil {
		return Token{}, err
	}
	scopes := slices.Clone(a.Scopes)
	if len(requested) > 0 {
		if scopes, err = normalizeScopes(requested, a.Scopes); err != nil {
			return Token{}, err
		}
	}
	value, err := s.random()
	if err != nil {
		return Token{}, err
	}
	now := s.now()
	t := store.AppToken{ID: uuid.New(), AppID: a.ID, TokenHash: hash(value), Scopes: scopes, CreatedAt: now, ExpiresAt: now.Add(s.tokenTTL)}
	if err := s.st.CreateAppToken(ctx, t); err != nil {
		return Token{}, err
	}
	return Token{Value: value, IssuedAt: now, ExpiresAt: t.ExpiresAt, Scopes: scopes}, nil
}

// authenticateClient checks a client's credentials: ErrInvalidClient for an unknown client, a
// revoked app, an app of another organization or a wrong secret, one error so a caller cannot
// tell which.
func (s *Service) authenticateClient(ctx context.Context, clientID, secret string) (store.App, error) {
	a, err := s.st.GetAppByClientID(ctx, clientID)
	if errors.Is(err, store.ErrNotFound) {
		return store.App{}, ErrInvalidClient
	}
	if err != nil {
		return store.App{}, err
	}
	if a.RevokedAt != nil || a.OrganizationID != s.orgID || subtle.ConstantTimeCompare(hash(secret), a.SecretHash) != 1 {
		return store.App{}, ErrInvalidClient
	}
	return a, nil
}

// App is the principal an app's own token resolves to: the app and the scopes of that token.
type App struct {
	ID       uuid.UUID
	ClientID string
	Name     string
	Scopes   []string
}

// Has reports whether the token carries a scope.
func (a App) Has(scope string) bool { return slices.Contains(a.Scopes, scope) }

// Member is the principal a member's token resolves to (ADR-0015 §4): the member, acting through
// the app, within the token's scopes (App.Scopes). It is never an App in a context: what a member
// may do is the member's own right, narrowed by the token, never the app's.
type Member struct {
	UserID uuid.UUID
	App    App
}

// Has reports whether the token carries a scope.
func (m Member) Has(scope string) bool { return m.App.Has(scope) }

// Principal is what a bearer token resolves to: the app, and the member when it is a member's
// token (UserID is uuid.Nil for an app's own).
type Principal struct {
	App
	UserID uuid.UUID
}

// IsMember reports whether the token is a member's.
func (p Principal) IsMember() bool { return p.UserID != uuid.Nil }

// Authenticate resolves a bearer token to its principal, or ErrInvalidToken for an unknown or
// expired token, a revoked app's, or another organization's.
func (s *Service) Authenticate(ctx context.Context, token string) (Principal, error) {
	t, a, err := s.lookupToken(ctx, token)
	if err != nil {
		return Principal{}, err
	}
	p := Principal{App: App{ID: a.ID, ClientID: a.ClientID, Name: a.Name, Scopes: t.Scopes}}
	if t.UserID != nil {
		p.UserID = *t.UserID
	}
	return p, nil
}

// lookupToken finds a live token of this organization, or ErrInvalidToken.
func (s *Service) lookupToken(ctx context.Context, token string) (store.AppToken, store.App, error) {
	if token == "" {
		return store.AppToken{}, store.App{}, ErrInvalidToken
	}
	t, a, err := s.st.GetAppTokenByHash(ctx, hash(token), s.now())
	if errors.Is(err, store.ErrNotFound) {
		return store.AppToken{}, store.App{}, ErrInvalidToken
	}
	if err != nil {
		return store.AppToken{}, store.App{}, err
	}
	if a.OrganizationID != s.orgID {
		return store.AppToken{}, store.App{}, ErrInvalidToken
	}
	return t, a, nil
}

// Prune deletes expired tokens, and the codes no token issued from can still be alive, and
// reports how many tokens went.
func (s *Service) Prune(ctx context.Context) (int64, error) {
	n, err := s.st.DeleteExpiredAppTokens(ctx, s.now())
	if err != nil {
		return n, err
	}
	if _, err := s.st.DeleteExpiredAuthorizationCodes(ctx, s.now().Add(-MemberTokenTTL)); err != nil {
		return n, err
	}
	return n, nil
}

type ctxKey struct{}

type memberKey struct{}

// NewContext returns ctx carrying an app principal.
func NewContext(ctx context.Context, a App) context.Context {
	return context.WithValue(ctx, ctxKey{}, a)
}

// FromContext returns the app principal Middleware resolved, if any. A member's token is never
// one: see MemberFromContext.
func FromContext(ctx context.Context) (App, bool) {
	a, ok := ctx.Value(ctxKey{}).(App)
	return a, ok
}

// NewMemberContext returns ctx carrying a member's-token principal.
func NewMemberContext(ctx context.Context, m Member) context.Context {
	return context.WithValue(ctx, memberKey{}, m)
}

// MemberFromContext returns the member's-token principal Middleware resolved, if any.
func MemberFromContext(ctx context.Context) (Member, bool) {
	m, ok := ctx.Value(memberKey{}).(Member)
	return m, ok
}

func (s *Service) random() (string, error) {
	raw := make([]byte, 32)
	if _, err := io.ReadFull(s.rand, raw); err != nil {
		return "", fmt.Errorf("apps: random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func hash(v string) []byte {
	sum := sha256.Sum256([]byte(v))
	return sum[:]
}

// normalizeScopes trims, de-duplicates and sorts scopes, refusing one that does not exist or,
// when granted is given, one the app was not granted.
func normalizeScopes(in, granted []string) ([]string, error) {
	out := make([]string, 0, len(in))
	for _, raw := range in {
		sc := strings.TrimSpace(raw)
		if sc == "" {
			continue
		}
		if _, known := Scopes[sc]; !known && !IsHostScope(sc) {
			return nil, fmt.Errorf("%w: %q is not a scope (gravel's, or a host's own as host:<name>[:<more>], lower-case, at most %d characters)", ErrInvalidScope, sc, MaxHostScope)
		}
		if granted != nil && !slices.Contains(granted, sc) {
			return nil, fmt.Errorf("%w: %q was not granted to this app", ErrInvalidScope, sc)
		}
		if !slices.Contains(out, sc) {
			out = append(out, sc)
		}
	}
	slices.Sort(out)
	return out, nil
}
