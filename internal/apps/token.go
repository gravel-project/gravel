package apps

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// Form bodies at the token endpoint are small; anything larger is not a token request.
const maxTokenBody = 64 << 10

// TokenHandler serves the token endpoint (RFC 6749 §4.4, client credentials): a POST form with
// grant_type=client_credentials, the client authenticated with HTTP Basic (RFC 6749 §2.3.1) or
// client_id and client_secret in the body, and an optional space-separated scope. The answer is
// the JSON of RFC 6749 §5.1 with a Bearer token, or the error JSON of §5.2: invalid_client (401,
// with a Basic challenge), invalid_scope, unsupported_grant_type, invalid_request.
func (s *Service) TokenHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			tokenError(w, http.StatusMethodNotAllowed, "invalid_request", "POST a form to this endpoint")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxTokenBody)
		if err := r.ParseForm(); err != nil {
			tokenError(w, http.StatusBadRequest, "invalid_request", "the body is not a form")
			return
		}
		clientID, secret, basic := r.BasicAuth()
		if basic {
			// RFC 6749 §2.3.1: the id and the secret are form-encoded before Basic encoding.
			if id, err := url.QueryUnescape(clientID); err == nil {
				clientID = id
			}
			if sec, err := url.QueryUnescape(secret); err == nil {
				secret = sec
			}
		}
		formID, formSecret := r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
		switch {
		case basic && (formID != "" && formID != clientID || formSecret != ""):
			tokenError(w, http.StatusBadRequest, "invalid_request", "client credentials were given twice")
			return
		case !basic:
			clientID, secret = formID, formSecret
		}
		if clientID == "" || secret == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="gravel"`)
			tokenError(w, http.StatusUnauthorized, "invalid_client", "client authentication is required")
			return
		}
		switch grant := r.PostForm.Get("grant_type"); grant {
		case "client_credentials":
		case "":
			tokenError(w, http.StatusBadRequest, "invalid_request", "grant_type is required")
			return
		default:
			tokenError(w, http.StatusBadRequest, "unsupported_grant_type", "only client_credentials is supported")
			return
		}
		tok, err := s.Issue(r.Context(), clientID, secret, strings.Fields(r.PostForm.Get("scope")))
		switch {
		case errors.Is(err, ErrInvalidClient):
			w.Header().Set("WWW-Authenticate", `Basic realm="gravel"`)
			tokenError(w, http.StatusUnauthorized, "invalid_client", "unknown client, revoked app, or wrong secret")
			return
		case errors.Is(err, ErrInvalidScope):
			tokenError(w, http.StatusBadRequest, "invalid_scope", err.Error())
			return
		case err != nil:
			s.logger.ErrorContext(r.Context(), "token endpoint", "error", err.Error())
			tokenError(w, http.StatusInternalServerError, "server_error", "could not issue a token")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": tok.Value,
			"token_type":   "Bearer",
			"expires_in":   int64(tok.ExpiresAt.Sub(tok.IssuedAt).Seconds()),
			"scope":        strings.Join(tok.Scopes, " "),
		})
	})
}

func tokenError(w http.ResponseWriter, code int, kind, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": kind, "error_description": description})
}
