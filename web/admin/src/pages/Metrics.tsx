// Metrics (gravel#204, ADR-0030): the operator dashboards in the hub, from the same definitions as
// the Grafana JSON. Each panel asks the hub for its series by name; the hub runs the panel's own
// expressions against its metrics store and keeps nothing. Without a store, the page says how to
// configure one. metrics:read.
import { durationFromMs, timestampFromMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { useQuery } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import uPlot from "uplot";
import "uplot/dist/uPlot.min.css";

import { type MetricsDashboard, type MetricsPanel, MetricsService, type MetricsSeries } from "../gen/gravel/hub/v1/metrics_pb";
import { format, latest, type RangeKey, ranges, statOf, stepFor, tone } from "../metrics";
import { useClient } from "../transport";

const field = "rounded border border-line bg-surface px-2 py-1";
const button = "rounded border border-line bg-surface px-3 py-1";
const primary = "rounded border-2 border-accent bg-surface px-3 py-1 font-bold";

// refresh is how often a panel asks again: the store's data moves at the scrape interval.
const refresh = 30_000;

export function Metrics() {
  const metrics = useClient(MetricsService);
  const list = useQuery({ queryKey: ["dashboards"], queryFn: ({ signal }) => metrics.listDashboards({}, { signal }), refetchInterval: false, staleTime: Infinity });
  const [uid, setUid] = useState("gravel-hub");
  const [range, setRange] = useState<RangeKey>("6h");
  if (list.error) {
    const refused = ConnectError.from(list.error).code === Code.PermissionDenied;
    return (
      <>
        <h1 className="mb-4 text-2xl font-bold">Metrics</h1>
        <p role={refused ? undefined : "alert"}>
          {refused ? "Your roles do not include the operator dashboards (metrics:read)." : `The hub did not answer: ${ConnectError.from(list.error).rawMessage}.`}
        </p>
      </>
    );
  }
  if (!list.data) {
    return (
      <p role="status" className="text-muted">
        Loading the dashboards…
      </p>
    );
  }
  const boards = list.data.dashboards;
  const board = boards.find((d) => d.uid === uid) ?? boards[0];
  return (
    <>
      <div className="mb-4 flex flex-wrap items-baseline justify-between gap-3">
        <h1 className="text-2xl font-bold">Metrics</h1>
        {list.data.available && (
          <label htmlFor="metrics-range" className="flex items-center gap-2">
            Range
            <select id="metrics-range" className={field} value={range} onChange={(e) => { setRange(e.target.value as RangeKey); }}>
              {ranges.map((r) => (
                <option key={r.key} value={r.key}>
                  {r.label}
                </option>
              ))}
            </select>
          </label>
        )}
      </div>
      {!list.data.available ? (
        <section aria-labelledby="no-store" className="max-w-prose rounded border border-line bg-surface p-4">
          <h2 id="no-store" className="mb-2 text-xl font-bold">
            No metrics store is configured
          </h2>
          <p className="mb-2">
            The hub draws these dashboards from a Prometheus-compatible store that scrapes it: VictoriaMetrics, or Prometheus. Point hub.yaml at it and restart the hub:
          </p>
          <pre className="mb-2 overflow-x-auto rounded border border-line bg-ground p-2 font-mono text-sm">{"metrics:\n  query_url: http://127.0.0.1:8428"}</pre>
          <p className="text-muted">The hub only reads it, and only these dashboards’ own queries. docs/hub.md, Observability, says how to run one beside the hub.</p>
        </section>
      ) : (
        <>
          <div role="group" aria-label="Dashboard" className="mb-4 flex flex-wrap gap-2">
            {boards.map((d) => (
              <button key={d.uid} type="button" className={d.uid === board?.uid ? primary : button} aria-pressed={d.uid === board?.uid} onClick={() => { setUid(d.uid); }}>
                {d.title}
              </button>
            ))}
          </div>
          {board && <Board board={board} range={range} />}
        </>
      )}
    </>
  );
}

function Board({ board, range }: { board: MetricsDashboard; range: RangeKey }) {
  return (
    <div className="grid gap-6">
      {board.description && <p className="max-w-prose text-muted">{board.description}</p>}
      {board.rows.map((row, i) => (
        <section key={row.title + String(i)} aria-labelledby={`row-${String(i)}`}>
          <h2 id={`row-${String(i)}`} className="mb-2 text-xl font-bold">
            {row.title}
          </h2>
          <div className="grid grid-cols-1 gap-3 md:grid-cols-24">
            {row.panels.map((p) => (
              <Panel key={p.id} uid={board.uid} panel={p} range={range} />
            ))}
          </div>
        </section>
      ))}
    </div>
  );
}

// spans are the panel widths on the dashboard's 24-column grid, written out so Tailwind sees them.
const spans = ["", "md:col-span-1", "md:col-span-2", "md:col-span-3", "md:col-span-4", "md:col-span-5", "md:col-span-6", "md:col-span-7", "md:col-span-8",
  "md:col-span-9", "md:col-span-10", "md:col-span-11", "md:col-span-12", "md:col-span-13", "md:col-span-14", "md:col-span-15", "md:col-span-16",
  "md:col-span-17", "md:col-span-18", "md:col-span-19", "md:col-span-20", "md:col-span-21", "md:col-span-22", "md:col-span-23", "md:col-span-24"];

// span is a panel's width as its grid class, exactly as the Grafana dashboard lays it out.
function span(width: number): string {
  return spans[Math.min(24, Math.max(2, width))] ?? "md:col-span-6";
}

function Panel({ uid, panel, range }: { uid: string; panel: MetricsPanel; range: RangeKey }) {
  const metrics = useClient(MetricsService);
  const ms = ranges.find((r) => r.key === range)?.ms ?? 6 * 3_600_000;
  const q = useQuery({
    queryKey: ["panel", uid, panel.id, range],
    queryFn: ({ signal }) => {
      const end = Date.now();
      return metrics.queryRange({ dashboardUid: uid, panelId: panel.id, start: timestampFromMs(end - ms), end: timestampFromMs(end), step: durationFromMs(stepFor(ms)) }, { signal });
    },
    refetchInterval: refresh,
  });
  const series = q.data?.series ?? [];
  return (
    <figure className={`min-w-0 rounded border border-line bg-surface p-3 ${span(panel.width)}`}>
      <figcaption className="mb-1 font-bold" title={panel.description || undefined}>
        {panel.title}
      </figcaption>
      {q.error ? (
        <p className="text-sm">{ConnectError.from(q.error).rawMessage}</p>
      ) : !q.data ? (
        <p className="text-sm text-muted">Loading…</p>
      ) : series.length === 0 ? (
        <p className="text-sm text-muted">{panel.optional ? `No data: ${panel.optional}.` : "No data in this range."}</p>
      ) : panel.kind === "stat" ? (
        <Stat panel={panel} series={series} />
      ) : (
        <Chart panel={panel} series={series} />
      )}
    </figure>
  );
}

function Stat({ panel, series }: { panel: MetricsPanel; series: MetricsSeries[] }) {
  return (
    <ul className="grid gap-1">
      {series.slice(0, 6).map((s) => {
        const { text, color } = statOf(panel, latest(s));
        const t = tone(color);
        return (
          <li key={s.name} className="flex flex-wrap items-baseline justify-between gap-2">
            {series.length > 1 && <span className="min-w-0 truncate text-sm text-muted">{s.name}</span>}
            {/* The state is the border's colour, never the text's: coloured text fails contrast on a light ground. */}
            <span className={`border-l-4 pl-2 text-2xl font-bold tabular-nums ${t === "ok" ? "border-ok" : t === "warn" ? "border-warn" : t === "error" ? "border-error" : "border-transparent"}`}>
              {text}
            </span>
          </li>
        );
      })}
    </ul>
  );
}

// clock is an axis time as the dashboard writes times: 24-hour, in the viewer's zone, with the date
// when the range runs past two days.
function clock(seconds: number, times: number[]): string {
  const days = times.length > 1 ? ((times[times.length - 1] ?? 0) - (times[0] ?? 0)) / 86_400_000 : 0;
  const d = new Date(seconds * 1000);
  const hm = d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", hourCycle: "h23" });
  return days > 2 ? `${d.toLocaleDateString(undefined, { month: "short", day: "numeric" })} ${hm}` : hm;
}

// palette is the series' colours: distinct on the dashboard's light and dark grounds alike.
const palette = ["#e8901a", "#3d8fd1", "#2e9e6e", "#a463d9", "#d6454a", "#b8a12f", "#7f8c99", "#d46aa8"];

// Chart draws a time series panel with uPlot, and says the latest value of each line in words for
// whoever doesn't see the canvas.
function Chart({ panel, series }: { panel: MetricsPanel; series: MetricsSeries[] }) {
  const ref = useRef<HTMLDivElement>(null);
  const summary = series
    .slice(0, 8)
    .map((s) => `${s.name}: ${format(latest(s) ?? Number.NaN, panel.unit)}`)
    .join("; ");
  useEffect(() => {
    const el = ref.current;
    // A browser without a 2-D canvas (a test's DOM) gets the summary below and no drawing.
    if (!el || document.createElement("canvas").getContext("2d") === null) {
      return;
    }
    // One x axis: the union of every series' times, each series aligned onto it.
    const times = [...new Set(series.flatMap((s) => s.times.map(Number)))].sort((a, b) => a - b);
    const index = new Map(times.map((t, i) => [t, i]));
    const ys = series.map((s) => {
      const out: (number | null)[] = new Array<number | null>(times.length).fill(null);
      s.times.forEach((t, i) => {
        const at = index.get(Number(t));
        if (at !== undefined) {
          out[at] = s.values[i] ?? null;
        }
      });
      return out;
    });
    const css = getComputedStyle(el);
    const ink = css.getPropertyValue("--color-muted").trim() || "#888";
    const grid = css.getPropertyValue("--color-line").trim() || "#444";
    const axis = { stroke: ink, grid: { stroke: grid, width: 1 }, ticks: { stroke: grid, width: 1 } };
    let chart: uPlot;
    try {
      chart = new uPlot(
        {
          width: Math.max(240, el.getBoundingClientRect().width),
          height: 180,
          legend: { show: series.length > 1 && series.length <= 8 },
          cursor: { drag: { x: false, y: false } },
          scales: { x: { time: true } },
          axes: [{ ...axis, values: (_u, vals) => vals.map((v) => clock(v, times)) }, { ...axis, values: (_u, vals) => vals.map((v) => format(v, panel.unit)), size: 64 }],
          series: [{}, ...series.map((s, i) => ({ label: s.name, stroke: palette[i % palette.length] ?? "#888", width: 1.5, points: { show: false }, value: (_u: uPlot, v: number | null) => (v === null ? "—" : format(v, panel.unit)) }))],
        },
        [times.map((t) => t / 1000), ...ys],
        el,
      );
    } catch {
      return; // no canvas here (a test's DOM): the summary below still says what the chart shows
    }
    const fit =
      typeof ResizeObserver === "function"
        ? new ResizeObserver(() => {
            chart.setSize({ width: Math.max(240, el.getBoundingClientRect().width), height: 180 });
          })
        : undefined;
    fit?.observe(el);
    return () => {
      fit?.disconnect();
      chart.destroy();
    };
  }, [panel.unit, series]);
  return (
    <>
      <div ref={ref} aria-hidden="true" className="w-full min-w-0" />
      <p className="sr-only">Latest: {summary}</p>
    </>
  );
}
