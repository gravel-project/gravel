package apps

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/httpx"
	"github.com/gravel-project/gravel/internal/store"
)

// The authorization-code flow (ADR-0015): RFC 6749 §4.1 with PKCE (RFC 7636, S256 only, required
// for every client), following the OAuth 2.0 Security BCP (RFC 9700). No implicit grant, no
// password grant, no plain PKCE, no partial redirect-URI matching, no refresh token, no ID token.

// Lifetimes the ADR fixes.
const (
	// CodeTTL is how long an authorization code may be exchanged.
	CodeTTL = 60 * time.Second
	// MemberTokenTTL is how long a member's token lives. There is no refresh token: the app sends
	// the member through the flow again, which is silent while their hub session lasts.
	MemberTokenTTL = time.Hour
)

// Limits on what an app sends.
const (
	MaxRedirectURILength = 2000
	maxRedirectURIs      = 10
	// A PKCE verifier is 43 to 128 characters (RFC 7636 §4.1); an S256 challenge is the 43
	// characters of an unpadded base64url SHA-256.
	minVerifierLength = 43
	maxVerifierLength = 128
	challengeLength   = 43
)

// Errors of the flow.
var (
	// ErrInvalidGrant is a code that is unknown, expired, used, issued to another client or for
	// another redirect URI, or presented with the wrong PKCE verifier (RFC 6749 §5.2).
	ErrInvalidGrant = errors.New("apps: invalid grant")
	// ErrCodeReplayed is a code presented a second time: the tokens issued from it are revoked.
	ErrCodeReplayed = fmt.Errorf("%w: the code was already used, and the tokens issued from it are revoked", ErrInvalidGrant)
)

// AuthorizationError is a refusal the app hears about at its redirect URI (RFC 6749 §4.1.2.1):
// the client and the redirect URI were good, the rest of the request was not.
type AuthorizationError struct {
	// Code is the RFC 6749 error code: invalid_request, unsupported_response_type,
	// invalid_scope.
	Code        string
	Description string
}

func (e *AuthorizationError) Error() string { return "apps: " + e.Code + ": " + e.Description }

// ValidateRedirectURI says whether an app may register a redirect URI: an absolute https URL, or
// http on a loopback host for development (127.0.0.1, [::1], localhost), with no fragment and no
// user information, at most MaxRedirectURILength characters. It is compared as an exact string
// at the authorization endpoint, so it is stored as given.
func ValidateRedirectURI(raw string) error {
	bad := func(why string) error { return fmt.Errorf("%w: %q %s", ErrInvalidRedirectURI, raw, why) }
	if raw == "" || len(raw) > MaxRedirectURILength {
		return bad(fmt.Sprintf("must be 1 to %d characters", MaxRedirectURILength))
	}
	if strings.ContainsFunc(raw, func(r rune) bool { return r <= ' ' || r == 0x7f || r == '\\' }) {
		return bad("must not contain spaces, control characters or backslashes")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return bad("is not a URL")
	}
	host := u.Hostname()
	switch {
	case u.Scheme == "https" && host != "":
	case u.Scheme == "http" && isLoopback(host):
	default:
		return bad("must be https, or http on 127.0.0.1, [::1] or localhost")
	}
	switch {
	case u.Opaque != "":
		return bad("is not a hierarchical URL")
	case u.User != nil:
		return bad("must not carry user information")
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return bad("must not have a fragment")
	}
	return nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback() && (ip.Equal(net.IPv4(127, 0, 0, 1)) || ip.Equal(net.IPv6loopback))
}

// AuthorizationRequest is what an app sends to the authorization endpoint (RFC 6749 §4.1.1 with
// RFC 7636 §4.3). The state is the page's to carry back; the hub never stores it.
type AuthorizationRequest struct {
	ResponseType        string
	ClientID            string
	RedirectURI         string
	Scope               string
	CodeChallenge       string
	CodeChallengeMethod string
}

// Authorization is a checked request: the app, the scopes it asks for, and whether the member
// must be asked (no remembered grant covers them).
type Authorization struct {
	App             store.App
	Scopes          []string
	ConsentRequired bool
}

// CheckAuthorization checks an authorization request for a member (uuid.Nil: nobody is logged in
// yet, and consent is required). ErrInvalidClient for an unknown or revoked client and
// ErrInvalidRedirectURI for a redirect URI the app did not register: neither may be redirected
// to. *AuthorizationError for anything else, which the page sends back to the app; the
// Authorization then names the app and nothing more.
func (s *Service) CheckAuthorization(ctx context.Context, req AuthorizationRequest, userID uuid.UUID) (Authorization, error) {
	if req.ClientID == "" {
		return Authorization{}, ErrInvalidClient
	}
	a, err := s.st.GetAppByClientID(ctx, req.ClientID)
	if errors.Is(err, store.ErrNotFound) {
		return Authorization{}, ErrInvalidClient
	}
	if err != nil {
		return Authorization{}, err
	}
	if a.RevokedAt != nil || a.OrganizationID != s.orgID {
		return Authorization{}, ErrInvalidClient
	}
	// Exact string comparison: no prefix, no normalisation, no default (RFC 9700 §4.1.3).
	if req.RedirectURI == "" || !slices.Contains(a.RedirectURIs, req.RedirectURI) {
		return Authorization{}, fmt.Errorf("%w: not one the app registered", ErrInvalidRedirectURI)
	}
	switch {
	case req.ResponseType == "":
		return Authorization{App: a}, &AuthorizationError{Code: "invalid_request", Description: "response_type is required"}
	case req.ResponseType != "code":
		return Authorization{App: a}, &AuthorizationError{Code: "unsupported_response_type", Description: "only response_type=code is supported"}
	case req.CodeChallenge == "":
		return Authorization{App: a}, &AuthorizationError{Code: "invalid_request", Description: "PKCE is required: send code_challenge with code_challenge_method=S256"}
	case req.CodeChallengeMethod != "S256":
		return Authorization{App: a}, &AuthorizationError{Code: "invalid_request", Description: "code_challenge_method must be S256"}
	case !isChallenge(req.CodeChallenge):
		return Authorization{App: a}, &AuthorizationError{Code: "invalid_request", Description: "code_challenge is not an S256 challenge"}
	}
	scopes, err := normalizeScopes(strings.Fields(req.Scope), a.Scopes)
	if err != nil {
		return Authorization{App: a}, &AuthorizationError{Code: "invalid_scope", Description: strings.TrimPrefix(err.Error(), ErrInvalidScope.Error()+": ")}
	}
	for _, sc := range scopes {
		if _, ok := MemberScopes[sc]; !ok {
			return Authorization{App: a}, &AuthorizationError{Code: "invalid_scope", Description: fmt.Sprintf("%q is the app's own; a member does not grant it", sc)}
		}
	}
	auth := Authorization{App: a, Scopes: scopes, ConsentRequired: true}
	if userID == uuid.Nil {
		return auth, nil
	}
	g, err := s.st.GetAppGrant(ctx, userID, a.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return Authorization{}, err
	default:
		auth.ConsentRequired = !covers(g.Scopes, scopes)
	}
	return auth, nil
}

// Authorize records the member's consent and issues a code: the request is checked again, the
// grant is remembered (widened to what was asked, never narrowed), and the code, valid for
// CodeTTL, is returned once. The page calls it when the member pressed Allow, or straight away
// when a remembered grant covers the request.
func (s *Service) Authorize(ctx context.Context, req AuthorizationRequest, userID uuid.UUID) (string, Authorization, error) {
	if userID == uuid.Nil {
		return "", Authorization{}, errors.New("apps: authorize needs a member")
	}
	auth, err := s.CheckAuthorization(ctx, req, userID)
	if err != nil {
		return "", Authorization{}, err
	}
	now := s.now()
	if auth.ConsentRequired {
		granted := slices.Clone(auth.Scopes)
		if g, err := s.st.GetAppGrant(ctx, userID, auth.App.ID); err == nil {
			for _, sc := range g.Scopes {
				if !slices.Contains(granted, sc) {
					granted = append(granted, sc)
				}
			}
			slices.Sort(granted)
		} else if !errors.Is(err, store.ErrNotFound) {
			return "", Authorization{}, err
		}
		by := store.Actor{UserID: &userID, Via: store.ViaAPI, RequestID: httpx.RequestIDFromContext(ctx)}
		if err := s.st.SaveAppGrant(ctx, store.AppGrant{UserID: userID, AppID: auth.App.ID, Scopes: granted, GrantedAt: now}, by); err != nil {
			return "", Authorization{}, err
		}
		s.logger.InfoContext(ctx, "app allowed", "app", auth.App.Name, "client_id", auth.App.ClientID, "user_id", userID, "scopes", granted)
	}
	code, err := s.random()
	if err != nil {
		return "", Authorization{}, err
	}
	c := store.AuthorizationCode{ID: uuid.New(), CodeHash: hash(code), AppID: auth.App.ID, UserID: userID, RedirectURI: req.RedirectURI,
		CodeChallenge: req.CodeChallenge, Scopes: auth.Scopes, CreatedAt: now, ExpiresAt: now.Add(CodeTTL)}
	if err := s.st.CreateAuthorizationCode(ctx, c); err != nil {
		return "", Authorization{}, err
	}
	return code, auth, nil
}

// Exchange trades an authorization code for a member's token (RFC 6749 §4.1.3): the client
// authenticates, and the code must be live, unused, issued to that client for exactly that
// redirect URI, and match the PKCE verifier. A code is spent by its first presentation, whatever
// the outcome; a second presentation revokes every token issued from it (ErrCodeReplayed).
func (s *Service) Exchange(ctx context.Context, clientID, secret, code, redirectURI, verifier string) (Token, error) {
	a, err := s.authenticateClient(ctx, clientID, secret)
	if err != nil {
		return Token{}, err
	}
	if code == "" {
		return Token{}, fmt.Errorf("%w: no code", ErrInvalidGrant)
	}
	now := s.now()
	var tok Token
	c, err := s.st.RedeemAuthorizationCode(ctx, hash(code), now, func(c store.AuthorizationCode) (store.AppToken, error) {
		switch {
		case c.AppID != a.ID:
			return store.AppToken{}, fmt.Errorf("%w: the code was issued to another client", ErrInvalidGrant)
		case !now.Before(c.ExpiresAt):
			return store.AppToken{}, fmt.Errorf("%w: the code expired", ErrInvalidGrant)
		case c.RedirectURI != redirectURI:
			return store.AppToken{}, fmt.Errorf("%w: redirect_uri is not the one the code was issued for", ErrInvalidGrant)
		case !verifyPKCE(verifier, c.CodeChallenge):
			return store.AppToken{}, fmt.Errorf("%w: code_verifier does not match the code_challenge", ErrInvalidGrant)
		}
		value, err := s.random()
		if err != nil {
			return store.AppToken{}, err
		}
		scopes := make([]string, 0, len(c.Scopes))
		for _, sc := range c.Scopes { // the app's scopes bound the token, whatever the code says
			if slices.Contains(a.Scopes, sc) {
				scopes = append(scopes, sc)
			}
		}
		uid := c.UserID
		tok = Token{Value: value, IssuedAt: now, ExpiresAt: now.Add(MemberTokenTTL), Scopes: scopes}
		return store.AppToken{ID: uuid.New(), AppID: a.ID, TokenHash: hash(value), Scopes: scopes, CreatedAt: now, ExpiresAt: tok.ExpiresAt, UserID: &uid}, nil
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		return Token{}, fmt.Errorf("%w: unknown code", ErrInvalidGrant)
	case errors.Is(err, store.ErrCodeReplayed):
		s.logger.WarnContext(ctx, "authorization code replayed: the tokens issued from it are revoked", "client_id", clientID, "app", a.Name, "code_client_matches", c.AppID == a.ID, "user_id", c.UserID)
		return Token{}, ErrCodeReplayed
	case err != nil:
		return Token{}, err
	}
	return tok, nil
}

// Grants are the apps a member allowed, oldest first; a revoked app's are gone.
func (s *Service) Grants(ctx context.Context, userID uuid.UUID) ([]store.AppGrant, error) {
	return s.st.ListAppGrants(ctx, userID)
}

// RevokeGrant withdraws a member's grant to an app, with the member's tokens for it and its codes
// not yet exchanged, and reports how many tokens went. store.ErrNotFound when the member had not
// allowed it.
func (s *Service) RevokeGrant(ctx context.Context, userID uuid.UUID, clientID string) (int64, error) {
	a, err := s.st.GetAppByClientID(ctx, clientID)
	if err != nil {
		return 0, err
	}
	if a.OrganizationID != s.orgID {
		return 0, store.ErrNotFound
	}
	n, err := s.st.DeleteAppGrant(ctx, userID, a.ID, s.now(), store.Actor{UserID: &userID, Via: store.ViaAPI, RequestID: httpx.RequestIDFromContext(ctx)})
	if err != nil {
		return 0, err
	}
	s.logger.InfoContext(ctx, "app grant revoked", "app", a.Name, "client_id", a.ClientID, "user_id", userID, "tokens", n)
	return n, nil
}

// covers reports whether a remembered grant includes every scope asked for.
func covers(granted, asked []string) bool {
	for _, sc := range asked {
		if !slices.Contains(granted, sc) {
			return false
		}
	}
	return true
}

// verifyPKCE checks a verifier against an S256 challenge (RFC 7636 §4.6), in constant time.
func verifyPKCE(verifier, challenge string) bool {
	if len(verifier) < minVerifierLength || len(verifier) > maxVerifierLength {
		return false
	}
	for _, r := range verifier {
		if !unreserved(r) {
			return false
		}
	}
	sum := sha256.Sum256([]byte(verifier))
	return subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(challenge)) == 1
}

func isChallenge(c string) bool {
	if len(c) != challengeLength {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(c)
	return err == nil
}

// unreserved is RFC 3986's unreserved set, the verifier's alphabet.
func unreserved(r rune) bool {
	return r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.' || r == '_' || r == '~'
}
