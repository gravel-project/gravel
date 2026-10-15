# ADR-0030: Operator dashboards in the hub, through a query proxy

**Status:** proposed (2026-10-15, built in #204) · **Changes:** where an operator looks at gravel's metrics: the admin dashboard, not only Grafana; extends ADR-0009 (one set of definitions now serves both) · **Decided in:** John's design session *gravel — Design Session: Hub, Spines & Dashboard* (2026-10-10 to 12: "No Grafana", "lighter stores", the hub emits and something beside it collects), handbook D35–D36 · **Part of:** H1 (#182); closes #154's direction

## Context

ADR-0009 ships gravel's operator dashboards as Grafana JSON generated from Go definitions. Grafana is a second web app with its own login, about 580 MB of memory on HTG's hub, and a page the VPS Dad persona (ADR-0025) won't run. The design session decided that dashboards render in gravel's own admin area, that the hub emits metrics and a lean Prometheus-compatible store beside it collects them (VictoriaMetrics preferred over Prometheus), and that the hub doesn't gather or keep metrics itself.

The admin dashboard (ADR-0029) is a static client of the API. To draw a chart it needs series over a range, which only the metrics store has. Letting the browser query the store directly would mean exposing it and its credentials, and accepting arbitrary PromQL from anyone holding a session.

## Decision

1. **One set of definitions, two renderings.** The dashboards' Go definitions move to a library, `internal/dashboards`. The Grafana generator (`internal/tools/dashboards`) and the hub both read them. The Grafana JSON is unchanged, and ADR-0009's rules hold for both: every `gravel_*` metric is on a panel, and a panel says when it may be empty.
2. **The hub proxies; the browser never sees the store.** `hub.yaml metrics.query_url` names a Prometheus-compatible store (anything serving `/api/v1/query_range`: Prometheus, VictoriaMetrics), with an optional `bearer_token_file`. `MetricsService.ListDashboards` returns the definitions and whether a store is configured. `MetricsService.QueryRange` returns one panel's series over a range. Both need `metrics:read` (operators, admins, the owner), in person.
3. **The allow-list is the definitions themselves.** A caller names a dashboard and a panel, never an expression. The hub expands the panel's own expressions with its own variables: `$job` is the configured scrape job, `$instance` is every instance, and `$__rate_interval` is at least 60 s and two steps. No caller input reaches PromQL. A panel the hub doesn't define is `not_found`.
4. **Bounded.** A step is at least 15 s and gets widened to keep a line under 11,000 points. A range is at most 30 days, a panel at most 500 lines, an answer at most 8 MiB, and a query at most `metrics.timeout` (10 s). A store that doesn't answer is `unavailable`; one that refuses is `failed_precondition`; a panel that's too big is `resource_exhausted`. Each query is counted in `gravel_metrics_queries_total{result}`.
5. **Nothing is required.** With no store configured, `/admin/metrics` says how to set one up and every other page works (the unplug test, ADR-0025). Grafana stays supported for those who run it (ADR-0009); it is no longer the only way to see the dashboards.

## Consequences

- An operator sees the hub's and the bot's panels at `/admin/metrics` with their own login, and needs no Grafana.
- A single-machine host can run VictoriaMetrics, one small process, instead of Prometheus plus Grafana. Which store each tier ships with, and logs, is ADR-0032's and #209's to decide.
- The hub holds no series. A store that's down only costs the metrics page.
- The admin dashboard gains a charting library, uPlot (about 50 kB), drawn on a canvas, with each line's latest value in text for screen readers.
- A host's own metrics belong on the host's own dashboards. The hub draws only gravel's.

## As built (#204, 2026-10-15)

- `internal/promquery` is the store client: `POST /api/v1/query_range` with a form body. It handles Prometheus' and VictoriaMetrics' answers (the latter's `isPartial` and `stats` are ignored), leaves NaN samples out, and maps errors to `ErrUnavailable` (no answer, 5xx, not the query API), `ErrRefused` (4xx, status `error`, not a matrix) and `ErrTooLarge`.
- `dashboards.ExpandFor` does the expansion; a test runs it over every shipped expression and fails on any `$` left behind. Panel ids are `r<row>p<panel>`, stable within a hub version.
- `/admin/metrics`: a dashboard per tab, a range picker (an hour to 30 days, about 240 points per line), stat tiles coloured by their mappings and thresholds, and uPlot time series that follow the theme's colours and resize with the page.
