package httpx

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds the collectors for one registry.
type Metrics struct {
	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec
	rpcRequests  *prometheus.CounterVec
	rpcDuration  *prometheus.HistogramVec
}

// NewMetrics registers the hub's request metrics on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gravel_http_requests_total", Help: "HTTP requests by handler, method and status code.",
		}, []string{"handler", "method", "code"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gravel_http_request_duration_seconds", Help: "HTTP request duration by handler.",
			Buckets: prometheus.DefBuckets,
		}, []string{"handler"}),
		rpcRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gravel_rpc_requests_total", Help: "Connect procedures handled, by procedure and Connect code (ok on success).",
		}, []string{"procedure", "code"}),
		rpcDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gravel_rpc_request_duration_seconds", Help: "Connect procedure duration.",
			Buckets: prometheus.DefBuckets,
		}, []string{"procedure"}),
	}
	reg.MustRegister(m.httpRequests, m.httpDuration, m.rpcRequests, m.rpcDuration)
	return m
}

// HTTP records a plain HTTP handler under the given name.
func (m *Metrics) HTTP(handler string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		m.httpRequests.WithLabelValues(handler, r.Method, strconv.Itoa(rec.status())).Inc()
		m.httpDuration.WithLabelValues(handler).Observe(time.Since(start).Seconds())
	})
}

// Interceptor records every Connect procedure, unary or streaming.
func (m *Metrics) Interceptor() connect.Interceptor { return rpcInterceptor{m: m} }

type rpcInterceptor struct{ m *Metrics }

func (i rpcInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		start := time.Now()
		resp, err := next(ctx, req)
		i.m.record(req.Spec().Procedure, err, start)
		return resp, err
	}
}

func (i rpcInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i rpcInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		start := time.Now()
		err := next(ctx, conn)
		i.m.record(conn.Spec().Procedure, err, start)
		return err
	}
}

func (m *Metrics) record(procedure string, err error, start time.Time) {
	code := "ok"
	if err != nil {
		code = connect.CodeOf(err).String()
		var ce *connect.Error
		if !errors.As(err, &ce) {
			code = connect.CodeUnknown.String()
		}
	}
	m.rpcRequests.WithLabelValues(procedure, code).Inc()
	m.rpcDuration.WithLabelValues(procedure).Observe(time.Since(start).Seconds())
}
