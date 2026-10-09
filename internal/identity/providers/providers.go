// Package providers implements identity.Provider for the accounts a member can log in or link
// with: Discord over OAuth2 and Steam over OpenID 2.0.
//
// Both build on Goth (README principle 1) for the parts it gets right, the authorization URL and
// the OpenID verification, and step around the two places it gets in the way. Goth's Discord
// token exchange runs with oauth2.NoContext and ignores the provider's HTTP client, so it has no
// timeout, no cancellation and no seam for a test; its Steam profile fetch calls the Web API over
// plain HTTP with the key in the URL and cannot run without a key. Those two calls are made here
// instead, through the caller's client, so every network call a login makes is bounded and
// testable. Nothing here keeps state between Begin and Complete beyond the session string the
// interface already carries.
package providers

import (
	"net/http"
	"time"
	"unicode/utf8"
)

// defaultTimeout bounds every call to a provider when the caller supplies no client: a provider
// outage must fail a login, not hold a hub request open.
const defaultTimeout = 15 * time.Second

// maxErrorCode bounds what an error code echoed back from a provider contributes to an error
// message. The callback's query is the member's input, so it is capped rather than trusted.
const maxErrorCode = 64

func httpClient(client *http.Client) *http.Client {
	if client != nil {
		return client
	}
	return &http.Client{Timeout: defaultTimeout}
}

// clip truncates s to maxErrorCode bytes on a rune boundary.
func clip(s string) string {
	if len(s) <= maxErrorCode {
		return s
	}
	cut := maxErrorCode
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
