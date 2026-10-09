// Package httpx is the HTTP plumbing every listener shares: request ids, structured access
// logs, panic recovery and Prometheus metrics for HTTP handlers and Connect procedures.
package httpx

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/uuid"
)

type ctxKey int

const requestIDKey ctxKey = iota

// RequestIDHeader carries the request id in and out.
const RequestIDHeader = "X-Request-ID"

var safeRequestID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// RequestID adopts a well-formed incoming X-Request-ID or generates a UUIDv7, stores it in the
// context and echoes it on the response.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if !safeRequestID.MatchString(id) {
			id = newRequestID()
		}
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

func newRequestID() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	return uuid.NewString()
}

// RequestIDFromContext returns the request id set by RequestID, or "".
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// Recover turns a panic into a 500 and a log line with the stack, and keeps the server up.
func Recover(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if p := recover(); p != nil {
					if p == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity, as net/http does
						panic(p)
					}
					logger.ErrorContext(r.Context(), "panic in handler",
						"panic", p, "method", r.Method, "path", r.URL.Path,
						"request_id", RequestIDFromContext(r.Context()), "stack", string(debug.Stack()))
					http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// Logging writes one structured line per request. Requests to quietPaths (health probes) are
// logged at debug level unless they fail.
func Logging(logger *slog.Logger, quietPaths ...string) func(http.Handler) http.Handler {
	quiet := make(map[string]bool, len(quietPaths))
	for _, p := range quietPaths {
		quiet[p] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &recorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			level := slog.LevelInfo
			if quiet[r.URL.Path] {
				level = slog.LevelDebug
			}
			if rec.status() >= 500 {
				level = slog.LevelError
			}
			logger.LogAttrs(r.Context(), level, "request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status()),
				slog.Int64("bytes", rec.bytes),
				slog.Duration("duration", time.Since(start)),
				slog.String("remote", r.RemoteAddr),
				slog.String("request_id", RequestIDFromContext(r.Context())),
			)
		})
	}
}

// Chain applies middlewares so the first listed is outermost.
func Chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// recorder captures the status and byte count. It keeps Flush and Unwrap so streaming
// responses and http.ResponseController keep working through it.
type recorder struct {
	http.ResponseWriter
	code  int
	bytes int64
}

func (r *recorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *recorder) status() int {
	if r.code == 0 {
		return http.StatusOK
	}
	return r.code
}

// RealIP takes the client address from a trusted reverse proxy's header (CF-Connecting-IP
// behind cloudflared) and puts it in RemoteAddr, where access logs and rate limits read it.
// Use it only when nothing but that proxy can reach the listener: the header is trusted as
// given. An empty header name leaves requests untouched.
func RealIP(header string) func(http.Handler) http.Handler {
	if header == "" {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ip := net.ParseIP(strings.TrimSpace(r.Header.Get(header))); ip != nil {
				r2 := new(http.Request)
				*r2 = *r
				r2.RemoteAddr = net.JoinHostPort(ip.String(), "0")
				r = r2
			}
			next.ServeHTTP(w, r)
		})
	}
}
