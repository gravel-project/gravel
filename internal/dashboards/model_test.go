package dashboards

import (
	"strings"
	"testing"
	"time"
)

// The hub's own expansion (gravel#204): no variable or macro is left, the job is quoted, every
// instance is selected, and the rate window covers four scrapes and two steps.
func TestExpandFor(t *testing.T) {
	expr := `sum by (instance) (rate(gravel_http_requests_total{` + Sel + `}[$__rate_interval])) / $__interval + $__range`
	got := ExpandFor(expr, `gravel-hub`, 30*time.Second, time.Hour)
	want := `sum by (instance) (rate(gravel_http_requests_total{job="gravel-hub", instance=~".+"}[60s])) / 30s + 3600s`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if got := ExpandFor(`rate(x{`+Sel+`}[$__rate_interval])`, `a"b`, 5*time.Minute, time.Hour); !strings.Contains(got, `job="a\"b"`) || !strings.Contains(got, "[600s]") {
		t.Errorf("a quote in the job, a long step: %s", got)
	}
	for _, d := range All() {
		for _, r := range d.Rows {
			for _, p := range r.Panels {
				for _, tg := range p.Targets {
					if e := ExpandFor(tg.Expr, "gravel-hub", time.Minute, time.Hour); strings.Contains(e, "$") {
						t.Errorf("%s / %s: a variable left in %s", d.UID, p.Title, e)
					}
				}
			}
		}
	}
}
