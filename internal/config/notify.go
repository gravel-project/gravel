package config

import (
	"fmt"
	"maps"
	"net"
	"net/mail"
	"regexp"
	"slices"
	"strings"
)

// EnvSMTPPassword overrides notify.smtp.password_file and notify.smtp.password when set.
const EnvSMTPPassword = "GRAVEL_SMTP_PASSWORD"

// Notify is the notifiers' secrets and the SMTP server email notifiers send through (ADR-0028 §5,
// gravel#196). Which notifiers exist, where they send and what they take are the Organization
// settings' (notifiers); nothing secret is.
type Notify struct {
	// Secrets are each notifier's secret, by its name in the settings: a JSON webhook's signing
	// key, a Discord webhook's whole URL, or an ntfy access token.
	Secrets map[string]NotifySecret `yaml:"secrets"`
	SMTP    SMTP                    `yaml:"smtp"`
}

// NotifySecret is one notifier's secret, read from a file (a podman secret).
type NotifySecret struct {
	File  string `yaml:"file"`
	value string
}

// Secret is the named notifier's secret, "" when hub.yaml has none.
func (n Notify) Secret(name string) string { return n.Secrets[name].value }

// SMTP is the mail server email notifiers send through. Empty Host: email notifiers fail with a
// message saying so.
type SMTP struct {
	Host string `yaml:"host"`
	// Port is 0 for the default: 587, or 465 with tls: implicit.
	Port     int    `yaml:"port"`
	Username string `yaml:"username"`
	// Password is development only: production sets GRAVEL_SMTP_PASSWORD or PasswordFile.
	Password     string `yaml:"password"`
	PasswordFile string `yaml:"password_file"`
	// From is the sender's address.
	From string `yaml:"from"`
	// TLS is SMTPStartTLS ("", the default), SMTPImplicitTLS, or SMTPNoTLS (to the loopback
	// interface only: a relay on the same host).
	TLS            string `yaml:"tls"`
	passwordSource string
}

// SMTP TLS modes.
const (
	SMTPStartTLS    = "starttls"
	SMTPImplicitTLS = "implicit"
	SMTPNoTLS       = "none"
)

// Enabled reports whether a mail server is configured.
func (s SMTP) Enabled() bool { return s.Host != "" }

// TLSMode is the TLS mode, the default made explicit.
func (s SMTP) TLSMode() string {
	if s.TLS == "" {
		return SMTPStartTLS
	}
	return s.TLS
}

// PortOrDefault is the port.
func (s SMTP) PortOrDefault() int {
	switch {
	case s.Port != 0:
		return s.Port
	case s.TLSMode() == SMTPImplicitTLS:
		return 465
	}
	return 587
}

// PasswordSource says where the password came from, for the startup log.
func (s SMTP) PasswordSource() string { return s.passwordSource }

// notifierName is the settings' rule for a notifier's name (internal/org), restated: config can't
// import org.
var notifierName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func (n *Notify) resolveSecrets(env Env) error {
	for _, name := range slices.Sorted(maps.Keys(n.Secrets)) {
		s := n.Secrets[name]
		if s.File == "" {
			continue // Validate says so
		}
		b, err := env.ReadFile(s.File)
		if err != nil {
			return fmt.Errorf("config: notify.secrets.%s.file: %w", name, err)
		}
		s.value = strings.TrimSpace(string(b))
		n.Secrets[name] = s
	}
	if n.SMTP.Enabled() {
		var err error
		if n.SMTP.Password, n.SMTP.passwordSource, err = resolveSecret(env, EnvSMTPPassword, n.SMTP.PasswordFile, n.SMTP.Password, "notify.smtp.password"); err != nil {
			return err
		}
	}
	return nil
}

func (n Notify) validate(bad func(string, ...any)) {
	for _, name := range slices.Sorted(maps.Keys(n.Secrets)) {
		s := n.Secrets[name]
		switch {
		case !notifierName.MatchString(name):
			bad("notify.secrets: %q is not a notifier name (lower case letters, digits and hyphens, at most 32)", name)
		case s.File == "":
			bad("notify.secrets.%s.file: required", name)
		case s.value == "":
			bad("notify.secrets.%s.file: %s is empty", name, s.File)
		}
	}
	s := n.SMTP
	if !s.Enabled() {
		if s != (SMTP{}) {
			bad("notify.smtp.host: required when anything else in notify.smtp is set")
		}
		return
	}
	if strings.ContainsAny(s.Host, " /:@") {
		bad("notify.smtp.host: %q is not a host name", s.Host)
	}
	if s.Port < 0 || s.Port > 65535 {
		bad("notify.smtp.port: %d is not a port", s.Port)
	}
	if a, err := mail.ParseAddress(s.From); err != nil || a.Address != s.From {
		bad("notify.smtp.from: %q is not a plain email address", s.From)
	}
	switch s.TLSMode() {
	case SMTPStartTLS, SMTPImplicitTLS:
	case SMTPNoTLS:
		if ip := net.ParseIP(s.Host); s.Host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			bad("notify.smtp.tls: none sends the mail in the clear, so only to the loopback interface (%q is not)", s.Host)
		}
		if s.Username != "" {
			bad("notify.smtp.username: tls: none sends no password in the clear; a relay on the host needs none")
		}
	default:
		bad("notify.smtp.tls: %q is not %q, %q or %q", s.TLS, SMTPStartTLS, SMTPImplicitTLS, SMTPNoTLS)
	}
	if (s.Username == "") != (s.Password == "") {
		bad("notify.smtp: a username needs a password and a password a username; set %s, notify.smtp.password_file or notify.smtp.password", EnvSMTPPassword)
	}
}
