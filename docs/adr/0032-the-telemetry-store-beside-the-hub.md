# ADR-0032: The telemetry store beside the hub

**Status:** proposed (2026-11-05, from the spike hidden-token-gaming/deploy#201: two sixteen-day trials beside production on Hidden Token Gaming's hub; accepted when merged) · **Changes:** what a tier ships for telemetry; amends ADR-0009 (Prometheus and Grafana stop being the release's default pins) and settles ADR-0030 §5's open line ("which store each tier ships with, and logs") · **Decided in:** hidden-token-gaming/deploy#201 · **Part of:** H1 (#182), framed by ADR-0025; unblocks #209 (the pipeline) and #212 (the stock host's metrics endpoint)

## Context

ADR-0025 left the telemetry store undecided on purpose: measure first. ADR-0030 then moved the operator dashboards into the hub, behind a query proxy that speaks to any Prometheus-compatible store, so Grafana stopped being the only way to see them. What remained was to find out what each candidate costs on the machine a hub actually runs on, and what a hosted fleet (GRVL) needs from the hub's side.

The first look (`podman stats`, 2026-10-13) was misleading: it reports a cgroup's whole charge, page cache included, and called Grafana 508 MB when its own memory is about a third of that. The trials read each container's cgroup every five minutes for sixteen days and keep anonymous memory, cache, CPU and disk apart.

**Where they ran.** Hidden Token Gaming's hub, `htg-hub-1`, a 2 vCPU / 4 GB DigitalOcean Droplet carrying the hub, the bot, Muster, Postgres, Prometheus (30-day retention, 27 alert rules), Alertmanager and Grafana, in production the whole time: roughly 2,100 to 2,400 series from five targets at 15 s, and 2,200 journal lines an hour from the hub, the bot and Muster. The trials ran beside that stack (hidden-token-gaming/deploy#230, #231), scraping the same targets and reading the same journal, on loopback, and were stopped on 2026-11-05 with their samples kept on the host.

## Measurements

**Trial 1, the metrics store** (2026-10-20 19:15Z to 2026-11-05 16:05Z, 380.8 h; 4,571 samples per container, no gaps, no restarts). Memory in MB from the cgroup: anonymous (median / p95 / max), then the medians of page cache and of the whole charge.

| Container | Anon | Cache | Charged | CPU (mean) | Disk (first → last) | Series | Scrape, sum of 5 targets |
|---|---|---|---|---|---|---|---|
| Prometheus v3 | 52 / 55 / 110 | 141 | 197 | 0.62 % | 120 → 327 MB | 2,360 | 44.1 ms |
| Grafana 13 | 134 / 135 / 153 | 508 | 652 | 1.41 % | 474 → 474 MB | – | – |
| VictoriaMetrics v1.153.0 | 56 / 78 / 87 | 24 | 99 | 0.64 % | 0 → 44 MB | 2,324 | 31.4 ms |

Prometheus's directory already held 30 days of history at the start (its retention), so its figure is growth over the window, WAL and compaction included, not a like-for-like size; VictoriaMetrics started empty. Only Prometheus evaluated the alert rules, which is where the 1.5 % series difference comes from.

**Trial 2, the log stores** (2026-10-20 19:50Z to 2026-11-05 16:05Z, 380.2 h; 4,564 samples per container, no gaps, no restarts). One shipper, Grafana Alloy, read the user journal once and wrote the same 835,897 lines (2,192 an hour) to both stores; both held every line.

| Container | Anon | Cache | Charged | CPU (mean) | Disk (first → last) |
|---|---|---|---|---|---|
| Alloy v1.20.1 (the shipper; an OpenTelemetry Collector distribution) | 34 / 39 / 53 | 169 | 213 | 0.66 % | 0 MB |
| Loki 3.7.8 | 54 / 59 / 64 | 113 | 170 | 0.76 % | 1 → 37 MB |
| VictoriaLogs v1.53.0 | 32 / 40 / 61 | 20 | 64 | 0.40 % | 0 → 36 MB |

**What a hosted fleet needs from the hub** (deploy#201, 2026-10-20; a throwaway measurement on a workstation, seven shippers against a synthetic 2,100-series `/metrics`): pushing from the hub beats pulling from the centre (outbound HTTPS only, a disk buffer across an outage, one credential per hub). `vmagent` pushed at 60 MB RSS and 12.8 bytes a sample (about 4.6 GB a month per hub at 15 s, 1.2 GB at 60 s); Prometheus in agent mode 87–89 MB; Alloy 226 MB; the OpenTelemetry Collector 252 MB with its buffer in memory. A VictoriaMetrics receiver forces the per-hub label (`extra_label`) over anything the client sends, so the label is the centre's, not the hub's; a Prometheus receiver cannot. At 200 hubs (420k series, 28k samples/s) the docs size a single-node VictoriaMetrics at about 2 vCPU / 4 GB and Mimir at about 16 GB at replication 3. VictoriaLogs' `extra_fields` does the same for logs.

**Reading the numbers.** The store was never the weight; Grafana is. VictoriaMetrics costs what Prometheus costs in memory and CPU, writes about a fifth of the disk, scrapes faster, and is the same family as the shipper and receiver a fleet wants. VictoriaLogs costs about 60 % of Loki in memory and half its CPU for the same lines on the same disk, with one binary and no object store to configure. The shipper for logs is a cost of its own (Alloy: 34 MB), and the trials did not measure the lighter ones.

## Decision

1. **The metrics store is VictoriaMetrics, single-node, beside the hub.** It scrapes the hub, the bot and Muster where present at 15 s and keeps 30 days; `hub.yaml metrics.query_url` points at it (its root serves `/api/v1/query_range`), so `/admin/metrics` (ADR-0030) draws from it with nothing else in the path. It is one process on loopback, pinned by digest like the other images (docs/releasing.md).
2. **Grafana is not in a tier's default.** The stock quadlet units and the Compose tier stop shipping it; it becomes an opt-in profile, and ADR-0009's dashboard JSON is still generated, tested and published for whoever runs it. The operator's dashboards are the hub's (ADR-0030).
3. **Alert rules stay Prometheus-format and are evaluated by `vmalert`, with Alertmanager as today.** The 27 rules Hidden Token Gaming runs (hub down, backups stale, a server unreachable, the feed silent with players, the bot's gateway gone, and so on) move unchanged. Over time the rules that describe gravel's own state (servers, cloud VMs, ingest, the bot's jobs) are candidates to become hub events with notifiers (ADR-0028), because the hub knows them first-hand; the rules that say the hub, a backup or Muster is down must stay outside the hub, and `vmalert` is where they live.
4. **The log store, where a tier has one, is VictoriaLogs,** fed from the journal of the hub's units. Logs are optional at every tier (docs/tiers.md): nothing in the hub reads them, and with no store configured nothing changes. The shipper is decided when #209 builds it: `systemd-journal-upload` straight into VictoriaLogs' journald endpoint needs no process of its own and is tried first; Alloy, measured here, is the fallback.
5. **A fleet's copy is pushed by `vmagent` from the hub to the centre's VictoriaMetrics behind `vmauth`,** over outbound HTTPS with one credential per hub, buffered on disk; the receiver forces the `hub` label. The hub binary never ships series itself (ADR-0030: the hub holds none). The stock host (#212) runs `vmagent` only when a remote address is given, so a self-hosted hub carries nothing for a fleet it is not in.
6. **The tiers.** Single machine and Compose: VictoriaMetrics, `vmalert` and Alertmanager by default, VictoriaLogs by profile, Grafana by profile. The stock cloud host: the same, plus `vmagent` when a centre is named. The Kubernetes tier is K1's to settle and is not decided here.

## Not chosen

- **Prometheus and Grafana as today.** It works, and Prometheus's own cost is fine; Grafana is the largest thing on the host (134 MB of its own, 508 MB of cache, 474 MB of disk, the most CPU) for pages the hub now renders, and Prometheus cannot be the fleet's receiver (no forced label) nor its shipper without a second component.
- **Prometheus beside the hub with Grafana dropped.** Half the saving at the same operational cost as the chosen store, and a different family from the fleet's side of the pipe.
- **Mimir, Thanos or Cortex at the centre.** An order of magnitude more memory for the same load, for scale a fleet of two hundred hubs does not need; moving from single-node to the VictoriaMetrics cluster later changes only `vmauth`'s target.
- **Loki.** Heavier than VictoriaLogs on every measure but disk, where they are equal, and it brings an object-store shape a single machine has no use for.
- **An OpenTelemetry Collector or Alloy as the metrics shipper.** Three to four times `vmagent`'s memory for the same bytes, and the Collector buffers in memory by default.
- **A store inside the hub.** The hub holds no series (ADR-0030) and does not become a database for them; the unplug test (ADR-0025) stays a hub with nothing configured.

## Consequences

- The release's observability pins change: VictoriaMetrics, `vmalert` and VictoriaLogs join; Prometheus and Grafana remain published for the opt-in profiles. `deploy/observability/`'s Prometheus config becomes a VictoriaMetrics scrape config (the file format is the same) and the CI job that runs the stack runs the new one; `promtool check rules` keeps checking the rule files, which do not change.
- On Hidden Token Gaming's host the telemetry set goes from about 200 MB of its own memory and 800 MB of disk (Prometheus, Grafana, Alertmanager) to roughly 90 MB and 60 MB plus `vmalert`, measured again at the cut-over, not assumed.
- `/admin/metrics` is unchanged: `internal/promquery` already reads VictoriaMetrics' answers.
- #209 builds the single-machine and Compose shape and cuts Hidden Token Gaming over; #212 adds `vmagent` and the scrape token to the stock host. GRVL's centre (`vmauth` in front of VictoriaMetrics, per-hub credentials, a `count by (hub)` alert as the cardinality guard) is GRVL's own work, designed against this ADR.

## Open before acceptance, and the first facts to check after it

1. `vmalert`'s own memory and CPU beside the store (not measured; the store trial ran without it).
2. The log shipper: whether `systemd-journal-upload` reaches VictoriaLogs' journald endpoint from the hub's systemd (custom headers need systemd 258 or later), else Alloy.
3. Whether a fleet-hosted hub keeps a local store at all, or only pushes (a GRVL question; both work with this ADR).
4. The centre's real memory and disk at 420k series: a one-day load test before GRVL's first hundred hubs.
5. Whether VictoriaMetrics accepts Prometheus remote write 2.0, in case a third-party shipper is ever wanted (not tested; `vmagent` needs no answer).
6. 15 s or 60 s for the fleet's copy; the local store stays at 15 s either way.
