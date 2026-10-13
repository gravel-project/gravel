// What the fleet page says about a server's health and how long ago something was, kept apart from
// the page so the words are tested on their own. Times are measured against the summary's as_of,
// the hub's clock, so a viewer's wrong clock never makes a server look stale.
import { pollInterval } from "./queries";

export type Health = "up" | "degraded" | "down" | "idle" | "unknown";

// healthStyle is a health's label and its pill: the colour fills the pill and the text on it is
// black, which reads on every status colour in both schemes; the label carries the meaning.
export const healthStyle: Record<Health, { label: string; pill: string }> = {
  down: { label: "Down", pill: "bg-error" },
  degraded: { label: "Degraded", pill: "bg-warn" },
  unknown: { label: "Unknown", pill: "bg-info" },
  up: { label: "Up", pill: "bg-ok" },
  idle: { label: "Idle", pill: "bg-info" },
};

export function health(h: string): Health {
  return h in healthStyle ? (h as Health) : "unknown";
}

// trouble reports whether a server's row should stand out.
export function trouble(h: string): boolean {
  return h === "down" || h === "degraded";
}

// duration says how long from then to now, to the unit that matters: "8 s", "3 min", "2 h", "4 d";
// "" under a second and for a moment after now.
export function duration(then: Date, now: Date): string {
  const s = Math.floor((now.getTime() - then.getTime()) / 1000);
  if (s < 1) {
    return "";
  }
  if (s < 60) {
    return `${s} s`;
  }
  if (s < 3600) {
    return `${Math.floor(s / 60)} min`;
  }
  if (s < 86400) {
    return `${Math.floor(s / 3600)} h`;
  }
  return `${Math.floor(s / 86400)} d`;
}

// ago is duration as a moment past: "8 s ago", or "just now".
export function ago(then: Date, now: Date): string {
  const d = duration(then, now);
  return d === "" ? "just now" : d + " ago";
}

// staleAfter is how long the page goes without a fresh summary before it says so: three polls.
export const staleAfter = 3 * pollInterval;

// stale reports whether the summary on screen is old: the last poll failed, or none has answered
// for staleAfter (fetchedAt and now are the viewer's clock, both).
export function stale(fetchedAt: number, now: number, lastFailed: boolean): boolean {
  return lastFailed || now - fetchedAt > staleAfter;
}

// clock is a time of day in the viewer's zone, to the second.
export function clock(d: Date): string {
  return d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}
