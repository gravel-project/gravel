// Package session is the hub's server-side browser sessions: an opaque random token in an
// httpOnly cookie, its SHA-256 in Postgres, a per-session CSRF token for forms, and the
// middleware that puts the session in the request context for the API and the pages.
//
// There is no signing key. The cookie value is random and only its hash is stored, so a copy
// of the table yields no usable cookie and the hub needs no secret for sessions. Logging out
// everywhere deletes the user's rows; a session is dead the moment its row is gone.
package session

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
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/store"
)

// Cookie purposes; the name is prefixed with "__Host-" when cookies are secure, which binds
// them to this origin and path.
const (
	CookieSession = "session"
	CookieAuth    = "auth"
	CookieFlash   = "flash"
)

// CSRFField is the form field (and header) that carries the session's CSRF token.
const (
	CSRFField  = "_csrf"
	CSRFHeader = "X-CSRF-Token"
)

// touchEvery bounds the writes a busy session causes: last_seen_at moves at most this often.
const touchEvery = 5 * time.Minute

// ErrNoSession is returned by Load when the request carries no valid session.
var ErrNoSession = errors.New("no session")

// Store is what the manager needs from the database.
type Store interface {
	CreateSession(ctx context.Context, s store.Session) error
	GetSessionByTokenHash(ctx context.Context, hash []byte, now time.Time) (store.Session, error)
	TouchSession(ctx context.Context, id uuid.UUID, lastSeen time.Time) error
	DeleteSession(ctx context.Context, id uuid.UUID) error
	DeleteUserSessions(ctx context.Context, userID uuid.UUID) (int64, error)
	DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error)
}

// Manager issues, loads and revokes sessions.
type Manager struct {
	st     Store
	ttl    time.Duration
	secure bool
	logger *slog.Logger

	now  func() time.Time
	rand io.Reader
}

// New builds a manager whose sessions live for ttl. secure says whether cookies get the Secure
// attribute and the __Host- prefix: true behind HTTPS, false for a plain-HTTP development hub.
func New(st Store, ttl time.Duration, secure bool, logger *slog.Logger) *Manager {
	return &Manager{st: st, ttl: ttl, secure: secure, logger: logger, now: time.Now, rand: rand.Reader}
}

// Secure reports whether cookies are secure.
func (m *Manager) Secure() bool { return m.secure }

// CookieName returns the cookie name for a purpose.
func (m *Manager) CookieName(purpose string) string {
	if m.secure {
		return "__Host-gravel_" + purpose
	}
	return "gravel_" + purpose
}

// Cookie builds a cookie with the attributes every hub cookie has: Path=/, HttpOnly,
// SameSite=Lax (the OAuth callbacks are top-level GET navigations, which Lax sends cookies on)
// and Secure when the hub is behind HTTPS. A non-positive maxAge deletes the cookie.
func (m *Manager) Cookie(purpose, value string, maxAge time.Duration) *http.Cookie {
	c := &http.Cookie{
		Name:     m.CookieName(purpose),
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   m.secure,
		SameSite: http.SameSiteLaxMode,
	}
	if maxAge <= 0 {
		c.MaxAge = -1
	} else {
		c.MaxAge = int(maxAge / time.Second)
	}
	return c
}

// Issue creates a session for a user and sets its cookie.
func (m *Manager) Issue(ctx context.Context, w http.ResponseWriter, userID uuid.UUID) (store.Session, error) {
	token, err := m.random()
	if err != nil {
		return store.Session{}, err
	}
	csrf, err := m.random()
	if err != nil {
		return store.Session{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return store.Session{}, fmt.Errorf("session: new id: %w", err)
	}
	now := m.now()
	s := store.Session{ID: id, UserID: userID, TokenHash: hash(token), CSRFToken: csrf, CreatedAt: now, ExpiresAt: now.Add(m.ttl), LastSeenAt: now}
	if err := m.st.CreateSession(ctx, s); err != nil {
		return store.Session{}, err
	}
	http.SetCookie(w, m.Cookie(CookieSession, token, m.ttl))
	return s, nil
}

// Load returns the request's session, touching last_seen_at at most every few minutes.
// ErrNoSession when there is no cookie, or it names no live session.
func (m *Manager) Load(ctx context.Context, r *http.Request) (store.Session, error) {
	c, err := r.Cookie(m.CookieName(CookieSession))
	if err != nil || c.Value == "" {
		return store.Session{}, ErrNoSession
	}
	now := m.now()
	s, err := m.st.GetSessionByTokenHash(ctx, hash(c.Value), now)
	if errors.Is(err, store.ErrNotFound) {
		return store.Session{}, ErrNoSession
	}
	if err != nil {
		return store.Session{}, err
	}
	if now.Sub(s.LastSeenAt) >= touchEvery {
		if err := m.st.TouchSession(ctx, s.ID, now); err != nil {
			m.logger.WarnContext(ctx, "could not touch session", "error", err.Error())
		} else {
			s.LastSeenAt = now
		}
	}
	return s, nil
}

// Revoke ends one session and clears its cookie.
func (m *Manager) Revoke(ctx context.Context, w http.ResponseWriter, s store.Session) error {
	m.Clear(w)
	return m.st.DeleteSession(ctx, s.ID)
}

// RevokeAll ends every session of a user and reports how many there were. The caller clears
// its own cookie.
func (m *Manager) RevokeAll(ctx context.Context, userID uuid.UUID) (int64, error) {
	return m.st.DeleteUserSessions(ctx, userID)
}

// Clear deletes the session cookie.
func (m *Manager) Clear(w http.ResponseWriter) {
	http.SetCookie(w, m.Cookie(CookieSession, "", 0))
}

// Prune deletes expired sessions and reports how many.
func (m *Manager) Prune(ctx context.Context) (int64, error) {
	return m.st.DeleteExpiredSessions(ctx, m.now())
}

// CheckCSRF reports whether a state-changing form or request carries the session's CSRF token,
// in the _csrf form field or the X-CSRF-Token header.
func (m *Manager) CheckCSRF(r *http.Request, s store.Session) bool {
	got := r.Header.Get(CSRFHeader)
	if got == "" {
		got = r.PostFormValue(CSRFField)
	}
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(s.CSRFToken)) == 1
}

type ctxKey struct{}

// Middleware loads the request's session, if any, into the context. A stale cookie is cleared.
// When the store cannot be read, a request that carries a session cookie is answered 503
// rather than treated as anonymous.
func (m *Manager) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, err := m.Load(r.Context(), r)
		switch {
		case err == nil:
			next.ServeHTTP(w, r.WithContext(NewContext(r.Context(), s)))
			return
		case errors.Is(err, ErrNoSession):
			if c, cerr := r.Cookie(m.CookieName(CookieSession)); cerr == nil && c.Value != "" {
				m.Clear(w)
			}
		default:
			m.logger.ErrorContext(r.Context(), "session store unavailable", "error", err.Error())
			http.Error(w, "session store unavailable", http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// NewContext returns ctx carrying s.
func NewContext(ctx context.Context, s store.Session) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

// FromContext returns the session Middleware loaded, if any.
func FromContext(ctx context.Context) (store.Session, bool) {
	s, ok := ctx.Value(ctxKey{}).(store.Session)
	return s, ok
}

func (m *Manager) random() (string, error) {
	raw := make([]byte, 32)
	if _, err := io.ReadFull(m.rand, raw); err != nil {
		return "", fmt.Errorf("session: random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func hash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
