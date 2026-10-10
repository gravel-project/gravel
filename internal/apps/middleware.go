package apps

import (
	"errors"
	"net/http"
	"strings"
)

// Middleware resolves an Authorization: Bearer token and puts the principal in the request
// context: an App for an app's own token, a Member for a member's (ADR-0015). A request without a bearer token passes through untouched (a member's
// session, or anonymous). One with an unknown, expired or revoked token is answered 401 with the
// RFC 6750 challenge, so a service sees why rather than an unauthenticated procedure error; when
// the store cannot be read it is answered 503 rather than treated as anonymous.
func (s *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if len(auth) < 7 || !strings.EqualFold(auth[:7], "Bearer ") {
			next.ServeHTTP(w, r)
			return
		}
		p, err := s.Authenticate(r.Context(), strings.TrimSpace(auth[7:]))
		switch {
		case err == nil && p.IsMember():
			next.ServeHTTP(w, r.WithContext(NewMemberContext(r.Context(), Member{UserID: p.UserID, App: p.App})))
		case err == nil:
			next.ServeHTTP(w, r.WithContext(NewContext(r.Context(), p.App)))
		case errors.Is(err, ErrInvalidToken):
			w.Header().Set("WWW-Authenticate", `Bearer realm="gravel", error="invalid_token", error_description="the token is unknown, expired, or its app was revoked"`)
			tokenError(w, http.StatusUnauthorized, "invalid_token", "the token is unknown, expired, or its app was revoked")
		default:
			s.logger.ErrorContext(r.Context(), "app store unavailable", "error", err.Error())
			http.Error(w, "app store unavailable", http.StatusServiceUnavailable)
		}
	})
}
