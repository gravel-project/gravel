// Package ratelimit bounds request rates per key: per client IP on the public surface, per
// user on authenticated calls (README, API). It is a token bucket per key over
// golang.org/x/time/rate, with idle keys swept so memory stays bounded.
package ratelimit

import (
	"context"
	"errors"
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/time/rate"
)

// idleFor is how long a key goes unused before Sweep drops it.
const idleFor = 10 * time.Minute

// Limiter is a set of token buckets keyed by string.
type Limiter struct {
	limit rate.Limit
	burst int

	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
}

type bucket struct {
	l        *rate.Limiter
	lastSeen time.Time
}

// New builds a limiter allowing perMinute sustained requests per key with the given burst.
func New(perMinute, burst int) *Limiter {
	return &Limiter{limit: rate.Limit(float64(perMinute) / 60), burst: burst, buckets: map[string]*bucket{}, now: time.Now}
}

// Allow takes one token for key. When refused it says how long until one is available.
func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	b, found := l.buckets[key]
	if !found {
		b = &bucket{l: rate.NewLimiter(l.limit, l.burst)}
		l.buckets[key] = b
	}
	now := l.now()
	b.lastSeen = now
	l.mu.Unlock()
	r := b.l.ReserveN(now, 1)
	if !r.OK() {
		return false, time.Minute
	}
	if d := r.DelayFrom(now); d > 0 {
		r.CancelAt(now)
		return false, d
	}
	return true, 0
}

// Sweep drops keys idle for ten minutes and reports how many remain.
func (l *Limiter) Sweep() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := l.now().Add(-idleFor)
	for k, b := range l.buckets {
		if b.lastSeen.Before(cutoff) {
			delete(l.buckets, k)
		}
	}
	return len(l.buckets)
}

// KeyFunc names the bucket a request falls in. An empty key means "not limited".
type KeyFunc func(r *http.Request) string

// Rejected is called when a request is refused, so the hub can count it.
type Rejected func(scope string)

// Scoped pairs a limiter with the key function that selects it. The first scope whose key is
// non-empty applies: list the per-user scope before the per-IP one.
type Scoped struct {
	Scope   string
	Limiter *Limiter
	Key     KeyFunc
}

// Middleware answers 429 with Retry-After when the request's scope is over its limit.
func Middleware(scopes []Scoped, rejected Rejected) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if scope, retryAfter, ok := check(scopes, r); !ok {
				if rejected != nil {
					rejected(scope)
				}
				w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retryAfter.Seconds()))))
				http.Error(w, http.StatusText(http.StatusTooManyRequests), http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func check(scopes []Scoped, r *http.Request) (scope string, retryAfter time.Duration, ok bool) {
	for _, s := range scopes {
		key := s.Key(r)
		if key == "" {
			continue
		}
		ok, retryAfter := s.Limiter.Allow(s.Scope + ":" + key)
		return s.Scope, retryAfter, ok
	}
	return "", 0, true
}

// ErrLimited is the error a limited Connect procedure returns, with CodeResourceExhausted.
var ErrLimited = errors.New("rate limit exceeded")

// Interceptor refuses Connect procedures over the limit with CodeResourceExhausted. The key
// functions see a synthetic request carrying the procedure's context and headers.
func Interceptor(scopes []Scoped, rejected Rejected) connect.Interceptor {
	return rpcInterceptor{scopes: scopes, rejected: rejected}
}

type rpcInterceptor struct {
	scopes   []Scoped
	rejected Rejected
}

func (i rpcInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if err := i.check(ctx, req.Header(), req.Peer()); err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (i rpcInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i rpcInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if err := i.check(ctx, conn.RequestHeader(), conn.Peer()); err != nil {
			return err
		}
		return next(ctx, conn)
	}
}

func (i rpcInterceptor) check(ctx context.Context, header http.Header, peer connect.Peer) error {
	r := (&http.Request{Header: header, RemoteAddr: peer.Addr}).WithContext(ctx)
	scope, retryAfter, ok := check(i.scopes, r)
	if ok {
		return nil
	}
	if i.rejected != nil {
		i.rejected(scope)
	}
	err := connect.NewError(connect.CodeResourceExhausted, ErrLimited)
	err.Meta().Set("Retry-After", strconv.Itoa(int(math.Ceil(retryAfter.Seconds()))))
	return err
}

// ClientIP is the per-IP key: the request's remote address without its port. Put a
// proxy-aware middleware in front when the hub sits behind one.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
