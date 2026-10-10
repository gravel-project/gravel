package apps

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// Form bodies at the token and introspection endpoints are small; anything larger is not a
// request for them.
const maxTokenBody = 64 << 10

// Grant types the token endpoint knows, as the metrics name them.
const (
	GrantClientCredentials = "client_credentials"
	GrantAuthorizationCode = "authorization_code"
)

// TokenHandler serves the token endpoint: a POST form, the client authenticated with HTTP Basic
// (RFC 6749 §2.3.1) or client_id and client_secret in the body, and one of two grants:
//   - grant_type=client_credentials (RFC 6749 §4.4, ADR-0008) with an optional space-separated
//     scope: a token for the app itself;
//   - grant_type=authorization_code (RFC 6749 §4.1.3, ADR-0015) with code, redirect_uri and
//     code_verifier (RFC 7636, required): a member's token, for an hour, with no refresh token.
//
// The answer is the JSON of RFC 6749 §5.1 with a Bearer token, or the error JSON of §5.2:
// invalid_client (401, with a Basic challenge), invalid_grant, invalid_scope,
// unsupported_grant_type, invalid_request.
func (s *Service) TokenHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		grant := "other"
		result := func(res string) { s.metrics.grant(grant, res) }
		clientID, secret, ok := readClient(w, r)
		if !ok {
			result("invalid_request")
			return
		}
		switch g := r.PostForm.Get("grant_type"); g {
		case GrantClientCredentials, GrantAuthorizationCode:
			grant = g
		case "":
			result("invalid_request")
			tokenError(w, http.StatusBadRequest, "invalid_request", "grant_type is required")
			return
		default:
			result("unsupported_grant_type")
			tokenError(w, http.StatusBadRequest, "unsupported_grant_type", "only client_credentials and authorization_code are supported")
			return
		}
		if clientID == "" || secret == "" {
			result("invalid_client")
			unauthorizedClient(w, "client authentication is required")
			return
		}
		var tok Token
		var err error
		if grant == GrantClientCredentials {
			tok, err = s.Issue(r.Context(), clientID, secret, strings.Fields(r.PostForm.Get("scope")))
		} else {
			f := r.PostForm
			missing := ""
			for _, k := range []string{"code", "redirect_uri", "code_verifier"} {
				if f.Get(k) == "" {
					missing = k
					break
				}
			}
			if missing != "" {
				result("invalid_request")
				tokenError(w, http.StatusBadRequest, "invalid_request", missing+" is required")
				return
			}
			tok, err = s.Exchange(r.Context(), clientID, secret, f.Get("code"), f.Get("redirect_uri"), f.Get("code_verifier"))
		}
		switch {
		case errors.Is(err, ErrInvalidClient):
			result("invalid_client")
			unauthorizedClient(w, "unknown client, revoked app, or wrong secret")
			return
		case errors.Is(err, ErrInvalidScope):
			result("invalid_scope")
			tokenError(w, http.StatusBadRequest, "invalid_scope", err.Error())
			return
		case errors.Is(err, ErrCodeReplayed):
			result("replayed")
			tokenError(w, http.StatusBadRequest, "invalid_grant", strings.TrimPrefix(err.Error(), ErrInvalidGrant.Error()+": "))
			return
		case errors.Is(err, ErrInvalidGrant):
			result("invalid_grant")
			tokenError(w, http.StatusBadRequest, "invalid_grant", strings.TrimPrefix(err.Error(), ErrInvalidGrant.Error()+": "))
			return
		case err != nil:
			result("error")
			s.logger.ErrorContext(r.Context(), "token endpoint", "error", err.Error())
			tokenError(w, http.StatusInternalServerError, "server_error", "could not issue a token")
			return
		}
		result("issued")
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

// readClient answers a request that is not a POST form, and reads the client's credentials from
// HTTP Basic or the body. It answers and reports false when the request is malformed: not a POST,
// not a form, or credentials given twice. Missing credentials are the caller's to refuse.
func readClient(w http.ResponseWriter, r *http.Request) (clientID, secret string, ok bool) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		tokenError(w, http.StatusMethodNotAllowed, "invalid_request", "POST a form to this endpoint")
		return "", "", false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxTokenBody)
	if err := r.ParseForm(); err != nil {
		tokenError(w, http.StatusBadRequest, "invalid_request", "the body is not a form")
		return "", "", false
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
		return "", "", false
	case !basic:
		clientID, secret = formID, formSecret
	}
	return clientID, secret, true
}

func unauthorizedClient(w http.ResponseWriter, description string) {
	w.Header().Set("WWW-Authenticate", `Basic realm="gravel"`)
	tokenError(w, http.StatusUnauthorized, "invalid_client", description)
}

func tokenError(w http.ResponseWriter, code int, kind, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": kind, "error_description": description})
}
