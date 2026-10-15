package api

// The operator dashboards in the hub (ADR-0030, gravel#204): the definitions the Grafana JSON is
// generated from, and their series read from the hub's metrics store. A client names a panel; the
// hub runs that panel's own expressions, expanded with its own variables, so no viewer's PromQL ever
// reaches the store.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/authz"
	"github.com/gravel-project/gravel/internal/config"
	"github.com/gravel-project/gravel/internal/dashboards"
	"github.com/gravel-project/gravel/internal/promquery"
)

// The bounds of a range query: a step no finer than the scrape interval, at most this many points
// on a line, at most this long a range.
const (
	MinStep      = 15 * time.Second
	MaxPoints    = 11_000
	MaxRange     = 30 * 24 * time.Hour
	maxSeriesOut = 500
)

// Querier is a metrics store's range query (promquery.Client).
type Querier interface {
	QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]promquery.Series, error)
}

// MetricsServer serves gravel.hub.v1.MetricsService: metrics:read.
type MetricsServer struct {
	hubv1connect.UnimplementedMetricsServiceHandler
	access  *Access
	cfg     config.Metrics
	q       Querier // nil: no store configured
	defs    []dashboards.Dashboard
	panels  map[string]panelRef
	observe func(result string)
	logger  *slog.Logger
}

type panelRef struct {
	panel dashboards.Panel
	job   string
}

// NewMetricsServer serves defs from the store q reads (nil when cfg names none); observe counts
// each query by its result (gravel_metrics_queries_total).
func NewMetricsServer(defs []dashboards.Dashboard, cfg config.Metrics, q Querier, access *Access, observe func(string), logger *slog.Logger) *MetricsServer {
	s := &MetricsServer{access: access, cfg: cfg, q: q, defs: defs, panels: map[string]panelRef{}, observe: observe, logger: logger}
	if s.observe == nil {
		s.observe = func(string) {}
	}
	for _, d := range defs {
		for i, r := range d.Rows {
			for j, p := range r.Panels {
				s.panels[d.UID+"/"+panelID(i, j)] = panelRef{panel: p, job: cfg.Job(d.Binary())}
			}
		}
	}
	return s
}

func panelID(row, panel int) string { return fmt.Sprintf("r%dp%d", row, panel) }

// ListDashboards is the definitions and whether a store is configured.
func (s *MetricsServer) ListDashboards(ctx context.Context, _ *connect.Request[hubv1.ListDashboardsRequest]) (*connect.Response[hubv1.ListDashboardsResponse], error) {
	if _, err := s.access.Require(ctx, authz.MetricsRead, authz.Target{}); err != nil {
		return nil, err
	}
	out := &hubv1.ListDashboardsResponse{Available: s.q != nil}
	for _, d := range s.defs {
		md := &hubv1.MetricsDashboard{Uid: d.UID, Title: d.Title, Description: d.Description}
		for i, r := range d.Rows {
			mr := &hubv1.MetricsRow{Title: r.Title}
			for j, p := range r.Panels {
				mp := &hubv1.MetricsPanel{Id: panelID(i, j), Title: p.Title, Description: p.Description, Kind: p.Kind, Unit: p.Unit,
					Width: int32(max(p.Width, 6)), Optional: p.Optional} //nolint:gosec // a grid width
				for _, t := range p.Thresholds {
					mp.Thresholds = append(mp.Thresholds, &hubv1.MetricsThreshold{Value: t.Value, Color: t.Color})
				}
				for _, m := range p.Mappings {
					mp.Mappings = append(mp.Mappings, &hubv1.MetricsValueMap{Value: m.Value, Text: m.Text, Color: m.Color})
				}
				mr.Panels = append(mr.Panels, mp)
			}
			md.Rows = append(md.Rows, mr)
		}
		out.Dashboards = append(out.Dashboards, md)
	}
	return connect.NewResponse(out), nil
}

// QueryRange is one panel's series over a range.
func (s *MetricsServer) QueryRange(ctx context.Context, req *connect.Request[hubv1.QueryRangeRequest]) (*connect.Response[hubv1.QueryRangeResponse], error) {
	if _, err := s.access.Require(ctx, authz.MetricsRead, authz.Target{}); err != nil {
		return nil, err
	}
	m := req.Msg
	ref, ok := s.panels[m.GetDashboardUid()+"/"+m.GetPanelId()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("no such panel on this hub's dashboards"))
	}
	if s.q == nil {
		s.observe("no_store")
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("no metrics store is configured: set metrics.query_url in hub.yaml (docs/hub.md, Observability)"))
	}
	if !m.GetStart().IsValid() || !m.GetEnd().IsValid() {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("start and end are required"))
	}
	start, end := m.GetStart().AsTime(), m.GetEnd().AsTime()
	span := end.Sub(start)
	if span <= 0 || span > MaxRange {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("the range must end after it starts and span at most %s", MaxRange))
	}
	step := max(m.GetStep().AsDuration(), MinStep, (span/MaxPoints + time.Second).Truncate(time.Second))
	qctx, cancel := context.WithTimeout(ctx, s.cfg.QueryTimeout())
	defer cancel()
	out := &hubv1.QueryRangeResponse{Step: durationpb.New(step)}
	for _, t := range ref.panel.Targets {
		series, err := s.q.QueryRange(qctx, dashboards.ExpandFor(t.Expr, ref.job, step, span), start, end, step)
		if err != nil {
			return nil, s.storeError(ctx, err)
		}
		for _, x := range series {
			if len(out.Series) == maxSeriesOut {
				s.observe("too_large")
				return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("the panel has over %d lines over this range", maxSeriesOut))
			}
			ms := &hubv1.MetricsSeries{Name: legend(t.Legend, x.Labels), Labels: x.Labels, Values: x.Values}
			for _, at := range x.Times {
				ms.Times = append(ms.Times, at.UnixMilli())
			}
			out.Series = append(out.Series, ms)
		}
	}
	s.observe("ok")
	return connect.NewResponse(out), nil
}

// storeError is a store's failure in words a viewer can act on, counted by its kind.
func (s *MetricsServer) storeError(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, promquery.ErrTooLarge):
		s.observe("too_large")
		return connect.NewError(connect.CodeResourceExhausted, errors.New("the metrics store's answer is too large: pick a shorter range"))
	case errors.Is(err, promquery.ErrRefused):
		s.observe("refused")
		s.logger.WarnContext(ctx, "the metrics store refused a dashboard query", "error", err.Error())
		return connect.NewError(connect.CodeFailedPrecondition, err)
	default:
		s.observe("unavailable")
		s.logger.WarnContext(ctx, "the metrics store did not answer", "error", err.Error())
		return connect.NewError(connect.CodeUnavailable, errors.New("the metrics store did not answer in time"))
	}
}

var legendLabel = regexp.MustCompile(`\{\{\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\}\}`)

// legend is a target's legend with the series' labels in it, as Grafana draws it; an empty legend
// is the labels themselves.
func legend(format string, labels map[string]string) string {
	if format == "" {
		var parts []string
		for k, v := range labels {
			parts = append(parts, k+"="+v)
		}
		if len(parts) == 0 {
			return "value"
		}
		slices.Sort(parts)
		return strings.Join(parts, ", ")
	}
	return legendLabel.ReplaceAllStringFunc(format, func(m string) string {
		return labels[legendLabel.FindStringSubmatch(m)[1]]
	})
}
