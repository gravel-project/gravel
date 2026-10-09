// Package identity is the hub's users and their linked identities: who a member is, which
// provider accounts prove it, and the login and linking flows that create those links.
//
// A linked identity is a (provider, subject) pair, unique at the database level (gravel#5). A
// member logs in with a provider that allows login (Discord, the primary) and links further
// providers (Steam) while logged in. Every flow is an attempt: a row bound to the browser by a
// cookie and to the provider's callback by a state value, consumed exactly once, so a replayed
// or forged callback finds nothing to complete. Stats are keyed by (provider, subject), never by
// the user id; a user is what the pairs resolve to at read time (README principle 3).
package identity

import (
	"context"
	"errors"
	"net/url"
)

// Verification methods, recorded on every identity so trust can depend on how it was proven.
const (
	MethodOAuth2 = "oauth2" // the member signed in at the provider and granted the hub a token
	MethodOpenID = "openid" // the provider asserted the subject over OpenID 2.0 (Steam)
)

// Intent is what an authentication attempt is for.
type Intent string

// Intents.
const (
	IntentLogin Intent = "login" // sign in, registering the user on the first visit
	IntentLink  Intent = "link"  // add a provider account to the logged-in user
)

// Errors the API and the pages map to responses.
var (
	ErrUnknownProvider = errors.New("unknown provider")
	ErrLoginNotAllowed = errors.New("this provider can be linked but not used to log in")
	ErrAttemptInvalid  = errors.New("no authentication is in progress for this browser, or it expired or was already used")
	ErrStateMismatch   = errors.New("the callback's state does not match the attempt")
	ErrWrongUser       = errors.New("the link was started by a different user than the one logged in")
	ErrProviderDenied  = errors.New("the provider did not authorize the request")
	ErrProviderFailed  = errors.New("the provider could not complete the authentication")
	ErrIdentityTaken   = errors.New("that account is already linked to another user")
	ErrLastIdentity    = errors.New("the last linked identity cannot be unlinked")
	ErrNotLinked       = errors.New("that identity is not linked to this user")
)

// Provider is one login or linking provider behind a browser redirect: Discord, Steam, later
// Patreon or Xbox. Implementations live in internal/identity/providers.
type Provider interface {
	// Name is the provider key stored on identities: "discord", "steam".
	Name() string
	// DisplayName is what a page calls it: "Discord".
	DisplayName() string
	// Begin starts an authentication. state is a random value the callback must echo in its
	// "state" query parameter; the provider carries it through the redirect (OAuth2's state
	// parameter, or the OpenID return_to URL). It returns the URL to send the browser to and an
	// opaque session the provider needs again at Complete.
	Begin(ctx context.Context, state string) (authURL, session string, err error)
	// Complete finishes an authentication from the callback's query parameters and returns the
	// provider's account. The caller has already matched the state. A refusal by the member at
	// the provider is ErrProviderDenied; any other failure wraps ErrProviderFailed.
	Complete(ctx context.Context, session string, params url.Values) (Account, error)
}

// Account is what a provider knows about the account that just authenticated.
type Account struct {
	Provider    string // the provider's Name
	Subject     string // the stable id at the provider: a Discord user id, a SteamID64
	DisplayName string // mutable and for display only: Discord's global name, Steam's persona name; may be empty
	AvatarURL   string // may be empty
	Method      string // MethodOAuth2 or MethodOpenID
}
