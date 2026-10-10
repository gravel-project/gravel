// Package apps is the service-credentials slice of first-party app support (ADR-0008, the part
// of gravel#6 a bot needs): registered apps that authenticate with a client id and a secret,
// hold short-lived opaque bearer tokens, and act within scopes. The secret and the tokens are
// stored as SHA-256 hashes, like session tokens, so a copy of the tables yields no credential.
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
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/store"
)

// ScopeIdentityRead admits the identity reads a role-sync reconciler makes: LookupUser,
// ListUsers and ListIdentityEvents.
const ScopeIdentityRead = "identity:read"

// ScopeServersRead admits who is on a server (ListServerPlayers); the rest of ServerService is
// public (ADR-0010).
const ScopeServersRead = "servers:read"

// ScopeServersModerate admits ModerationService: acting on a server's players, its bans and the
// audit log (ADR-0010 §5).
const ScopeServersModerate = "servers:moderate"

// Scopes is every scope an app may hold, with what it admits.
var Scopes = map[string]string{
	ScopeIdentityRead:    "look up members and their identities (LookupUser, ListUsers, ListIdentityEvents)",
	ScopeServersRead:     "see who is on a server: names and provider identities (ListServerPlayers)",
	ScopeServersModerate: "kick, ban, unban, message, broadcast to and move a server's players, list its bans and read the audit log (ModerationService)",
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
}

// Service registers apps, issues their tokens and resolves a token to its app.
type Service struct {
	st       Store
	orgID    uuid.UUID
	tokenTTL time.Duration
	logger   *slog.Logger

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

// Create registers an app with a name and scopes. The client id and the secret are random; the
// secret comes back once, in Created, and only its hash is kept.
func (s *Service) Create(ctx context.Context, name string, scopes []string) (Created, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > MaxNameLength {
		return Created{}, fmt.Errorf("%w: a name is required, at most %d characters", ErrInvalidName, MaxNameLength)
	}
	scopes, err := normalizeScopes(scopes, nil)
	if err != nil {
		return Created{}, err
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
		SecretHash: hash(secret), Scopes: scopes, CreatedAt: s.now(),
	}
	if err := s.st.CreateApp(ctx, a); err != nil {
		return Created{}, err
	}
	s.logger.Info("app registered", "app", a.Name, "client_id", a.ClientID, "scopes", a.Scopes)
	return Created{App: a, Secret: secret}, nil
}

// List returns every app, oldest first, revoked ones included.
func (s *Service) List(ctx context.Context) ([]store.App, error) {
	return s.st.ListApps(ctx, s.orgID)
}

// Revoke ends an app: its tokens are deleted and no new one is issued. store.ErrNotFound for an
// unknown client id or one already revoked.
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
	a, err := s.st.GetAppByClientID(ctx, clientID)
	if errors.Is(err, store.ErrNotFound) {
		return Token{}, ErrInvalidClient
	}
	if err != nil {
		return Token{}, err
	}
	if a.RevokedAt != nil || subtle.ConstantTimeCompare(hash(secret), a.SecretHash) != 1 {
		return Token{}, ErrInvalidClient
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

// App is the principal a bearer token resolves to: the app and the scopes of that token.
type App struct {
	ID       uuid.UUID
	ClientID string
	Name     string
	Scopes   []string
}

// Has reports whether the token carries a scope.
func (a App) Has(scope string) bool { return slices.Contains(a.Scopes, scope) }

// Authenticate resolves a bearer token to its app, or ErrInvalidToken.
func (s *Service) Authenticate(ctx context.Context, token string) (App, error) {
	if token == "" {
		return App{}, ErrInvalidToken
	}
	t, a, err := s.st.GetAppTokenByHash(ctx, hash(token), s.now())
	if errors.Is(err, store.ErrNotFound) {
		return App{}, ErrInvalidToken
	}
	if err != nil {
		return App{}, err
	}
	return App{ID: a.ID, ClientID: a.ClientID, Name: a.Name, Scopes: t.Scopes}, nil
}

// Prune deletes expired tokens and reports how many.
func (s *Service) Prune(ctx context.Context) (int64, error) {
	return s.st.DeleteExpiredAppTokens(ctx, s.now())
}

type ctxKey struct{}

// NewContext returns ctx carrying an app principal.
func NewContext(ctx context.Context, a App) context.Context {
	return context.WithValue(ctx, ctxKey{}, a)
}

// FromContext returns the app principal Middleware resolved, if any.
func FromContext(ctx context.Context) (App, bool) {
	a, ok := ctx.Value(ctxKey{}).(App)
	return a, ok
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
		if _, known := Scopes[sc]; !known {
			return nil, fmt.Errorf("%w: %q is not a scope", ErrInvalidScope, sc)
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
