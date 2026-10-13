// Package notify carries the hub's events to people (ADR-0028 §5, gravel#196). A Notifier
// delivers a batch of events from the events log somewhere a person sees them; webhook (JSON,
// signed, or a Discord webhook), ntfy and email are the defaults, each built by a Factory from the
// Organization settings' entry and the notifier's secret in hub.yaml. The Dispatcher follows the
// log with one cursor per notifier, so one that fails never holds up another.
package notify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gravel-project/gravel/internal/events"
	"github.com/gravel-project/gravel/internal/org"
)

// Event is one entry of the events log, as a notifier carries it.
type Event struct {
	ID       int64
	At       time.Time
	Kind     string
	Severity events.Severity
	ServerID string
	GameID   string
	Summary  string
	Detail   map[string]any
}

// Notifier delivers events to people. Notify delivers the whole batch or returns an error, and a
// failed batch is sent again whole, so a notifier sends a batch as one message: nobody gets the
// same event twice from a retry.
type Notifier interface {
	Name() string
	Notify(ctx context.Context, evs []Event) error
}

// Deps are what a notifier needs besides its settings entry.
type Deps struct {
	// Secret is the notifier's secret from hub.yaml (notify.secrets.<name>.file): a JSON webhook's
	// signing key, a Discord webhook's URL, an ntfy access token.
	Secret string
	SMTP   SMTP
	Client *http.Client
	// Hub is the organization's name, in titles and subjects.
	Hub string
	// Link is a page that shows the events (the admin dashboard's), "" for none.
	Link string
	Now  func() time.Time
}

// SMTP is the mail server email notifiers send through (hub.yaml notify.smtp).
type SMTP struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	// TLS is "starttls", "implicit" or "none".
	TLS string
}

// Factory builds a notifier of one kind from its settings entry.
type Factory func(n org.Notifier, d Deps) (Notifier, error)

// Factories are the notifier kinds this hub has, by the settings' kind.
var Factories = map[string]Factory{
	org.NotifierWebhook: NewWebhook,
	org.NotifierNtfy:    NewNtfy,
	org.NotifierEmail:   NewEmail,
}

// New builds a notifier from its settings entry.
func New(n org.Notifier, d Deps) (Notifier, error) {
	f, ok := Factories[n.Kind]
	if !ok {
		return nil, fmt.Errorf("notify: %s: no notifier kind %q", n.Name, n.Kind)
	}
	if d.Client == nil {
		d.Client = &http.Client{Timeout: SendTimeout}
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	return f(n, d)
}

// SendTimeout bounds one delivery.
const SendTimeout = 10 * time.Second

// TestKind is the kind of the event TestNotifier sends: it is never in the log.
const TestKind = "notifier.test"

// TestEvent is what TestNotifier sends.
func TestEvent(hub string, now time.Time) Event {
	return Event{At: now, Kind: TestKind, Severity: events.Info,
		Summary: "A test from " + hub + ": this notifier works."}
}

// maxSeverity is the batch's highest severity.
func maxSeverity(evs []Event) events.Severity {
	m := events.Info
	for _, e := range evs {
		if events.Rank(e.Severity) > events.Rank(m) {
			m = e.Severity
		}
	}
	return m
}

// title is a batch's one-line title: the event's kind, or how many there are.
func title(hub string, evs []Event) string {
	if len(evs) == 1 {
		return hub + ": " + evs[0].Kind
	}
	return fmt.Sprintf("%s: %d events", hub, len(evs))
}

// line is an event as one line of text.
func line(e Event) string {
	var b strings.Builder
	b.WriteString(e.Kind)
	if e.ServerID != "" {
		b.WriteString(" · " + e.ServerID)
	}
	b.WriteString(" · " + e.Summary)
	return b.String()
}

// lines are the batch as text, one event a line, cut to fit max bytes with a last line saying how
// many were left out.
func lines(evs []Event, prefix func(Event) string, maxBytes int) string {
	var out []string
	size := 0
	for i, e := range evs {
		l := prefix(e) + line(e)
		rest := ""
		if left := len(evs) - i - 1; left > 0 {
			rest = fmt.Sprintf("\n… and %d more", left)
		}
		if size+len(l)+1+len(rest) > maxBytes {
			out = append(out, fmt.Sprintf("… and %d more", len(evs)-i))
			break
		}
		out = append(out, l)
		size += len(l) + 1
	}
	return strings.Join(out, "\n")
}

// post sends body to u and treats anything but a 2xx answer as a failure. Its errors never carry
// the URL, which may hold a secret (a Discord webhook's token).
func post(ctx context.Context, c *http.Client, u string, body []byte, header http.Header) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(string(body)))
	if err != nil {
		return errors.New("bad URL")
	}
	req.Header = header
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "gravel-hub")
	}
	resp, err := c.Do(req)
	if err != nil {
		return sanitize(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("answered %s", resp.Status)
	}
	return nil
}

// sanitize drops the request's URL from a client error.
func sanitize(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", strings.ToLower(ue.Op), ue.Err)
	}
	return err
}
