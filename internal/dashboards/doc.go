// Package dashboards is gravel's operator dashboards as definitions (ADR-0009, ADR-0030): rows of
// panels, each a stat or a time series over PromQL targets. One definition serves twice: rendered
// as Grafana's dashboard JSON (internal/tools/dashboards gen, deploy/observability), and read by
// the hub's MetricsService, which runs a panel's expressions for the admin dashboard's
// /admin/metrics and nothing else (gravel#204).
package dashboards
