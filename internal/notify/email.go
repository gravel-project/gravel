package notify

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/wneessen/go-mail"

	"github.com/gravel-project/gravel/internal/org"
)

type email struct {
	name string
	to   []string
	d    Deps
}

// NewEmail builds an email notifier: one plain-text message per batch to the settings'
// addresses, through hub.yaml's mail server.
func NewEmail(n org.Notifier, d Deps) (Notifier, error) {
	if d.SMTP.Host == "" {
		return nil, errors.New("no mail server: set hub.yaml notify.smtp")
	}
	return &email{name: n.Name, to: n.To, d: d}, nil
}

func (e *email) Name() string { return e.name }

func (e *email) Notify(ctx context.Context, evs []Event) error {
	m := mail.NewMsg()
	if err := m.From(e.d.SMTP.From); err != nil {
		return fmt.Errorf("from: %w", err)
	}
	if err := m.To(e.to...); err != nil {
		return fmt.Errorf("to: %w", err)
	}
	m.Subject(title(e.d.Hub, evs))
	var b strings.Builder
	for _, ev := range evs {
		fmt.Fprintf(&b, "%s  %s  %s\n", ev.At.UTC().Format("2006-01-02 15:04:05 UTC"), ev.Severity, line(ev))
	}
	if e.d.Link != "" {
		fmt.Fprintf(&b, "\n%s\n", e.d.Link)
	}
	m.SetBodyString(mail.TypeTextPlain, b.String())
	return send(ctx, e.d.SMTP, m)
}

// Mailer sends one plain-text message to one address through hub.yaml's mail server: what the hub
// mails a member who asked (a recovery address's verification, a password link, gravel#234),
// beside the notifiers, which carry events to staff.
type Mailer struct {
	smtp SMTP
}

// NewMailer builds a Mailer over the mail server.
func NewMailer(s SMTP) (*Mailer, error) {
	if s.Host == "" {
		return nil, errors.New("no mail server: set hub.yaml notify.smtp")
	}
	return &Mailer{smtp: s}, nil
}

// Mail sends the message. Its errors never carry the body, which may hold a one-time link.
func (m *Mailer) Mail(ctx context.Context, to, subject, body string) error {
	msg := mail.NewMsg()
	if err := msg.From(m.smtp.From); err != nil {
		return fmt.Errorf("from: %w", err)
	}
	if err := msg.To(to); err != nil {
		return fmt.Errorf("to: %w", err)
	}
	msg.Subject(subject)
	msg.SetBodyString(mail.TypeTextPlain, body)
	return send(ctx, m.smtp, msg)
}

// send delivers a message through the mail server.
func send(ctx context.Context, s SMTP, m *mail.Msg) error {
	opts := []mail.Option{mail.WithTimeout(SendTimeout), mail.WithPort(s.Port)}
	switch s.TLS {
	case "implicit":
		opts = append(opts, mail.WithSSL())
	case "none":
		opts = append(opts, mail.WithTLSPolicy(mail.NoTLS))
	default:
		opts = append(opts, mail.WithTLSPolicy(mail.TLSMandatory))
	}
	if s.Username != "" {
		opts = append(opts, mail.WithSMTPAuth(mail.SMTPAuthAutoDiscover), mail.WithUsername(s.Username), mail.WithPassword(s.Password))
	}
	c, err := mail.NewClient(s.Host, opts...)
	if err != nil {
		return fmt.Errorf("mail client: %w", err)
	}
	if err := c.DialAndSendWithContext(ctx, m); err != nil {
		return fmt.Errorf("send: %w", err)
	}
	return nil
}
