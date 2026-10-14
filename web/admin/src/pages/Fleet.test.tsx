import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { axe } from "vitest-axe";

import { type Fleet, member, renderShell } from "../test/rig";

const asOf = new Date("2026-10-13T22:00:00Z");
const before = (s: number) => timestampFromDate(new Date(asOf.getTime() - s * 1000));

// A fleet in every health, in the hub's order: down, degraded, unknown, up, idle.
const fleet: Fleet = {
  asOf: timestampFromDate(asOf),
  totals: { servers: 5, up: 1, degraded: 1, down: 1, idle: 1, unknown: 1, players: 31, capacity: 190 },
  alarms: [
    { kind: "server.down", severity: "critical", serverId: "htg-wardogs-2", since: before(180), detail: "not answering" },
    { kind: "backup.stale", severity: "warning", since: before(3600 * 27), detail: "no base backup has succeeded since 2026-10-12 19:00 UTC" },
  ],
  servers: [
    { id: "htg-wardogs-2", name: "HTG WARDOGS #2", gameId: "wardogs", gameName: "War Dogs", health: "down", reason: "not answering", healthSince: before(300), misses: 6, maxPlayers: 80 },
    { id: "htg-cs2-1", name: "HTG CS2 #1", gameId: "cs2", gameName: "Counter-Strike 2", health: "degraded", reason: "the host is booting", hostState: "booting", leaseState: "starting", maxPlayers: 10 },
    { id: "htg-wardogs-3", name: "HTG WARDOGS #3", gameId: "wardogs", gameName: "War Dogs", health: "unknown", reason: "not polled yet" },
    { id: "htg-wardogs-1", name: "HTG WARDOGS #1", gameId: "wardogs", gameName: "War Dogs", health: "up", reason: "answering", players: 31, maxPlayers: 100, map: "Ozeti", observedAt: before(8), pollMs: 120 },
    { id: "htg-cs2-2", name: "HTG CS2 #2", gameId: "cs2", gameName: "Counter-Strike 2", health: "idle", reason: "parked, as asked", hostState: "parked" },
  ],
};

async function rows(): Promise<HTMLElement[]> {
  const list = within((await screen.findByRole("heading", { name: "Servers" })).closest("section") as HTMLElement);
  return list.getAllByRole("listitem");
}

// nth is the ith element, failing the test when there is none.
function nth(els: HTMLElement[], i: number): HTMLElement {
  const el = els[i];
  if (el === undefined) {
    throw new Error(`no element ${i} of ${els.length}`);
  }
  return el;
}

describe("the fleet", () => {
  it("shows the summary, the alarms and every server, trouble first", async () => {
    const { container } = renderShell({ me: member({ capabilities: ["servers:read"] }), fleet });
    const r = await rows();
    expect(screen.getByRole("heading", { level: 1, name: "Fleet" })).toBeTruthy();
    const strip = within(nth(screen.getAllByRole("term"), 0).closest("dl") as HTMLElement);
    expect(strip.getAllByRole("term").map((t) => t.textContent)).toEqual(["Up", "Players", "In alarm"]);
    expect(strip.getAllByRole("definition").map((d) => d.textContent)).toEqual(["1 of 4", "1 down · 1 degraded · 1 unknown · 1 idle", "31 of 190", "2"]);

    const alarms = within(screen.getByRole("heading", { name: "2 alarms" }).closest("section") as HTMLElement).getAllByRole("listitem");
    expect(nth(alarms, 0).textContent).toBe("Criticalserver.downhtg-wardogs-2not answeringsince 3 min agoEvents");
    expect(within(nth(alarms, 0)).getByRole("link", { name: "htg-wardogs-2" }).getAttribute("href")).toBe("/servers/htg-wardogs-2");
    // Each alarm opens the events feed on what it is about (gravel#193).
    expect(within(nth(alarms, 0)).getByRole("link", { name: "Events" }).getAttribute("href")).toBe("/admin/events?server=htg-wardogs-2");
    expect(within(nth(alarms, 1)).getByRole("link", { name: "Events" }).getAttribute("href")).toBe("/admin/events?kind=backup");
    expect(nth(alarms, 1).textContent).toContain("Warningbackup.stale");
    expect(nth(alarms, 1).textContent).toContain("since 1 d ago");

    // The hub's order, kept: trouble first, and trouble marked.
    expect(r.map((li) => within(li).getByRole("link").textContent)).toEqual(["HTG WARDOGS #2", "HTG CS2 #1", "HTG WARDOGS #3", "HTG WARDOGS #1", "HTG CS2 #2"]);
    expect(r.map((li) => li.firstElementChild?.textContent)).toEqual(["Down", "Degraded", "Unknown", "Up", "Idle"]);
    expect(r.map((li) => li.className.includes("border-l-error"))).toEqual([true, true, false, false, false]);
    expect(nth(r, 0).textContent).toContain("War Dogs · not answering · for 5 min");
    // A server that never answered has no player count, not a zero one.
    expect(nth(r, 0).textContent).toContain("no players known");
    expect(nth(r, 0).textContent).toContain("never answered");
    expect(nth(r, 0).querySelector("meter")).toBeNull();
    expect(nth(r, 1).textContent).toContain("host booting · lease starting");
    expect(nth(r, 3).textContent).toContain("31 / 100 players");
    expect(nth(r, 3).textContent).toContain("Ozeti");
    expect(nth(r, 3).textContent).toContain("polled 8 s ago · 120 ms");
    expect(nth(r, 4).textContent).toContain("host parked");
    expect(nth(r, 4).textContent).not.toContain("never answered"); // parked, as asked: not expected to
    expect(within(nth(r, 3)).getByRole("link").getAttribute("href")).toBe("/servers/htg-wardogs-1");
    expect(screen.getByText(/^As of/)).toBeTruthy();
    expect(await axe(container)).toHaveNoViolations();
  });

  it("shows a quiet fleet without an alarm banner", async () => {
    const quiet: Fleet = {
      asOf: timestampFromDate(asOf),
      totals: { servers: 1, up: 1, players: 0, capacity: 100 },
      servers: [{ id: "htg-wardogs-1", name: "HTG WARDOGS #1", health: "up", reason: "answering", maxPlayers: 100, observedAt: before(2) }],
    };
    const { container } = renderShell({ me: member(), fleet: quiet });
    expect(await rows()).toHaveLength(1);
    expect(screen.queryByRole("heading", { name: /alarm/ })).toBeNull();
    expect(await axe(container)).toHaveNoViolations();
  });

  it("tells the host how to add servers to an empty fleet", async () => {
    const { container } = renderShell({ me: member() });
    expect(await screen.findByRole("heading", { name: "No servers yet" })).toBeTruthy();
    expect(screen.getByText("gravel-hub servers apply servers.yaml")).toBeTruthy();
    expect(await axe(container)).toHaveNoViolations();
  });

  it("says so when the member's roles don't include the fleet", async () => {
    const { container } = renderShell({ me: member(), fleet: new ConnectError("no", Code.PermissionDenied) });
    expect(await screen.findByText(/do not include seeing the fleet/)).toBeTruthy();
    expect(await axe(container)).toHaveNoViolations();
  });

  it("says when the hub did not answer", async () => {
    renderShell({ me: member(), fleet: new ConnectError("connection refused", Code.Unavailable) });
    expect((await screen.findByRole("alert")).textContent).toMatch(/The hub did not answer: connection refused/);
  });
});
