// A server's feed gaps (ADR-0011 §5, gravel#3) in words, as the member server page says them
// (internal/web/templates GapWords): every reason the hub's gap detectors write, and anything
// else as missing data.
import type { Gap } from "./gen/gravel/hub/v1/stats_pb";

function count(n: number, one: string, many: string): string {
  return n === 1 ? `1 ${one}` : `${n} ${many}`;
}

export function gapWords(g: Pick<Gap, "reason" | "events" | "kills" | "deaths" | "players">): string {
  switch (g.reason) {
    case "hub_down":
      return "The hub was down, and the events the server sent meanwhile were lost.";
    case "silence":
      return "The feed went quiet during a full match.";
    case "dropped":
      return `The server gave up on ${count(g.events, "event", "events")} it couldn't send.`;
    case "reconcile":
      return `The feed was short ${count(g.kills, "kill", "kills")} and ${count(g.deaths, "death", "deaths")} for ${count(g.players, "player", "players")} in a match.`;
  }
  return "Some of the feed is missing.";
}

// span is a gap's length as the member page shows it: "9m", "1h 35m".
export function span(from: Date, to: Date): string {
  const s = Math.max(0, Math.floor((to.getTime() - from.getTime()) / 1000));
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  return h === 0 ? `${m}m` : `${h}h ${String(m).padStart(2, "0")}m`;
}
