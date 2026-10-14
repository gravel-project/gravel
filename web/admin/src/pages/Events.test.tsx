import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { axe } from "vitest-axe";

import { type LogEvent, member, renderShell } from "../test/rig";

const t0 = new Date("2026-10-14T02:00:00Z");

// log is n events, oldest first, ids 1..n, a server's and the hub's in turn.
function log(n: number): LogEvent[] {
  return Array.from({ length: n }, (_, i) => event(i + 1));
}

function event(id: number, fields: LogEvent = {}): LogEvent {
  return {
    id: BigInt(id),
    at: timestampFromDate(new Date(t0.getTime() + id * 60_000)),
    kind: "server.unreachable",
    severity: "warning",
    serverId: "htg-wardogs-1",
    gameId: "wardogs",
    summary: `event ${id}`,
    ...fields,
  };
}

const staff = member({ capabilities: ["events:read", "servers:read"] });

async function feed(): Promise<HTMLElement[]> {
  const list = await screen.findByRole("region", { name: "The events log" });
  return within(list).getAllByRole("listitem");
}

function summaries(rows: HTMLElement[]): string[] {
  return rows.map((r) => r.querySelector("p")?.textContent ?? "");
}

describe("the events feed", () => {
  it("shows the newest page with what each event is about and who did it", async () => {
    const entries: LogEvent[] = [
      event(1, { kind: "role.granted", severity: "notice", serverId: "", gameId: "cs2", summary: "Jo was granted operator for cs2", actorUserId: "u2", actorName: "Owner" }),
      event(2, { kind: "lease.ready", severity: "info", serverId: "cs2-1", gameId: "cs2", summary: "lease ready", actorAppId: "a1", actorName: "HTG Bot" }),
      event(3, { kind: "server.down", severity: "critical", summary: "HTG WARDOGS #1 is down", detail: { misses: "6" } }),
    ];
    const { container, calls } = renderShell({
      me: staff,
      path: "/events",
      log: entries,
      kinds: [
        { name: "server.down", group: "server", severity: "critical" },
        { name: "server.unreachable", group: "server", severity: "warning" },
        { name: "role.granted", group: "role", severity: "notice" },
      ],
      servers: [{ id: "htg-wardogs-1", name: "HTG WARDOGS #1" }],
      games: [{ id: "wardogs", name: "War Dogs" }],
    });
    const rows = await feed();
    expect(screen.getByRole("heading", { level: 1, name: "Events" })).toBeTruthy();
    expect(summaries(rows)).toEqual(["HTG WARDOGS #1 is down", "lease ready", "Jo was granted operator for cs2"]);
    expect(rows.map((r) => r.querySelector("span")?.textContent)).toEqual(["Critical", "Info", "Notice"]);
    const down = within(rows[0] as HTMLElement);
    expect(down.getByRole("link", { name: "htg-wardogs-1" }).getAttribute("href")).toBe("/servers/htg-wardogs-1");
    expect(down.getByRole("link", { name: "Only htg-wardogs-1's events" }).getAttribute("href")).toBe("/admin/events?server=htg-wardogs-1");
    expect(down.getByRole("link", { name: "server.down" }).getAttribute("href")).toBe("/admin/events?kind=server.down");
    expect(down.getByRole("link", { name: "wardogs" }).getAttribute("href")).toBe("/admin/events?game=wardogs");
    expect(rows[0]?.textContent).toContain("misses: 6");
    expect(within(rows[1] as HTMLElement).getByRole("link", { name: "Only what HTG Bot did" }).getAttribute("href")).toBe("/admin/events?actor_app=a1");
    expect(within(rows[2] as HTMLElement).getByRole("link", { name: "Only what Owner did" }).getAttribute("href")).toBe("/admin/events?actor_user=u2");
    expect(rows[0]?.querySelector("time")?.getAttribute("datetime")).toBe("2026-10-14T02:03:00.000Z");
    expect(screen.getByText("That is the start of the log.")).toBeTruthy();
    expect(calls[0]).toMatchObject({ afterId: 0n, beforeId: 0n, limit: 50, kinds: [], serverId: "" });
    await screen.findByRole("option", { name: "server.down" });
    expect(screen.getByRole("option", { name: "Every server event" })).toBeTruthy();
    expect(screen.getByRole("option", { name: "HTG WARDOGS #1" })).toBeTruthy();
    expect(await axe(container)).toHaveNoViolations();
  });

  it("follows new events from the head, without duplicates", async () => {
    const entries = log(3);
    const { calls } = renderShell({ me: staff, path: "/events", log: entries, poll: 20 });
    await feed();
    await waitFor(() => {
      expect(calls.length).toBeGreaterThanOrEqual(2);
    });
    expect(calls[1]).toMatchObject({ afterId: 3n, limit: 500 });
    entries.push(event(4, { summary: "event 4" }), event(5, { summary: "event 5" }));
    await waitFor(() => {
      expect(screen.getByText("event 5")).toBeTruthy();
    });
    // Later polls ask after the newest seen, and the feed keeps one row per event.
    await waitFor(() => {
      expect(calls.some((c) => c.afterId === 5n)).toBe(true);
    });
    expect(summaries(await feed())).toEqual(["event 5", "event 4", "event 3", "event 2", "event 1"]);
  });

  it("pages back through the log", async () => {
    const { calls } = renderShell({ me: staff, path: "/events", log: log(120) });
    expect(await feed()).toHaveLength(50);
    fireEvent.click(screen.getByRole("button", { name: "Older events" }));
    await waitFor(async () => {
      expect(await feed()).toHaveLength(100);
    });
    expect(calls[calls.length - 1]).toMatchObject({ beforeId: 71n, limit: 50 });
    fireEvent.click(screen.getByRole("button", { name: "Older events" }));
    await screen.findByText("That is the start of the log.");
    const rows = await feed();
    expect(rows).toHaveLength(120);
    expect(summaries(rows)).toEqual(Array.from({ length: 120 }, (_, i) => `event ${120 - i}`));
  });

  it("reads its filters from the address and changes them there", async () => {
    const entries = [event(1), event(2, { serverId: "htg-wardogs-2", summary: "other server" }), event(3, { severity: "notice", summary: "quiet" })];
    const { calls, container } = renderShell({
      me: staff,
      path: "/events?server=htg-wardogs-1&severity=warning",
      log: entries,
      servers: [
        { id: "htg-wardogs-1", name: "HTG WARDOGS #1" },
        { id: "htg-wardogs-2", name: "HTG WARDOGS #2" },
      ],
    });
    expect(summaries(await feed())).toEqual(["event 1"]);
    expect(calls[0]).toMatchObject({ serverId: "htg-wardogs-1", minSeverity: "warning" });
    expect(screen.getByLabelText<HTMLSelectElement>("Severity").value).toBe("warning");

    await screen.findByRole("option", { name: "HTG WARDOGS #2" });
    fireEvent.change(screen.getByLabelText("Server"), { target: { value: "htg-wardogs-2" } });
    await waitFor(() => {
      expect(screen.getByText("other server")).toBeTruthy();
    });
    expect(calls[calls.length - 1]).toMatchObject({ serverId: "htg-wardogs-2", minSeverity: "warning", afterId: 0n });

    fireEvent.click(screen.getByRole("link", { name: "Clear the filters" }));
    await waitFor(async () => {
      expect(summaries(await feed())).toEqual(["quiet", "other server", "event 1"]);
    });
    expect(await axe(container)).toHaveNoViolations();
  });

  it("filters by who acted and says how to see everyone's", async () => {
    renderShell({ me: staff, path: "/events?actor_user=u2", log: [event(1, { actorUserId: "u2", actorName: "Owner" }), event(2)] });
    expect(summaries(await feed())).toEqual(["event 1"]);
    expect(screen.getByText(/Only what one member did/)).toBeTruthy();
    expect(screen.getByRole("link", { name: "Everyone's" }).getAttribute("href")).toBe("/admin/events");
  });

  it("says when nothing has happened, and when nothing matches", async () => {
    const { container } = renderShell({ me: staff, path: "/events" });
    expect(await screen.findByRole("heading", { name: "No events yet" })).toBeTruthy();
    expect(await axe(container)).toHaveNoViolations();
  });

  it("says when nothing matches the filters", async () => {
    renderShell({ me: staff, path: "/events?kind=backup", log: log(3) });
    expect(await screen.findByRole("heading", { name: "No events match" })).toBeTruthy();
  });

  it("says so when the member's roles don't include the log", async () => {
    const { container } = renderShell({ me: member(), path: "/events", log: new ConnectError("no", Code.PermissionDenied) });
    expect(await screen.findByText(/do not include reading the events log/)).toBeTruthy();
    expect(await axe(container)).toHaveNoViolations();
  });

  it("says when the hub did not answer", async () => {
    renderShell({ me: staff, path: "/events", log: new ConnectError("connection refused", Code.Unavailable) });
    expect((await screen.findByRole("alert")).textContent).toMatch(/The hub did not answer: connection refused/);
  });
});

describe("the nav", () => {
  it("offers the feed to staff of one game, and not to a plain member", async () => {
    renderShell({ me: member({ grants: [{ role: "moderator", gameId: "cs2" }] }), path: "/events", log: log(1) });
    const nav = within(await screen.findByRole("navigation", { name: "Admin" }));
    expect(nav.getByRole("link", { name: "Events" }).getAttribute("aria-current")).toBe("page");
  });

  it("hides it from a member without events:read", async () => {
    renderShell({ me: member({ capabilities: ["servers:read"] }) });
    const nav = within(await screen.findByRole("navigation", { name: "Admin" }));
    expect(nav.queryByRole("link", { name: "Events" })).toBeNull();
  });
});
