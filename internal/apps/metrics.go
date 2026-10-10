package apps

import "github.com/prometheus/client_golang/prometheus"

// metrics are the endpoints' counters; a nil *metrics (the command line's Service) counts
// nothing.
type metrics struct {
	grants         *prometheus.CounterVec
	introspections *prometheus.CounterVec
}

// WithMetrics registers the token and introspection endpoints' counters on reg:
// gravel_oauth_token_grants_total{grant_type, result} and
// gravel_oauth_introspections_total{result}. Every label value is one the hub chose, never one
// a request named.
func WithMetrics(reg prometheus.Registerer) Option {
	return func(s *Service) {
		m := &metrics{
			grants: prometheus.NewCounterVec(prometheus.CounterOpts{
				Name: "gravel_oauth_token_grants_total",
				Help: "Token endpoint requests by grant_type (client_credentials, authorization_code, other) and result (issued, invalid_client, invalid_grant, replayed, invalid_scope, invalid_request, unsupported_grant_type, error).",
			}, []string{"grant_type", "result"}),
			introspections: prometheus.NewCounterVec(prometheus.CounterOpts{
				Name: "gravel_oauth_introspections_total",
				Help: "Introspection requests by result (active, inactive, invalid_client, invalid_request, error).",
			}, []string{"result"}),
		}
		reg.MustRegister(m.grants, m.introspections)
		s.metrics = m
	}
}

func (m *metrics) grant(grantType, result string) {
	if m != nil {
		m.grants.WithLabelValues(grantType, result).Inc()
	}
}

func (m *metrics) introspection(result string) {
	if m != nil {
		m.introspections.WithLabelValues(result).Inc()
	}
}
