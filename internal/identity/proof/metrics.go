package proof

import "github.com/prometheus/client_golang/prometheus"

// Results a proof is counted by.
const (
	resultStarted = "started"
	resultLinked  = "linked"
	resultProven  = "proven"
	resultTaken   = "taken"
	resultBadCode = "bad_code"
	resultLimited = "limited"
)

// WithMetrics registers gravel_identity_proofs_total{channel, result} on reg: codes started, and
// codes presented by what they did (linked, proven again, taken by another member, a bad code,
// refused by a rate limit). Every label value is the hub's.
func WithMetrics(reg prometheus.Registerer) Option {
	return func(s *Service) {
		s.counted = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gravel_identity_proofs_total",
			Help: "Proof-of-control codes by channel and result: started, linked, proven (again), taken (another member's account; nothing moved), bad_code, limited.",
		}, []string{"channel", "result"})
		reg.MustRegister(s.counted)
	}
}

func (s *Service) count(channel, result string) {
	if s.counted != nil {
		s.counted.WithLabelValues(channel, result).Inc()
	}
}
