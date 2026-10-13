package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gravel-project/gravel/internal/events"
	"github.com/gravel-project/gravel/internal/org"
)

// The headers a JSON webhook's request carries: when it was signed (Unix seconds) and the
// signature, "sha256=" and the hex HMAC-SHA256 of the timestamp, a dot and the body, keyed by the
// notifier's secret. A receiver checks both and refuses an old timestamp, so a request replayed
// later is refused too.
const (
	HeaderTimestamp = "X-Gravel-Timestamp"
	HeaderSignature = "X-Gravel-Signature"
)

// WebhookVersion is the JSON body's format.
const WebhookVersion = 1

// WebhookBody is a JSON webhook's body.
type WebhookBody struct {
	Version int            `json:"version"`
	Hub     string         `json:"hub"`
	Link    string         `json:"link,omitempty"`
	Events  []WebhookEvent `json:"events"`
}

// WebhookEvent is an event in a JSON webhook's body, named as ListEvents names it.
type WebhookEvent struct {
	ID       int64          `json:"id"`
	At       time.Time      `json:"at"`
	Kind     string         `json:"kind"`
	Severity string         `json:"severity"`
	ServerID string         `json:"server_id,omitempty"`
	GameID   string         `json:"game_id,omitempty"`
	Summary  string         `json:"summary"`
	Detail   map[string]any `json:"detail,omitempty"`
}

// Sign is a JSON webhook's signature of body at ts.
func Sign(key, ts string, body []byte) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(ts + "."))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// Verify checks a JSON webhook request's signature and that it was signed within maxAge of now:
// what a receiver written in Go does.
func Verify(key string, header http.Header, body []byte, now time.Time, maxAge time.Duration) error {
	ts := header.Get(HeaderTimestamp)
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errors.New("notify: no timestamp")
	}
	if d := now.Sub(time.Unix(sec, 0)); d > maxAge || d < -maxAge {
		return errors.New("notify: the timestamp is too old or in the future")
	}
	if !hmac.Equal([]byte(header.Get(HeaderSignature)), []byte(Sign(key, ts, body))) {
		return errors.New("notify: the signature does not match")
	}
	return nil
}

type webhook struct {
	name, url, flavour string
	d                  Deps
}

// NewWebhook builds a webhook notifier: a signed JSON POST to the settings' URL, or, with the
// discord flavour, a message to the Discord webhook whose URL is the notifier's secret, so a hub
// with no bot still posts to Discord.
func NewWebhook(n org.Notifier, d Deps) (Notifier, error) {
	w := &webhook{name: n.Name, url: n.URL, flavour: n.FlavourOrDefault(), d: d}
	switch w.flavour {
	case org.FlavourJSON:
		if d.Secret == "" {
			return nil, fmt.Errorf("no signing key: set hub.yaml notify.secrets.%s.file", n.Name)
		}
	case org.FlavourDiscord:
		u, err := url.Parse(d.Secret)
		if d.Secret == "" || err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, fmt.Errorf("no Discord webhook URL: put it in hub.yaml notify.secrets.%s.file", n.Name)
		}
		w.url = d.Secret
	default:
		return nil, fmt.Errorf("no webhook flavour %q", n.Flavour)
	}
	return w, nil
}

func (w *webhook) Name() string { return w.name }

func (w *webhook) Notify(ctx context.Context, evs []Event) error {
	if w.flavour == org.FlavourDiscord {
		return w.discord(ctx, evs)
	}
	b := WebhookBody{Version: WebhookVersion, Hub: w.d.Hub, Link: w.d.Link, Events: make([]WebhookEvent, 0, len(evs))}
	for _, e := range evs {
		b.Events = append(b.Events, WebhookEvent{ID: e.ID, At: e.At.UTC(), Kind: e.Kind, Severity: string(e.Severity), ServerID: e.ServerID,
			GameID: e.GameID, Summary: e.Summary, Detail: e.Detail})
	}
	body, err := json.Marshal(b)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	ts := strconv.FormatInt(w.d.Now().Unix(), 10)
	h := http.Header{"Content-Type": {"application/json"}}
	h.Set(HeaderTimestamp, ts)
	h.Set(HeaderSignature, Sign(w.d.Secret, ts, body))
	return post(ctx, w.d.Client, w.url, body, h)
}

// discordMax is a Discord message's length limit.
const discordMax = 2000

var severityMark = map[events.Severity]string{events.Info: "ℹ️", events.Notice: "🔔", events.Warning: "⚠️", events.Critical: "🚨"}

func (w *webhook) discord(ctx context.Context, evs []Event) error {
	text := lines(evs, func(e Event) string { return severityMark[e.Severity] + " " }, discordMax)
	body, err := json.Marshal(map[string]any{
		"username":         w.d.Hub,
		"content":          text,
		"allowed_mentions": map[string]any{"parse": []string{}}, // an event's words never ping anyone
	})
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	return post(ctx, w.d.Client, w.url, body, http.Header{"Content-Type": {"application/json"}})
}
