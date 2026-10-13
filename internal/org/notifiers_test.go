package org

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func htgNotifiers() []Notifier {
	return []Notifier{
		{Name: "john-phone", Kind: NotifierNtfy, Topic: "htg-ops", Events: NotifierEvents{Kinds: []string{"server", "backup.failed"}}},
		{Name: "ops-hook", Kind: NotifierWebhook, URL: "https://ops.example.com/gravel", Events: NotifierEvents{MinSeverity: "notice", Games: []string{"wardogs"}}},
		{Name: "discord", Kind: NotifierWebhook, Flavour: FlavourDiscord, Disabled: true},
		{Name: "mail", Kind: NotifierEmail, To: []string{"ops@example.com"}, Events: NotifierEvents{Servers: []string{"htg-wardogs-1"}}},
		{Name: "relay", Kind: NotifierNtfy, URL: "http://127.0.0.1:8090", Topic: "local"},
	}
}

func TestSettingsValidateNotifiers(t *testing.T) {
	if err := (Settings{Version: 1, Notifiers: htgNotifiers()}).Validate(); err != nil {
		t.Fatalf("good notifiers: %v", err)
	}
	with := func(i int, f func(*Notifier)) []Notifier {
		ns := htgNotifiers()
		f(&ns[i])
		return ns
	}
	cases := []struct {
		name string
		ns   []Notifier
		want string
	}{
		{"name", with(0, func(n *Notifier) { n.Name = "John Phone" }), `notifiers[0].name: "John Phone" is not a name`},
		{"duplicate", with(1, func(n *Notifier) { n.Name = "john-phone" }), `notifiers[1].name: "john-phone" is already notifiers[0]'s`},
		{"kind", with(0, func(n *Notifier) { n.Kind = "sms" }), `notifiers[0].kind: "sms" is not a notifier kind (webhook, ntfy, email)`},
		{"no topic", with(0, func(n *Notifier) { n.Topic = "" }), `notifiers[0].topic: "" is not an ntfy topic`},
		{"topic with a slash", with(0, func(n *Notifier) { n.Topic = "a/b" }), `notifiers[0].topic: "a/b" is not an ntfy topic`},
		{"ntfy over http", with(0, func(n *Notifier) { n.URL = "http://ntfy.example.com" }), `notifiers[0].url: "http://ntfy.example.com" must be an https URL`},
		{"to on ntfy", with(0, func(n *Notifier) { n.To = []string{"a@b.c"} }), `notifiers[0].to: ntfy notifiers have none`},
		{"webhook without url", with(1, func(n *Notifier) { n.URL = "" }), `notifiers[1].url: "" must be an https URL`},
		{"password in url", with(1, func(n *Notifier) { n.URL = "https://u:p@ops.example.com/" }), `notifiers[1].url: "https://u:p@ops.example.com/" must be an https URL (http only to the loopback interface), with no user or password`},
		{"a Discord webhook's URL", with(2, func(n *Notifier) { n.URL = "https://discord.com/api/webhooks/1/abc" }), `notifiers[2].url: a Discord webhook's URL holds its token, so it is a secret: put the whole URL in hub.yaml's notify.secrets.discord.file`},
		{"flavour", with(1, func(n *Notifier) { n.Flavour = "slack" }), `notifiers[1].flavour: "slack" is not "json" or "discord"`},
		{"topic on a webhook", with(1, func(n *Notifier) { n.Topic = "x" }), `notifiers[1].topic: webhook notifiers have none`},
		{"no addresses", with(3, func(n *Notifier) { n.To = nil }), `notifiers[3].to: at least one address`},
		{"an address with a name", with(3, func(n *Notifier) { n.To = []string{"Ops <ops@example.com>"} }), `notifiers[3].to[0]: "Ops <ops@example.com>" is not a plain email address`},
		{"url on email", with(3, func(n *Notifier) { n.URL = "https://x" }), `notifiers[3].url: email notifiers have none`},
		{"kind filter", with(0, func(n *Notifier) { n.Events.Kinds = []string{"server.exploded"} }), `notifiers[0].events.kinds[0]: "server.exploded" is neither an event kind nor a group of them`},
		{"severity", with(0, func(n *Notifier) { n.Events.MinSeverity = "loud" }), `notifiers[0].events.min_severity: "loud" is not info, notice, warning or critical`},
		{"server", with(3, func(n *Notifier) { n.Events.Servers = []string{"HTG 1"} }), `notifiers[3].events.servers[0]: "HTG 1" is not a server id`},
	}
	for _, c := range cases {
		err := (Settings{Version: 1, Notifiers: c.ns}).Validate()
		if !errors.Is(err, ErrInvalidSettings) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", c.name, c.want, err)
		}
	}
	many := make([]Notifier, MaxNotifiers+1)
	for i := range many {
		many[i] = Notifier{Name: "n" + strings.Repeat("x", i%3) + string(rune('a'+i)), Kind: NotifierNtfy, Topic: "t"}
	}
	if err := (Settings{Version: 1, Notifiers: many}).Validate(); err == nil || !strings.Contains(err.Error(), "notifiers: 21 notifiers, at most 20") {
		t.Errorf("too many: %v", err)
	}
	// A notifier's games are games this hub ships.
	if err := (Settings{Version: 1, Notifiers: htgNotifiers()}).CheckGames([]string{"cs2"}); err == nil || !strings.Contains(err.Error(), `notifiers[1].events.games[0]: "wardogs" is not a game this hub ships`) {
		t.Errorf("unknown game: %v", err)
	}
}

func TestNotifierDefaults(t *testing.T) {
	n := htgNotifiers()
	if n[0].Events.MinSeverityOrDefault() != "warning" || n[1].Events.MinSeverityOrDefault() != "notice" {
		t.Error("min severity")
	}
	if n[1].FlavourOrDefault() != FlavourJSON || n[2].FlavourOrDefault() != FlavourDiscord {
		t.Error("flavour")
	}
	if n[0].ServerOrDefault() != DefaultNtfyServer || n[4].ServerOrDefault() != "http://127.0.0.1:8090" {
		t.Error("ntfy server")
	}
	set := Settings{Version: 1, Notifiers: n}
	if got, ok := set.Notifier("mail"); !ok || got.Kind != NotifierEmail {
		t.Error("by name")
	}
	if _, ok := set.Notifier("nobody"); ok {
		t.Error("no such notifier")
	}
}

// The notifiers round-trip through the stored JSON and the YAML manifest.
func TestNotifiersRoundTrip(t *testing.T) {
	set := Settings{Version: 1, Notifiers: htgNotifiers()}
	raw, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseSettings(raw)
	if err != nil || !reflect.DeepEqual(back.Notifiers, set.Notifiers) {
		t.Fatalf("json: %+v %v", back.Notifiers, err)
	}
	y, err := set.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseManifest(y)
	if err != nil || !reflect.DeepEqual(m.Notifiers, set.Notifiers) {
		t.Fatalf("manifest:\n%s\n%+v %v", y, m.Notifiers, err)
	}
}
