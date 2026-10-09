package web

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"

	"github.com/gravel-project/gravel/internal/httpx"
)

// The pages are API clients (README principle 2, ADR-0005): every read and write goes through
// the hub's own Connect procedures, called in process over this transport. The page's request
// lends the call its cookie and request id; whatever the procedure sets as a cookie (a logout
// clearing the session) is relayed to the browser.

// browser is what a page request lends to the API calls made on its behalf, and what it
// collects from them.
type browser struct {
	cookie    string
	requestID string

	mu         sync.Mutex
	setCookies []string
}

type browserKey struct{}

type inProcessKey struct{}

// withBrowser returns a context carrying the request's cookie and request id for the in-process
// client, and the collector for cookies the API sets.
func withBrowser(r *http.Request) (context.Context, *browser) {
	b := &browser{cookie: r.Header.Get("Cookie"), requestID: httpx.RequestIDFromContext(r.Context())}
	return context.WithValue(r.Context(), browserKey{}, b), b
}

// relay copies the cookies the API set during the page's calls onto the page's response.
func (b *browser) relay(w http.ResponseWriter) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.setCookies {
		w.Header().Add("Set-Cookie", c)
	}
}

// IsInProcess reports whether ctx belongs to an API call a page made on a member's behalf. The
// hub's rate limiter skips those: the page request was counted already.
func IsInProcess(ctx context.Context) bool {
	v, _ := ctx.Value(inProcessKey{}).(bool)
	return v
}

// inProcess is an http.RoundTripper that hands requests to the API handler directly.
type inProcess struct {
	api http.Handler
}

func (t inProcess) RoundTrip(r *http.Request) (*http.Response, error) {
	ctx := context.WithValue(r.Context(), inProcessKey{}, true)
	req := r.Clone(ctx)
	if b, ok := r.Context().Value(browserKey{}).(*browser); ok {
		req.Header.Del("Cookie")
		if b.cookie != "" {
			req.Header.Set("Cookie", b.cookie)
		}
		if b.requestID != "" {
			req.Header.Set(httpx.RequestIDHeader, b.requestID)
		}
		defer func() {
			b.mu.Lock()
			defer b.mu.Unlock()
		}()
	}
	rec := &responseRecorder{header: http.Header{}}
	t.api.ServeHTTP(rec, req)
	if r.Body != nil {
		_ = r.Body.Close()
	}
	resp := &http.Response{
		Status:        http.StatusText(rec.status()),
		StatusCode:    rec.status(),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        rec.header,
		Body:          io.NopCloser(bytes.NewReader(rec.body.Bytes())),
		ContentLength: int64(rec.body.Len()),
		Request:       r,
	}
	if b, ok := r.Context().Value(browserKey{}).(*browser); ok {
		b.mu.Lock()
		b.setCookies = append(b.setCookies, rec.header.Values("Set-Cookie")...)
		b.mu.Unlock()
	}
	return resp, nil
}

// responseRecorder is the http.ResponseWriter the in-process call writes to.
type responseRecorder struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (r *responseRecorder) Header() http.Header { return r.header }

func (r *responseRecorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.body.Write(b)
}

func (r *responseRecorder) status() int {
	if r.code == 0 {
		return http.StatusOK
	}
	return r.code
}
