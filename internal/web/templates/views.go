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
	// Moderator holds the moderator role (ADR-0013); the owner moderates without it.
	Moderator bool
}

// CanModerate reports whether the member may moderate servers.
func (u *User) CanModerate() bool { return u != nil && (u.Owner || u.Moderator) }

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

// ServerRow is one game server as the server list and page show it.
type ServerRow struct {
	ID         string
	Name       string
	Game       string
	Location   string
	Official   bool
	State      string // the hub's state word: ok, unreachable, unknown, …
	StateLabel string // what a reader sees: Online, Offline, Checking, Unavailable
	Players    int
	MaxPlayers int
	Map        string
}

// Online reports whether the server answered its last poll.
func (s ServerRow) Online() bool { return s.State == "ok" }

// ServersPage lists the organization's servers.
type ServersPage struct {
	Page
	Servers []ServerRow
}

// Team is a team's score on a server.
type Team struct {
	Name  string
	Score int64
}

// PlayerRow is one player on a server, as members see them.
type PlayerRow struct {
	Name   string
	Team   string
	Kills  int
	Deaths int
	PingMs int
	Member bool
}

// MatchRow is one match in a server's history.
type MatchRow struct {
	Started time.Time
	Ended   *time.Time
	Map     string
	Players int
	Kills   int
}

// ServerPage is one server: its status, who is on, its recent matches.
type ServerPage struct {
	Page
	Server     ServerRow
	ObservedAt *time.Time
	Build      string
	Teams      []Team
	// Players is set for members; PlayersNote says why it is not, when it is not.
	Players     []PlayerRow
	PlayersNote string
	Matches     []MatchRow
	MatchesNote string
	BoardURL    string
}

// Option is one choice of a select.
type Option struct {
	Value    string
	Label    string
	Selected bool
}

// BoardColumn is a board column: a metric, its header, and the link that ranks by it.
type BoardColumn struct {
	Metric string
	Label  string
	Sorted bool
	URL    string
}

// BoardRow is one ranked player; Cells line up with the page's columns.
type BoardRow struct {
	Rank         int
	Name         string
	Pseudonymous bool
	// UserID is set for a member shown by name; their name links to their profile.
	UserID string
	Cells  []string
}

// BoardsPage is a leaderboard with its scope, window and ranking.
type BoardsPage struct {
	Page
	Heading string
	Scopes  []Option
	Windows []Option
	Metric  string
	Columns []BoardColumn
	Rows    []BoardRow
	// Range is the window's dates, empty for all time.
	Range string
	// Note explains an empty or private board.
	Note    string
	NextURL string
}

// TotalsRow is a member's totals over one window, as the profile shows them.
type TotalsRow struct {
	Label   string
	Kills   int
	Deaths  int
	KD      string
	Time    string
	Matches int
}

// PlayedMatch is one match a member played.
type PlayedMatch struct {
	Started  time.Time
	Live     bool
	ServerID string
	Server   string
	Map      string
	Kills    int
	Deaths   int
	Time     string
}

// MemberPage is a member's stats profile; Self is the member reading their own.
type MemberPage struct {
	Page
	UserID      string
	DisplayName string
	Self        bool
	ShowName    bool
	Totals      []TotalsRow
	Recent      []PlayedMatch
}

// MemberRow is one member on the owner's Members page.
type MemberRow struct {
	ID          string
	DisplayName string
	Accounts    string
	Joined      time.Time
	Owner       bool
	Moderator   bool
}

// MembersPage is the owner's list of members, where roles are granted.
type MembersPage struct {
	Page
	Members []MemberRow
	NextURL string
}
