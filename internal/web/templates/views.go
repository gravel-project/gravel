// Package templates holds the hub's pages as templ components and the view models they render.
// The handlers in internal/web build the models from the hub's own API; nothing here reads a
// database or knows a domain type. Run `make generate` after editing a .templ file; the
// generated *_templ.go files are committed and CI fails on drift.
package templates

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

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
	// Games are the per-game overlays by game id (gravel#66).
	Games map[string]GameTheme
}

// GameTheme is a game's overlay: its accent in each scheme (empty keeps the organization's), a
// banner and links, on the game's pages.
type GameTheme struct {
	LightAccent string
	DarkAccent  string
	BannerURL   string
	BannerAlt   string
	Nav         []NavLink
}

// Game is the game a page belongs to, with its overlay: the page's content renders inside it.
type Game struct {
	ID        string
	Name      string
	BannerURL string
	BannerAlt string
	Nav       []NavLink
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
	// Demo is a demo hub's (gravel#112): every page says its data is made up.
	Demo bool
}

// DemoNotice is what every page of a demo hub says, above everything else.
const DemoNotice = "Demo data: these members, matches and stats are made up"

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
	// Game is set on a game's pages (a server, its moderation pages, a board scoped to the game
	// or one of its servers); nil elsewhere.
	Game *Game
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
	// Apps are the first-party apps the member allowed (ADR-0015), each with a revoke button.
	Apps []AppGrant
}

// Scope is a scope an app holds or asks for, with what it lets the app do, in words.
type Scope struct {
	Name        string
	Description string
}

// AppGrant is an app the member allowed: which, what, when.
type AppGrant struct {
	ClientID  string
	Name      string
	Scopes    []Scope
	GrantedAt time.Time
}

// AuthorizationParams are an app's authorization request, carried through the consent form.
type AuthorizationParams struct {
	ResponseType        string
	ClientID            string
	RedirectURI         string
	Scope               string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
}

// ConsentPage asks the member whether an app may act as them (ADR-0015 §3).
type ConsentPage struct {
	Page
	AppName string
	Scopes  []Scope
	// Wider is set when the member allowed the app before and it now asks for more.
	Wider bool
	// ReturnHost is where the answer sends the member: the redirect URI's host.
	ReturnHost string
	Params     AuthorizationParams
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

// PlayerRow is one player on a server, as members see them. Subject is their id at the game's
// identity provider, which the moderation forms name them by.
type PlayerRow struct {
	Name    string
	Subject string
	Team    string
	Kills   int
	Deaths  int
	PingMs  int
	Member  bool
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
	// NextMap is the rotation's next map as the game names it; empty when unknown.
	NextMap string
	// Mod is set for the owner and moderators: the controls the server's driver offers.
	Mod *ModControls
}

// ModControls are the moderation controls a server page shows a moderator; each is shown only
// when the server's driver offers it.
type ModControls struct {
	Kick, Ban, Unban, Message, Broadcast bool
	Bans                                 []BanRow
	BansNote                             string
}

// Any reports whether a player row needs a moderation form.
func (m *ModControls) Any() bool { return m != nil && (m.Kick || m.Ban || m.Message) }

// BanRow is one ban a server holds.
type BanRow struct {
	Subject string
	Name    string
	Reason  string
	By      string
	At      time.Time
}

// ConfirmPage asks a moderator to confirm an action before it is sent.
type ConfirmPage struct {
	Page
	ServerID   string
	ServerName string
	Action     string
	Question   string
	Subject    string
	Name       string
	Reason     string
	Message    string
}

// AuditRow is one entry of a server's moderation log.
type AuditRow struct {
	At      time.Time
	Actor   string
	Action  string
	Target  string
	Detail  string
	Outcome string
	OK      bool
}

// AuditPage is a server's moderation log, newest first.
type AuditPage struct {
	Page
	ServerID   string
	ServerName string
	Entries    []AuditRow
	NextURL    string
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

// RecapMatch is one match in a session recap, its times of day in the organization's zone.
type RecapMatch struct {
	Started string
	// Ended is empty while the match is on.
	Ended   string
	Map     string
	Players int
	Teams   []Team
}

// RecapPage is a session recap (ADR-0018): what one server's window held.
type RecapPage struct {
	Page
	ServerID   string
	ServerName string
	BoardURL   string
	// Window is the window's dates and times in the organization's zone.
	Window string
	// Note explains a private recap; nothing else is shown with it.
	Note string
	// Floor is the player count MinutesAtFloor counts against; 0 shows no such line.
	Floor          int
	PeakPlayers    int
	Minutes        int
	MinutesAtFloor int
	Players        int
	Matches        []RecapMatch
	// Top is the top five by kills, shown as the leaderboards show players.
	Top []BoardRow
}

// Nobody reports whether nobody was on during the window.
func (p RecapPage) Nobody() bool { return p.PeakPlayers == 0 && p.Players == 0 }

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
	// Participation is one table per kind the member took part in (ADR-0014), rows by window.
	Participation []KindTable
}

// KindTable is a member's participation of one kind, a row per window.
type KindTable struct {
	Label string
	Rows  []KindTotalsRow
}

// KindTotalsRow is a member's participation of one kind over one window.
type KindTotalsRow struct {
	Label string
	Times int
	Time  string
}

// ParticipationRow is one ranked player on a participation board.
type ParticipationRow struct {
	Rank         int
	Name         string
	Pseudonymous bool
	// UserID is set for a member shown by name; their name links to their profile.
	UserID string
	Time   string
	Times  int
}

// ParticipationPage is a participation board (ADR-0014) with its kind and window.
type ParticipationPage struct {
	Page
	Kinds   []Option
	Windows []Option
	// Kind is the ranked kind's label, the table's caption.
	Kind string
	Rows []ParticipationRow
	// Range is the window's dates, empty for all time.
	Range string
	// Note explains a private or invalid board.
	Note string
	// Empty is a hub where nothing has been recorded yet.
	Empty   bool
	NextURL string
}

// KindLabel is a participation kind as a page shows it: underscores as spaces, the first letter
// upper-case ("event_attendance" is "Event attendance").
func KindLabel(kind string) string {
	s := strings.ReplaceAll(kind, "_", " ")
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
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
