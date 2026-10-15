// The operator dashboards' helpers (gravel#204): ranges, units as people read them, a stat's value
// and its colour.
import type { MetricsPanel, MetricsSeries } from "./gen/gravel/hub/v1/metrics_pb";

// ranges are the picker's choices, by how far back they reach.
export const ranges = [
  { key: "1h", label: "Last hour", ms: 3_600_000 },
  { key: "6h", label: "Last 6 hours", ms: 6 * 3_600_000 },
  { key: "24h", label: "Last 24 hours", ms: 24 * 3_600_000 },
  { key: "7d", label: "Last 7 days", ms: 7 * 24 * 3_600_000 },
  { key: "30d", label: "Last 30 days", ms: 30 * 24 * 3_600_000 },
] as const;

export type RangeKey = (typeof ranges)[number]["key"];

// stepFor is a step that draws about 240 points across a range, never under 15 s.
export function stepFor(ms: number): number {
  return Math.max(15_000, Math.ceil(ms / 240 / 1000) * 1000);
}

// short is a number with as many decimals as its size needs (2 under 10, 1 under 100), trailing
// zeros dropped.
const short = (x: number): string => String(Number(x.toFixed(Math.abs(x) >= 100 ? 0 : Math.abs(x) >= 10 ? 1 : 2)));

const scaled = (v: number, base: number, units: readonly string[]): string => {
  let i = 0;
  let x = Math.abs(v);
  while (x >= base && i < units.length - 1) {
    x /= base;
    i++;
  }
  return `${short(Math.sign(v) * x)} ${units[i] ?? ""}`.trim();
};

// format is a value in a panel's unit (Grafana's names), short enough for an axis or a tile.
export function format(v: number, unit: string): string {
  if (!Number.isFinite(v)) {
    return v > 0 ? "∞" : "—";
  }
  switch (unit) {
    case "bytes":
      return scaled(v, 1024, ["B", "KiB", "MiB", "GiB", "TiB"]);
    case "percentunit":
      return `${short(v * 100)}%`;
    case "percent":
      return `${v.toFixed(0)}%`;
    case "s":
      if (Math.abs(v) < 1) {
        return `${short(v * 1000)} ms`;
      }
      return Math.abs(v) < 120 ? `${short(v)} s` : Math.abs(v) < 7200 ? `${short(v / 60)} min` : Math.abs(v) < 172_800 ? `${short(v / 3600)} h` : `${short(v / 86_400)} d`;
    case "reqps":
      return `${scaled(v, 1000, ["", "k", "M"])}/s`;
    default:
      return scaled(v, 1000, ["", "k", "M", "G"]);
  }
}

// latest is the last value of a series, or undefined when it has none.
export function latest(s: MetricsSeries | undefined): number | undefined {
  return s && s.values.length > 0 ? s.values[s.values.length - 1] : undefined;
}

// statOf is what a stat panel shows for a value: its mapped text, or the value in its unit, and the
// colour its mapping or threshold gives it ("" for none).
export function statOf(p: MetricsPanel, v: number | undefined): { text: string; color: string } {
  if (v === undefined) {
    return { text: "No data", color: "" };
  }
  const m = p.mappings.find((x) => Number(x.value) === v);
  if (m) {
    return { text: m.text, color: m.color };
  }
  let color = "";
  for (const t of p.thresholds) {
    if (color === "" || v >= t.value) {
      color = t.color;
    }
  }
  return { text: format(v, p.unit), color };
}

// tone maps a Grafana colour name onto the dashboard's semantic colours.
export function tone(color: string): "ok" | "warn" | "error" | "" {
  switch (color) {
    case "green":
      return "ok";
    case "yellow":
    case "orange":
      return "warn";
    case "red":
      return "error";
  }
  return "";
}
