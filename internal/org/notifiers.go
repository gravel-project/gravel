package org

import (
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/gravel-project/gravel/internal/events"
)

// Notifier is one way the hub tells people what happened (ADR-0028 §5, gravel#196): it carries
// the events its filter takes from the events log to a webhook, an ntfy topic or email addresses.
// What it sends to and what it takes are here; its secrets (a webhook's signing key, a Discord
// webhook's URL, an ntfy access token, the SMTP password) are in hub.yaml's notify section.
// Notifiers are not public: GetOrganizationSettings shows them only to callers who may change the
// settings.
type Notifier struct {
	// Name is the notifier's id: its cursor in the log, its secret in hub.yaml, its metrics label.
	Name string `json:"name" yaml:"name"`
	// Kind is NotifierWebhook, NotifierNtfy or NotifierEmail.
	Kind string `json:"kind" yaml:"kind"`
	// Disabled keeps the notifier and its place in the log but sends nothing; TestNotifier still
	// reaches it.
	Disabled bool `json:"disabled,omitempty" yaml:"disabled,omitempty"`
	// URL is a webhook's endpoint (FlavourJSON only: a Discord webhook's URL holds its token, so it
	// is the notifier's secret) or an ntfy server ("" is DefaultNtfyServer). https, or http to
	// the loopback interface.
	URL string `json:"url,omitempty" yaml:"url,omitempty"`
	// Flavour is a webhook's body: FlavourJSON ("", the default) or FlavourDiscord.
	Flavour string `json:"flavour,omitempty" yaml:"flavour,omitempty"`
	// Topic is an ntfy topic. On a public server anyone who knows it can read it: protect it with
	// an access token or use a server of your own.
	Topic string `json:"topic,omitempty" yaml:"topic,omitempty"`
	// To are an email notifier's addresses.
	To []string `json:"to,omitempty" yaml:"to,omitempty"`
	// Events is which events the notifier takes.
	Events NotifierEvents `json:"events,omitzero" yaml:"events,omitempty"`
}

// NotifierEvents is a notifier's filter. Zero values take every warning and critical event.
type NotifierEvents struct {
	// Kinds are kinds ("server.unreachable") or groups ("server"); empty takes every kind.
	Kinds []string `json:"kinds,omitempty" yaml:"kinds,omitempty,flow"`
	// MinSeverity is the lowest severity taken: "" is DefaultNotifierSeverity.
	MinSeverity string `json:"min_severity,omitempty" yaml:"min_severity,omitempty"`
	// Games and Servers, when set, take only those games' or servers' events: an event about no
	// server (a role granted, a backup) passes neither.
	Games   []string `json:"games,omitempty" yaml:"games,omitempty,flow"`
	Servers []string `json:"servers,omitempty" yaml:"servers,omitempty,flow"`
}

// Notifier kinds and webhook flavours.
const (
	NotifierWebhook = "webhook"
	NotifierNtfy    = "ntfy"
	NotifierEmail   = "email"

	FlavourJSON    = "json"
	FlavourDiscord = "discord"

	DefaultNtfyServer = "https://ntfy.sh"
	// DefaultNotifierSeverity is what a notifier takes with no min_severity: what may need a
	// person.
	DefaultNotifierSeverity = events.Warning
)

// NotifierKinds are the kinds, in the order the docs list them.
var NotifierKinds = []string{NotifierWebhook, NotifierNtfy, NotifierEmail}

// Notifier limits.
const (
	MaxNotifiers        = 20
	MaxNotifierKinds    = 50
	MaxNotifierServers  = 50
	MaxNotifierGames    = MaxGames
	MaxNotifierAddrs    = 10
	maxNotifierNameRune = 32
)

var (
	notifierName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	// ntfyTopic is ntfy's own rule for a topic name.
	ntfyTopic = regexp.MustCompile(`^[-_A-Za-z0-9]{1,64}$`)
)

// MinSeverityOrDefault is the lowest severity the notifier takes.
func (e NotifierEvents) MinSeverityOrDefault() events.Severity {
	if e.MinSeverity == "" {
		return DefaultNotifierSeverity
	}
	return events.Severity(e.MinSeverity)
}

// FlavourOrDefault is a webhook's flavour.
func (n Notifier) FlavourOrDefault() string {
	if n.Flavour == "" {
		return FlavourJSON
	}
	return n.Flavour
}

// ServerOrDefault is an ntfy notifier's server.
func (n Notifier) ServerOrDefault() string {
	if n.URL == "" {
		return DefaultNtfyServer
	}
	return n.URL
}

// Notifier returns the named notifier.
func (s Settings) Notifier(name string) (Notifier, bool) {
	i := slices.IndexFunc(s.Notifiers, func(n Notifier) bool { return n.Name == name })
	if i < 0 {
		return Notifier{}, false
	}
	return s.Notifiers[i], true
}

func validateNotifiers(ns []Notifier, bad func(string, ...any)) {
	if len(ns) > MaxNotifiers {
		bad("notifiers: %d notifiers, at most %d", len(ns), MaxNotifiers)
	}
	seen := map[string]int{}
	for i, n := range ns {
		field := fmt.Sprintf("notifiers[%d]", i)
		if !notifierName.MatchString(n.Name) {
			bad("%s.name: %q is not a name (lower case letters, digits and hyphens, at most %d)", field, n.Name, maxNotifierNameRune)
		} else if j, dup := seen[n.Name]; dup {
			bad("%s.name: %q is already notifiers[%d]'s", field, n.Name, j)
		} else {
			seen[n.Name] = i
		}
		unused := func(key string, set bool) {
			if set {
				bad("%s.%s: %s notifiers have none", field, key, n.Kind)
			}
		}
		switch n.Kind {
		case NotifierWebhook:
			unused("topic", n.Topic != "")
			unused("to", len(n.To) > 0)
			switch n.FlavourOrDefault() {
			case FlavourJSON:
				if !endpointOK(n.URL) {
					bad("%s.url: %q must be an https URL (http only to the loopback interface), with no user or password", field, n.URL)
				}
			case FlavourDiscord:
				if n.URL != "" {
					bad("%s.url: a Discord webhook's URL holds its token, so it is a secret: put the whole URL in hub.yaml's notify.secrets.%s.file and leave url empty", field, n.Name)
				}
			default:
				bad("%s.flavour: %q is not %q or %q", field, n.Flavour, FlavourJSON, FlavourDiscord)
			}
		case NotifierNtfy:
			unused("flavour", n.Flavour != "")
			unused("to", len(n.To) > 0)
			if n.URL != "" && !endpointOK(n.URL) {
				bad("%s.url: %q must be an https URL (http only to the loopback interface), with no user or password", field, n.URL)
			}
			if !ntfyTopic.MatchString(n.Topic) {
				bad("%s.topic: %q is not an ntfy topic (letters, digits, - and _, at most 64)", field, n.Topic)
			}
		case NotifierEmail:
			unused("url", n.URL != "")
			unused("flavour", n.Flavour != "")
			unused("topic", n.Topic != "")
			switch c := len(n.To); {
			case c == 0:
				bad("%s.to: at least one address", field)
			case c > MaxNotifierAddrs:
				bad("%s.to: %d addresses, at most %d", field, c, MaxNotifierAddrs)
			}
			for j, a := range n.To {
				if p, err := mail.ParseAddress(a); err != nil || p.Address != a {
					bad("%s.to[%d]: %q is not a plain email address", field, j, a)
				}
			}
		default:
			bad("%s.kind: %q is not a notifier kind (%s)", field, n.Kind, strings.Join(NotifierKinds, ", "))
		}
		validateNotifierEvents(field+".events", n.Events, bad)
	}
}

func validateNotifierEvents(field string, e NotifierEvents, bad func(string, ...any)) {
	if len(e.Kinds) > MaxNotifierKinds {
		bad("%s.kinds: %d kinds, at most %d", field, len(e.Kinds), MaxNotifierKinds)
	}
	for i, k := range e.Kinds {
		if !kindOrGroup(k) {
			bad("%s.kinds[%d]: %q is neither an event kind nor a group of them (docs/hub.md, Events)", field, i, k)
		}
	}
	if e.MinSeverity != "" && events.Rank(events.Severity(e.MinSeverity)) < 0 {
		bad("%s.min_severity: %q is not info, notice, warning or critical", field, e.MinSeverity)
	}
	if len(e.Games) > MaxNotifierGames {
		bad("%s.games: %d games, at most %d", field, len(e.Games), MaxNotifierGames)
	}
	if len(e.Servers) > MaxNotifierServers {
		bad("%s.servers: %d servers, at most %d", field, len(e.Servers), MaxNotifierServers)
	}
	for i, s := range e.Servers {
		if !serverID.MatchString(s) {
			bad("%s.servers[%d]: %q is not a server id", field, i, s)
		}
	}
}

// kindOrGroup is a registered kind, or the group of one ("server" of "server.unreachable").
func kindOrGroup(k string) bool {
	if _, ok := events.Lookup(k); ok {
		return true
	}
	return slices.ContainsFunc(events.Kinds(), func(kind events.Kind) bool { return events.Group(kind.Name) == k })
}

// endpointOK is where a notifier may send: an https URL, or http to the loopback interface (a
// relay on the same host), never with a user or password in it.
func endpointOK(u string) bool {
	if len(u) > MaxURLLength {
		return false
	}
	p, err := url.Parse(u)
	if err != nil || p.Host == "" || p.User != nil {
		return false
	}
	switch p.Scheme {
	case "https":
		return true
	case "http":
		h := p.Hostname()
		if h == "localhost" {
			return true
		}
		ip := net.ParseIP(h)
		return ip != nil && ip.IsLoopback()
	}
	return false
}
