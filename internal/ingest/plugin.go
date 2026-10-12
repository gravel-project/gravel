package ingest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/gravel-project/gravel/internal/httpx"
	"github.com/gravel-project/gravel/internal/store"
)

// A server's match plugin (MatchZy on a CS2 server, ADR-0024) posts its match events and uploads
// its demos to the hub from the server's own host, with the server's feed token in the
// Authorization header it can send. An event batch is stored as received before the answer, as
// every batch is (ADR-0011); a demo is streamed to object storage instead, which is its home.

// Paths of the plugin routes.
const (
	EventsPath = "/api/ingest/matchzy"
	DemoPath   = "/api/ingest/matchzy/demo"
)

// EventsSource is the source a plugin's event batch is stored under.
const EventsSource = "matchzy"

// MaxEventsBody is the largest event accepted (a round's end with ten players' totals is a few KiB).
const MaxEventsBody = 1 << 20

// EventSink is told every stored event batch, after the answer, in the order they came.
type EventSink interface {
	Events(ctx context.Context, server string, body []byte)
}

// DemoSink keeps a demo a server uploaded: the plugin's headers say which match and map.
type DemoSink interface {
	// MaxDemoBytes is the largest demo it takes; 0 when it takes none.
	MaxDemoBytes() int64
	Demo(ctx context.Context, server string, header http.Header, body io.Reader, size int64) error
}

type pluginConfig struct {
	mu     sync.RWMutex
	events EventSink
	demos  DemoSink
}

// SetPluginSinks sets who is told of event batches and keeps demos; nil refuses them (404).
func (h *Handler) SetPluginSinks(events EventSink, demos DemoSink) {
	h.plugin.mu.Lock()
	defer h.plugin.mu.Unlock()
	h.plugin.events, h.plugin.demos = events, demos
}

func bearer(r *http.Request) string {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return strings.TrimSpace(token)
}

// ServeEvents stores one plugin event: 200 stored, 400 not JSON, 401, 404 the hub takes none, 413,
// 429, 503 not stored.
func (h *Handler) ServeEvents(w http.ResponseWriter, r *http.Request) {
	h.plugin.mu.RLock()
	sink := h.plugin.events
	h.plugin.mu.RUnlock()
	if sink == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	server, _, ok := h.fromHost(w, r, bearer(r))
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxEventsBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.count(server, EventsSource, "too_large")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		h.count(server, EventsSource, "bad_request")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if len(body) == 0 || body[0] != '{' {
		h.count(server, EventsSource, "bad_request")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), insertTimeout)
	defer cancel()
	now := h.now()
	if _, err := h.st.InsertIngestBatch(ctx, store.IngestBatch{OrganizationID: h.orgID, ServerID: server, Source: EventsSource, ReceivedAt: now,
		Body: body, Headers: []byte("{}")}); err != nil {
		h.count(server, EventsSource, "error")
		h.logger.ErrorContext(ctx, "ingest: an event was not stored", "server", server, "error", err.Error())
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	h.count(server, EventsSource, "stored")
	h.last.WithLabelValues(server).Set(float64(now.UnixNano()) / 1e9)
	w.WriteHeader(http.StatusOK)
	sink.Events(ctx, server, body)
}

// ServeDemo takes a demo upload: 200 kept, 401, 404 the hub keeps none, 411 no length, 413 too
// large, 429, 503 not kept.
func (h *Handler) ServeDemo(w http.ResponseWriter, r *http.Request) {
	h.plugin.mu.RLock()
	sink := h.plugin.demos
	h.plugin.mu.RUnlock()
	if sink == nil || sink.MaxDemoBytes() == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	server, _, ok := h.fromHost(w, r, bearer(r))
	if !ok {
		return
	}
	size := r.ContentLength
	if v := r.Header.Get("Content-Length"); size < 0 && v != "" {
		size, _ = strconv.ParseInt(v, 10, 64)
	}
	max := sink.MaxDemoBytes()
	if size > max {
		h.count(server, "demo", "too_large")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	body := http.MaxBytesReader(w, r.Body, max)
	if err := sink.Demo(r.Context(), server, r.Header, body, size); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.count(server, "demo", "too_large")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		h.count(server, "demo", "error")
		h.logger.ErrorContext(r.Context(), "ingest: a demo was not kept", "server", server, "error", err.Error(), "request_id", httpx.RequestIDFromContext(r.Context()))
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	h.count(server, "demo", "stored")
	w.WriteHeader(http.StatusOK)
}
