// Package ingest receives the events a server pushes into the hub (ADR-0011): War Dogs'
// [WDServerFeed] first, Counter-Strike 2's HTTP log and MatchZy with P4. One route authenticates
// the server by its feed token, commits the batch as received to ingest_batches and only then
// answers: the sources do not retry, so that insert is all the durability they get. Parsing reads
// the stored batches afterwards (an adapter's job), so a parser bug or a game update loses
// nothing. Batches are a 30-day buffer, pruned by a hub job.
package ingest

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/gravel-project/gravel/internal/ratelimit"
	"github.com/gravel-project/gravel/internal/store"
)

// Path is where the sources post: War Dogs appends it to the feed's url.
const Path = "/api/ingest/events"

const (
	// MaxBody is the largest batch accepted: War Dogs' own request limit.
	MaxBody = 64 << 10
	// RotationGrace is how long a feed token file's second line, the previous token, is still
	// accepted after the hub first reads it: the new token reaches the server at its next restart,
	// a daily one at the host, so two days covers it.
	RotationGrace = 48 * time.Hour
	// Retention is how long a stored batch is kept: long enough to parse it again after a parser
	// fix or a game update; the events parsed from it are the record.
	Retention = 30 * 24 * time.Hour
	// PruneEvery is the prune job's period.
	PruneEvery = time.Hour

	// A server's batches: War Dogs posts about one every two seconds while players fight.
	perServerPerMinute = 600
	perServerBurst     = 60
	// Requests with no valid token, per client address, before they are answered 429.
	failuresPerMinute = 10
	failuresBurst     = 10

	insertTimeout = 5 * time.Second
	unknown       = "unknown"
)

// Store is where batches go.
type Store interface {
	InsertIngestBatch(ctx context.Context, b store.IngestBatch) (int64, error)
	DeleteIngestBatchesBefore(ctx context.Context, cutoff time.Time) (int64, error)
}

// Feed is one server's feed as the keyring reads it.
type Feed struct {
	Server string
	// Source is the kind of source, which says how its batches are parsed: the server's game.
	Source string
	// Token is the token file's content: the token, then optionally the previous one.
	Token string
}

// Keyring holds each server's feed token, as hashes, and matches a presented token against all
// of them in constant time per server.
type Keyring struct {
	mu      sync.RWMutex
	entries map[string]*entry
	now     func() time.Time
}

type entry struct {
	source   string
	cur      [32]byte
	prev     *[32]byte
	prevSeen time.Time
}

// NewKeyring returns an empty keyring.
func NewKeyring() *Keyring { return &Keyring{entries: map[string]*entry{}, now: time.Now} }

// Update replaces the keyring's feeds with feeds. A previous token keeps the time it was first
// seen across updates, so its grace runs from then.
func (k *Keyring) Update(feeds []Feed) {
	k.mu.Lock()
	defer k.mu.Unlock()
	next := make(map[string]*entry, len(feeds))
	for _, f := range feeds {
		lines := tokenLines(f.Token)
		if len(lines) == 0 {
			continue
		}
		e := &entry{source: f.Source, cur: sha256.Sum256([]byte(lines[0]))}
		if len(lines) > 1 && lines[1] != lines[0] {
			prev := sha256.Sum256([]byte(lines[1]))
			e.prev, e.prevSeen = &prev, k.now()
			if old, ok := k.entries[f.Server]; ok && old.prev != nil && *old.prev == prev {
				e.prevSeen = old.prevSeen
			}
		}
		next[f.Server] = e
	}
	k.entries = next
}

// Match returns the server a token belongs to. Every server's tokens are compared, so the time
// taken does not depend on which one matched.
func (k *Keyring) Match(token string) (server, source string, ok bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	got := sha256.Sum256([]byte(token))
	now := k.now()
	for id, e := range k.entries {
		hit := subtle.ConstantTimeCompare(got[:], e.cur[:]) == 1
		if e.prev != nil && now.Sub(e.prevSeen) < RotationGrace && subtle.ConstantTimeCompare(got[:], e.prev[:]) == 1 {
			hit = true
		}
		if hit && !ok {
			server, source, ok = id, e.source, true
		}
	}
	return server, source, ok
}

// Len is how many servers have a feed token.
func (k *Keyring) Len() int {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return len(k.entries)
}

func tokenLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// Handler is the ingest route.
type Handler struct {
	keys     *Keyring
	st       Store
	orgID    uuid.UUID
	logger   *slog.Logger
	now      func() time.Time
	servers  *ratelimit.Limiter
	failures *ratelimit.Limiter

	batches *prometheus.CounterVec
	last    *prometheus.GaugeVec
}

// NewHandler builds the route and registers its metrics.
func NewHandler(keys *Keyring, st Store, orgID uuid.UUID, reg prometheus.Registerer, logger *slog.Logger) *Handler {
	h := &Handler{
		keys: keys, st: st, orgID: orgID, logger: logger, now: time.Now,
		servers:  ratelimit.New(perServerPerMinute, perServerBurst),
		failures: ratelimit.New(failuresPerMinute, failuresBurst),
		batches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gravel_ingest_batches_total",
			Help: "Requests to the ingest route, by server, source and result (stored, unauthorized, rate_limited, too_large, bad_request, error).",
		}, []string{"server", "source", "result"}),
		last: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gravel_ingest_last_batch_timestamp_seconds",
			Help: "When the last batch from a server was stored, as a Unix time.",
		}, []string{"server"}),
	}
	reg.MustRegister(h.batches, h.last)
	return h
}

// Sweep drops idle rate-limit buckets; the hub's prune calls it.
func (h *Handler) Sweep() {
	h.servers.Sweep()
	h.failures.Sweep()
}

// ServeHTTP stores one batch. The answers carry no body and say nothing about which servers
// exist: 200 stored, 401 no valid token, 413 too large, 429 slow down, 503 not stored.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	server, source, ok := "", "", false
	if token = strings.TrimSpace(token); token != "" {
		server, source, ok = h.keys.Match(token)
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
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
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
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), insertTimeout)
	defer cancel()
	now := h.now()
	if _, err := h.st.InsertIngestBatch(ctx, store.IngestBatch{OrganizationID: h.orgID, ServerID: server, Source: source, ReceivedAt: now, Body: body}); err != nil {
		h.count(server, source, "error")
		h.logger.ErrorContext(ctx, "ingest: batch not stored", "server", server, "bytes", len(body), "error", err.Error())
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	h.count(server, source, "stored")
	h.last.WithLabelValues(server).Set(float64(now.UnixNano()) / 1e9)
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) count(server, source, result string) {
	h.batches.WithLabelValues(server, source, result).Inc()
}

func tooMany(w http.ResponseWriter, retry time.Duration) {
	secs := int(retry.Seconds() + 0.999)
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	w.WriteHeader(http.StatusTooManyRequests)
}

// Prune deletes the batches older than Retention.
func Prune(ctx context.Context, st Store, now time.Time, logger *slog.Logger) error {
	n, err := st.DeleteIngestBatchesBefore(ctx, now.Add(-Retention))
	if err != nil {
		return fmt.Errorf("ingest: prune: %w", err)
	}
	if n > 0 {
		logger.InfoContext(ctx, "ingest: pruned batches", "deleted", n, "older_than", Retention.String())
	}
	return nil
}
