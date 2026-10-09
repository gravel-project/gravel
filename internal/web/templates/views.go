// Package templates holds the hub's pages as templ components and the view models they render.
// The handlers in internal/web build the models from the hub's own API; nothing here reads a
// database or knows a domain type. Run `make generate` after editing a .templ file; the
// generated *_templ.go files are committed and CI fails on drift.
package templates

import "time"

// HTMXConfig is the htmx configuration the layout sets: requests to this origin only, no eval,
// no script tags from responses, no injected indicator styles (the CSP forbids inline styles),
// and error responses swapped in so a 4xx fragment shows its message.
const HTMXConfig = `{"selfRequestsOnly":true,"allowEval":false,"allowScriptTags":false,"includeIndicatorStyles":false,"historyCacheSize":0,"responseHandling":[{"code":"204","swap":false},{"code":"[23]..","swap":true},{"code":"[45]..","swap":true,"error":true}]}`

// Tokens are the colours one scheme uses; the stylesheet reads them as custom properties.
type Tokens struct {
	Accent     string
	Background string
	Foreground string
	Muted      string
	Line       string
	OK         string
	Err        string
}

// NavLink is a navigation extension a host adds: in the header or the footer, optionally only
// for a role ("" for everyone, "owner").
type NavLink struct {
	Label     string
	URL       string
	Placement string
	Role      string
}

// Theme is how an organization's pages look.
type Theme struct {
	Light      Tokens
	Dark       Tokens
	Font       string
	LogoURL    string
	FaviconURL string
	Nav        []NavLink
}

// Flash is a one-shot message: Kind is "ok" or "err".
type Flash struct {
	Kind string
	Text string
}

// Org is the organization as the pages show it.
type Org struct {
	Name  string
	Owned bool
}

// User is the logged-in member.
type User struct {
	ID          string
	DisplayName string
	Owner       bool
}

// Identity is one linked account as the account page shows it.
type Identity struct {
	Provider        string
	ProviderDisplay string
	Subject         string
	DisplayName     string
	AvatarURL       string
	Method          string
	VerifiedAt      time.Time
	LastLoginAt     *time.Time
	CanUnlink       bool
}

// Provider is a login or link provider a page offers.
type Provider struct {
	Name        string
	DisplayName string
}

// Page is what every page starts from.
type Page struct {
	Title     string
	Org       Org
	User      *User
	CSRF      string
	Flash     *Flash
	Theme     Theme
	HeaderNav []NavLink
	FooterNav []NavLink
}

// LoginPage lists the providers a member can sign in with.
type LoginPage struct {
	Page
	Providers []Provider
}

// AccountPage is the member's account.
type AccountPage struct {
	Page
	Identities    []Identity
	LinkProviders []Provider
	// RoleProviders are linked providers that show the member's linked accounts (Discord's
	// Linked Roles), each with an update button.
	RoleProviders []Provider
}

// ErrorPage explains a refusal or a failure.
type ErrorPage struct {
	Page
	Message string
}

// Label is the identity's display name, or its subject when the provider gave none.
func (i Identity) Label() string {
	if i.DisplayName != "" {
		return i.DisplayName
	}
	return i.Subject
}
