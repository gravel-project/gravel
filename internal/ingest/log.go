package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"

	"github.com/gravel-project/gravel/internal/httpx"
	"github.com/gravel-project/gravel/internal/ratelimit"
	"github.com/gravel-project/gravel/internal/store"
)

// LogPath is where a game that cannot send an Authorization header pushes its log (CS2's
// logaddress_add_http): the feed token is the path's last segment. The access log writes the
// path without it, and a batch is accepted only from the address of the server's own host.
const LogPath = "/api/ingest/log/{token}"

// MaxLogBody is the largest log batch accepted (CS2 posts a few KiB at a time; its startup dump is
// the largest).
const MaxLogBody = 256 << 10

// Tap is told every stored log batch, after the answer: the cloud-VM driver keeps its roster
// from it. It must not block for long.
type Tap interface {
	Lines(server string, body []byte)
}

// AddressPin is where each server's log push may come from (its instance's address, and a reserved
// address its outbound traffic leaves from); ok is false for a server with no host running.
type AddressPin interface {
	SourceAddresses(server string) ([]netip.Addr, bool)
}

// LogSource is what the route knows of a kind of log source: the headers it defines (stored with
// the batch: offsets, instance, game state) and how to redact a batch before it is kept.
type LogSource struct {
	Headers []string
	// Redact returns the body with every secret replaced; secrets are the strings the route
	// knows to be secret (the token the batch was posted with).
	Redact func(body []byte, secrets ...string) []byte
}

type logConfig struct {
	mu      sync.RWMutex
	tap     Tap
	pin     AddressPin
	sources map[string]LogSource
}

// SetLogTap sets who is told of stored log batches.
func (h *Handler) SetLogTap(t Tap) {
	h.log.mu.Lock()
	defer h.log.mu.Unlock()
	h.log.tap = t
}

// SetAddressPin sets where each server's log push must come from; nil accepts any address.
func (h *Handler) SetAddressPin(p AddressPin) {
	h.log.mu.Lock()
	defer h.log.mu.Unlock()
	h.log.pin = p
}

// SetLogSource says how batches from a kind of source (a game id) are kept.
func (h *Handler) SetLogSource(source string, s LogSource) {
	h.log.mu.Lock()
	defer h.log.mu.Unlock()
	if h.log.sources == nil {
		h.log.sources = map[string]LogSource{}
	}
	h.log.sources[source] = s
}

// ServeLog stores one log batch. Like ServeHTTP it answers with no body: 200 stored, 401 no valid
// token or not from the server's host, 413 too large, 429 slow down, 503 not stored.
func (h *Handler) ServeLog(w http.ResponseWriter, r *http.Request) {
	httpx.SetLogPath(r.Context(), strings.TrimSuffix(LogPath, "{token}")+"<redacted>")
	token := strings.TrimSpace(r.PathValue("token"))
	server, source, ok := "", "", false
	if token != "" {
		server, source, ok = h.keys.Match(token)
	}
	h.log.mu.RLock()
	tap, pin, src := h.log.tap, h.log.pin, h.log.sources[source]
	h.log.mu.RUnlock()
	if ok && pin != nil {
		want, running := pin.SourceAddresses(server)
		got, err := netip.ParseAddr(ratelimit.ClientIP(r))
		if !running || err != nil || !slices.ContainsFunc(want, func(a netip.Addr) bool { return a.Unmap() == got.Unmap() }) {
			h.count(server, source, "address_mismatch")
			h.logger.Warn("ingest: a log batch with a server's token came from another address", "server", server)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}
	if !ok {
		if allowed, retry := h.failures.Allow(ratelimit.ClientIP(r)); !allowed {
			h.count(unknown, unknown, "rate_limited")
			tooMany(w, retry)
			return
		}
		h.count(unknown, unknown, "unauthorized")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if allowed, retry := h.servers.Allow(server); !allowed {
		h.count(server, source, "rate_limited")
		tooMany(w, retry)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxLogBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.count(server, source, "too_large")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		h.count(server, source, "bad_request")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if len(body) == 0 {
		h.count(server, source, "bad_request")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if src.Redact != nil {
		body = src.Redact(body, token)
	}
	headers := map[string]string{}
	for _, name := range src.Headers {
		if v := r.Header.Get(name); v != "" {
			headers[name] = v
		}
	}
	hj, err := json.Marshal(headers)
	if err != nil {
		h.count(server, source, "error")
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), insertTimeout)
	defer cancel()
	now := h.now()
	if _, err := h.st.InsertIngestBatch(ctx, store.IngestBatch{OrganizationID: h.orgID, ServerID: server, Source: source, ReceivedAt: now, Body: body, Headers: hj}); err != nil {
		h.count(server, source, "error")
		h.logger.ErrorContext(ctx, "ingest: batch not stored", "server", server, "bytes", len(body), "error", err.Error())
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	h.count(server, source, "stored")
	h.last.WithLabelValues(server).Set(float64(now.UnixNano()) / 1e9)
	w.WriteHeader(http.StatusOK)
	if tap != nil {
		tap.Lines(server, body)
	}
}
