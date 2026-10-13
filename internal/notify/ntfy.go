package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gravel-project/gravel/internal/events"
	"github.com/gravel-project/gravel/internal/org"
)

type ntfy struct {
	name, server, topic string
	d                   Deps
}

// NewNtfy builds an ntfy notifier: one message per batch to the settings' topic on its server,
// with the notifier's secret, when there is one, as the access token.
func NewNtfy(n org.Notifier, d Deps) (Notifier, error) {
	return &ntfy{name: n.Name, server: strings.TrimRight(n.ServerOrDefault(), "/"), topic: n.Topic, d: d}, nil
}

func (n *ntfy) Name() string { return n.name }

// ntfyPriority is ntfy's priority for a severity: 2 (low) to 5 (urgent).
var ntfyPriority = map[events.Severity]int{events.Info: 2, events.Notice: 3, events.Warning: 4, events.Critical: 5}

// ntfyTag is the tag ntfy shows as an emoji.
var ntfyTag = map[events.Severity]string{events.Info: "information_source", events.Notice: "bell", events.Warning: "warning", events.Critical: "rotating_light"}

// ntfyMax keeps a message under ntfy's 4096-byte limit, past which it becomes an attachment.
const ntfyMax = 4000

func (n *ntfy) Notify(ctx context.Context, evs []Event) error {
	sev := maxSeverity(evs)
	msg := map[string]any{
		"topic":    n.topic,
		"title":    title(n.d.Hub, evs),
		"message":  lines(evs, func(Event) string { return "" }, ntfyMax),
		"priority": ntfyPriority[sev],
		"tags":     []string{ntfyTag[sev]},
	}
	if n.d.Link != "" {
		msg["click"] = n.d.Link
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	h := http.Header{"Content-Type": {"application/json"}}
	if n.d.Secret != "" {
		h.Set("Authorization", "Bearer "+n.d.Secret)
	}
	// ntfy takes a JSON message at its root URL.
	return post(ctx, n.d.Client, n.server+"/", body, h)
}
