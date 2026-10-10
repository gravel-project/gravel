package apps

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// IntrospectionMaxAge is how long an app may cache an introspection answer, at most: until the
// token's exp or this long, whichever is sooner (ADR-0015 §5). Revoking an app, a grant or a
// member's sessions takes effect within it.
const IntrospectionMaxAge = 60 * time.Second

// Introspection is the answer of RFC 7662 §2.2 for a token. An inactive token's answer is
// {"active": false} and nothing else, so it says nothing about why.
type Introspection struct {
	Active bool
	// Sub is the member's user id for a member's token; empty for an app's own.
	Sub      string
	ClientID string
	Scope    string
	// Exp is the token's expiry in seconds since the epoch.
	Exp int64
}

// MarshalJSON writes {"active": false} for an inactive token, else the fields the ADR names.
func (i Introspection) MarshalJSON() ([]byte, error) {
	if !i.Active {
		return []byte(`{"active":false}`), nil
	}
	out := map[string]any{"active": true, "client_id": i.ClientID, "scope": i.Scope, "exp": i.Exp, "token_type": "Bearer"}
	if i.Sub != "" {
		out["sub"] = i.Sub
	}
	return json.Marshal(out)
}

// Introspect says whether a token is active: unknown, expired, revoked and another
// organization's tokens are all just inactive.
func (s *Service) Introspect(ctx context.Context, token string) (Introspection, error) {
	t, a, err := s.lookupToken(ctx, token)
	if errors.Is(err, ErrInvalidToken) {
		return Introspection{}, nil
	}
	if err != nil {
		return Introspection{}, err
	}
	in := Introspection{Active: true, ClientID: a.ClientID, Scope: strings.Join(t.Scopes, " "), Exp: t.ExpiresAt.Unix()}
	if t.UserID != nil {
		in.Sub = t.UserID.String()
	}
	return in, nil
}

// IntrospectHandler serves POST /oauth/introspect (RFC 7662), for apps only: the caller
// authenticates with its client credentials, as at the token endpoint, and names the token in the
// form's token field (token_type_hint is accepted and ignored: there is one kind). Without valid
// credentials it answers 401 invalid_client and introspects nothing.
func (s *Service) IntrospectHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientID, secret, ok := readClient(w, r)
		if !ok {
			s.metrics.introspection("invalid_request")
			return
		}
		if clientID == "" || secret == "" {
			s.metrics.introspection("invalid_client")
			unauthorizedClient(w, "client authentication is required")
			return
		}
		if _, err := s.authenticateClient(r.Context(), clientID, secret); err != nil {
			if errors.Is(err, ErrInvalidClient) {
				s.metrics.introspection("invalid_client")
				unauthorizedClient(w, "unknown client, revoked app, or wrong secret")
				return
			}
			s.metrics.introspection("error")
			s.logger.ErrorContext(r.Context(), "introspection endpoint", "error", err.Error())
			tokenError(w, http.StatusInternalServerError, "server_error", "could not introspect")
			return
		}
		token := r.PostForm.Get("token")
		if token == "" {
			s.metrics.introspection("invalid_request")
			tokenError(w, http.StatusBadRequest, "invalid_request", "token is required")
			return
		}
		in, err := s.Introspect(r.Context(), token)
		if err != nil {
			s.metrics.introspection("error")
			s.logger.ErrorContext(r.Context(), "introspection endpoint", "error", err.Error())
			tokenError(w, http.StatusInternalServerError, "server_error", "could not introspect")
			return
		}
		if in.Active {
			s.metrics.introspection("active")
		} else {
			s.metrics.introspection("inactive")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		_ = json.NewEncoder(w).Encode(in)
	})
}
