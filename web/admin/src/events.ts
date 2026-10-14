// The events feed's logic (gravel#193), apart from the page so it is tested on its own: the filters
// as they sit in the address (so a link can open the feed filtered), and the cursor merge. The feed
// is newest first; new events come oldest first after the cursor, older pages newest first before
// it (ListEvents, ADR-0028), and every merge keeps one copy of each id, in order.
import type { Event, ListEventsRequest } from "./gen/gravel/hub/v1/organization_pb";

export interface Filters {
  // kind is a kind ("server.down") or a group ("server"); "" for every kind.
  kind: string;
  // severity is the lowest shown: "info" (or ""), "notice", "warning", "critical".
  severity: string;
  server: string;
  game: string;
  actorUser: string;
  actorApp: string;
}

export const noFilters: Filters = { kind: "", severity: "", server: "", game: "", actorUser: "", actorApp: "" };

// params are the filters' names in the address: /admin/events?server=htg-wardogs-1&severity=warning.
const params: Record<keyof Filters, string> = {
  kind: "kind",
  severity: "severity",
  server: "server",
  game: "game",
  actorUser: "actor_user",
  actorApp: "actor_app",
};

export function filtersFrom(search: URLSearchParams): Filters {
  const f = { ...noFilters };
  for (const key of Object.keys(params) as (keyof Filters)[]) {
    f[key] = search.get(params[key])?.trim() ?? "";
  }
  return f;
}

// search is the address's query for f, without the empty filters; "" for none.
export function search(f: Filters): string {
  const s = new URLSearchParams();
  for (const key of Object.keys(params) as (keyof Filters)[]) {
    if (f[key] !== "") {
      s.set(params[key], f[key]);
    }
  }
  const q = s.toString();
  return q === "" ? "" : "?" + q;
}

// filtered reports whether any filter is set.
export function filtered(f: Filters): boolean {
  return search(f) !== "";
}

// request is ListEvents's filter fields for f.
export function request(f: Filters): Pick<ListEventsRequest, "kinds" | "minSeverity" | "serverId" | "gameId" | "actorUserId" | "actorAppId"> {
  return {
    kinds: f.kind === "" ? [] : [f.kind],
    minSeverity: f.severity === "info" ? "" : f.severity,
    serverId: f.server,
    gameId: f.game,
    actorUserId: f.actorUser,
    actorAppId: f.actorApp,
  };
}

// newer puts events that came after the cursor (oldest first) on top of the feed (newest first).
export function newer(feed: readonly Event[], batch: readonly Event[]): Event[] {
  const top = feed[0]?.id ?? 0n;
  const fresh = batch.filter((e) => e.id > top).sort((a, b) => (a.id < b.id ? 1 : a.id > b.id ? -1 : 0));
  return dedupe([...fresh, ...feed]);
}

// older puts a page from before the feed's oldest event (newest first) under it.
export function older(feed: readonly Event[], page: readonly Event[]): Event[] {
  const bottom = feed[feed.length - 1]?.id;
  const below = bottom === undefined ? [...page] : page.filter((e) => e.id < bottom);
  return dedupe([...feed, ...below]);
}

function dedupe(evs: Event[]): Event[] {
  const seen = new Set<bigint>();
  return evs.filter((e) => {
    if (seen.has(e.id)) {
      return false;
    }
    seen.add(e.id);
    return true;
  });
}

// severityStyle is a severity's label and pill, like the fleet's health: the colour fills the pill
// and the text on it is black; info gets an outline, it asks nothing of anyone.
export const severityStyle: Record<string, { label: string; pill: string }> = {
  critical: { label: "Critical", pill: "bg-error text-black" },
  warning: { label: "Warning", pill: "bg-warn text-black" },
  notice: { label: "Notice", pill: "bg-info text-black" },
  info: { label: "Info", pill: "border border-line text-muted" },
};

export function severity(s: string): { label: string; pill: string } {
  return severityStyle[s] ?? { label: s, pill: "border border-line text-muted" };
}
