package httpx

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRequestIDPassthroughAndGeneration(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFromContext(r.Context())
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	req.Header.Set(RequestIDHeader, "abc-123")
	h.ServeHTTP(rec, req)
	if seen != "abc-123" || rec.Header().Get(RequestIDHeader) != "abc-123" {
		t.Errorf("passthrough: ctx %q header %q", seen, rec.Header().Get(RequestIDHeader))
	}

	for _, bad := range []string{"", "has space", strings.Repeat("x", 129), "a\nb"} {
		rec = httptest.NewRecorder()
		req = httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		req.Header.Set(RequestIDHeader, bad)
		h.ServeHTTP(rec, req)
		got := rec.Header().Get(RequestIDHeader)
		if got == bad || len(got) != 36 || seen != got {
			t.Errorf("bad id %q should be replaced by a uuid: header %q ctx %q", bad, got, seen)
		}
	}
	if RequestIDFromContext(context.Background()) != "" {
		t.Error("no id in a bare context")
	}
}

func TestRecover(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }), RequestID, Recover(logger))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status %d", rec.Code)
	}
	for _, want := range []string{`"panic":"boom"`, `"path":"/x"`, `"request_id":"`, `"stack":"`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log should contain %s: %s", want, buf.String())
		}
	}

	// http.ErrAbortHandler is re-panicked, as net/http expects.
	h = Recover(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	defer func() {
		if p := recover(); p != http.ErrAbortHandler { //nolint:errorlint // identity, like net/http
			t.Errorf("ErrAbortHandler should propagate, got %v", p)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
}

func TestLogging(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fail" {
			w.WriteHeader(http.StatusBadGateway)
		}
		_, _ = w.Write([]byte("hello"))
	}), RequestID, Logging(logger))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/ok", nil))
	line := buf.String()
	for _, want := range []string{`"level":"INFO"`, `"status":200`, `"bytes":5`, `"path":"/ok"`, `"request_id":"`} {
		if !strings.Contains(line, want) {
			t.Errorf("want %s in %s", want, line)
		}
	}
	buf.Reset()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/fail", nil))
	if !strings.Contains(buf.String(), `"level":"ERROR"`) || !strings.Contains(buf.String(), `"status":502`) {
		t.Errorf("5xx should log at error: %s", buf.String())
	}

	buf.Reset()
	quiet := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}), RequestID, Logging(logger, "/healthz", "/readyz"))
	quiet.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/healthz", nil))
	if buf.Len() != 0 {
		t.Errorf("a healthy probe should be debug-only (handler at info): %s", buf.String())
	}
	quiet.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil))
	if !strings.Contains(buf.String(), `"level":"ERROR"`) {
		t.Errorf("a failing probe still logs: %s", buf.String())
	}
}

func TestRecorderFlushAndUnwrap(t *testing.T) {
	base := httptest.NewRecorder()
	r := &recorder{ResponseWriter: base}
	r.Flush()
	if !base.Flushed {
		t.Error("Flush should reach the underlying writer")
	}
	if r.Unwrap() != base {
		t.Error("Unwrap should return the underlying writer")
	}
	if r.status() != http.StatusOK {
		t.Error("default status is 200")
	}
}

func TestHTTPMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	h := m.HTTP("healthz", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/healthz", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/healthz", nil))
	got := testutil.ToFloat64(m.httpRequests.WithLabelValues("healthz", "GET", "503"))
	if got != 2 {
		t.Errorf("counter: %v", got)
	}
	if n := testutil.CollectAndCount(m.httpDuration); n != 1 {
		t.Errorf("duration series: %d", n)
	}
}

func TestRPCInterceptorCodes(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	ic := m.Interceptor()
	spec := connect.Spec{Procedure: "/gravel.hub.v1.OrganizationService/ClaimOwnership"}
	req := connect.NewRequest(&struct{}{})
	// NewRequest has no spec; use the unary wrapper with a request that reports one.
	call := func(err error) {
		fn := ic.WrapUnary(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) { return nil, err })
		_, _ = fn(context.Background(), specRequest{AnyRequest: req, spec: spec})
	}
	call(nil)
	call(connect.NewError(connect.CodePermissionDenied, errors.New("no")))
	call(errors.New("plain"))
	for code, want := range map[string]float64{"ok": 1, "permission_denied": 1, "unknown": 1} {
		if got := testutil.ToFloat64(m.rpcRequests.WithLabelValues(spec.Procedure, code)); got != want {
			t.Errorf("%s: got %v want %v", code, got, want)
		}
	}
}

type specRequest struct {
	connect.AnyRequest
	spec connect.Spec
}

func (s specRequest) Spec() connect.Spec { return s.spec }
