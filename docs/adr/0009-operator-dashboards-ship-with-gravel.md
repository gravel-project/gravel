# ADR-0009: Operator dashboards ship with gravel

**Status:** proposed (2026-10-09) · **Changes:** what a gravel release contains, how a metric is renamed, how an operator gets dashboards · **Tracked by:** #61; #60 (the metrics it needs); hidden-token-gaming/htg#29 (the first consumer)

## Context

The hub and the bot expose Prometheus metrics (docs/hub.md and docs/bot.md, Observability), and the README says Grafana is optional and operator-facing. gravel ships nothing to look at them with, so every operator would write their own scrape config and dashboards. Each copy breaks silently the moment a metric is renamed: a panel with a wrong name shows "No data", which reads like "nothing happened".

The first consumer, Hidden Token Gaming, needs a dashboard now. Its host was going to write one in its deploy repo, the copy this ADR exists to prevent.

## Decision

1. **gravel ships the dashboards and the scrape config** in `deploy/observability/`: a Prometheus config for compose and one for the quadlets, Grafana provisioning (a datasource whose address comes from `GRAVEL_PROMETHEUS_URL`, and a file provider), and one dashboard each for the hub and the bot. They are portable: a datasource variable and job and instance variables, no datasource uid or job name in a panel.
2. **The dashboards are generated** from Go definitions (`internal/tools/dashboards`, `make generate`) and committed; `make generate-check` fails on a stale file. They are provisioned read-only (`allowUiUpdates: false`): a change is a gravel PR, never an edit in Grafana.
3. **Metric names are a public interface.** Every `gravel_*` metric the hub or the bot registers is on a panel, and every `gravel_*` name a panel reads is registered: a test fails otherwise (the hub and the bot report their names through `MetricNames()`). A rename is a CHANGELOG `Changed` entry with the dashboards updated in the same PR.
4. **A panel says whether it may be empty.** A panel that can have no data on a healthy deployment (a feature not configured, nothing happened yet) carries the reason. `dashboards check` runs every panel's query through a live Grafana and fails on an empty panel that doesn't. CI runs it against a throwaway hub; an operator can run it against their own deployment.
5. **Operators take them from a release.** The quadlet set in `deploy/quadlet/observability/` and `make up-observability` use the files at the release in hand. A host that vendors them pins the gravel tag it copied from.
6. **Grafana stays optional, operator-facing and on loopback.** Nothing in gravel depends on it. Alerting (rules, Alertmanager) is not part of this decision.

## Consequences

- An operator gets working dashboards with the release, and a gravel upgrade brings dashboards that match its metrics.
- Renaming a metric costs a dashboard edit in the same PR; the test makes forgetting impossible.
- CI gains a job that runs Prometheus, Grafana, Postgres and the hub (about a minute), plus promtool over every panel query.
- The dashboards are what gravel can say about itself. A host's own metrics belong on the host's own dashboards beside these.
- Two images join the release's pins (Prometheus and Grafana by index digest), moved like the others (docs/releasing.md).
