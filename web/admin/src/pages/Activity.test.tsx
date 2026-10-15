import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { axe } from "vitest-axe";

import { member, renderShell } from "../test/rig";
import { busiest, length, shade } from "./Activity";

const owner = member({ id: "jo", displayName: "Jo", owner: true });

// hours is a week with Wednesday 20:00 the busiest and Saturday 21:00 next.
const hours = Array.from({ length: 168 }, (_, i) => {
  const day = Math.floor(i / 24) + 1;
  const hour = i % 24;
  const average = day === 3 && hour === 20 ? 6.2 : day === 6 && hour === 21 ? 4 : day === 1 && hour === 3 ? 0.4 : 0;
  return { day, hour, average, peak: Math.ceil(average * 1.5) };
});

const month = {
  from: timestampFromDate(new Date("2026-09-15T17:00:00Z")),
  to: timestampFromDate(new Date("2026-10-15T17:00:00Z")),
  hours,
  sessions: 120,
  players: 31,
  medianSessionSeconds: 3900,
  returningPlayers: 19,
  meanActiveDays: 3.42,
  newPlayers: 7,
  lostPlayers: 4,
  weeks: [{ start: timestampFromDate(new Date("2026-09-14T05:00:00Z")), players: 22, newPlayers: 3 }],
  quiet: [
    { name: "Ana", userId: "u-ana", daysBefore: 9, hours: 14.5, lastSeen: timestampFromDate(new Date("2026-09-01T02:00:00Z")), lastServerId: "htg-wardogs-1" },
    { name: "Brave Falcon", pseudonymous: true, daysBefore: 5, hours: 6, lastSeen: timestampFromDate(new Date("2026-08-28T02:00:00Z")), lastServerId: "gone-1" },
  ],
};

const servers = [{ id: "htg-wardogs-1", name: "HTG WARDOGS #1", gameId: "wardogs" }];

describe("player activity", () => {
  it("shows when people play and who stopped", async () => {
    const { container } = renderShell({ me: owner, path: "/activity", activity: { "30d": month }, servers });
    expect(await screen.findByText("Busiest:", { exact: false })).toBeTruthy();
    expect(screen.getByText(/Wednesday 20:00, 6.2 on average \(at most 10\)/)).toBeTruthy();
    expect(screen.getByText(/15 Sept 2026 to 15 Oct 2026, hours and days in America\/Chicago/)).toBeTruthy();
    const tile = (label: string) => screen.getByText(label, { selector: "dt" }).nextElementSibling?.textContent;
    expect(tile("Players")).toBe("31");
    expect(tile("Median session")).toBe("1 h 5 min");
    expect(tile("Came back on another day")).toBe("19 of 31");
    expect(tile("Days played, on average")).toBe("3.4");
    expect(tile("Growth")).toBe("+3");
    const table = screen.getByRole("table", { name: /Players on in each hour of the week/ });
    const wed = within(table).getByRole("rowheader", { name: "Wed" }).parentElement as HTMLElement;
    const cell = within(wed).getAllByRole("cell")[20] as HTMLElement;
    expect(cell.textContent).toBe("6.2 players at 20:00");
    expect(cell.className).toContain("bg-accent/40");
    expect((within(wed).getAllByRole("cell")[19] as HTMLElement).textContent).toBe("nobody at 19:00");
    const quiet = screen.getByRole("region", { name: "Regulars gone quiet" });
    expect(within(quiet).getByRole("link", { name: "Ana" }).getAttribute("href")).toBe("/members/u-ana");
    await waitFor(() => { expect(within(quiet).getByText("HTG WARDOGS #1")).toBeTruthy(); });
    expect(within(quiet).getByText("gone-1")).toBeTruthy();
    expect(within(quiet).queryByRole("link", { name: "Brave Falcon" })).toBeNull();
    expect(await axe(container)).toHaveNoViolations();
  });

  it("asks for another window", async () => {
    const { actions } = renderShell({ me: owner, path: "/activity", activity: { "30d": month, "7d": { ...month, players: 12 } } });
    await screen.findByText("Busiest:", { exact: false });
    fireEvent.click(screen.getByRole("button", { name: "7 days" }));
    await waitFor(() => { expect(screen.getByText("Players", { selector: "dt" }).nextElementSibling?.textContent).toBe("12"); });
    expect(screen.getByRole("button", { name: "7 days" }).getAttribute("aria-pressed")).toBe("true");
    expect(actions.filter((a) => a.method === "getActivity").map((a) => (a.req as { window: string }).window)).toEqual(["30d", "7d"]);
  });

  it("says when nobody played", async () => {
    renderShell({ me: owner, path: "/activity" });
    expect(await screen.findByText("No one played in this window.")).toBeTruthy();
    expect(screen.getByText("No regular has gone quiet.")).toBeTruthy();
    expect(screen.getByText("Median session").nextElementSibling?.textContent).toBe("–");
  });

  it("says why it was refused", async () => {
    renderShell({ me: owner, path: "/activity", activity: new ConnectError("needs stats:read everywhere", Code.PermissionDenied) });
    expect(await screen.findByText(/needs stats:read across the whole hub/)).toBeTruthy();
  });

  it("is in the nav for stats:read everywhere only", async () => {
    renderShell({ me: member({ capabilities: ["servers:read", "stats:read"] }) });
    expect(await screen.findByRole("link", { name: "Activity" })).toBeTruthy();
    renderShell({ me: member({ id: "u2", capabilities: ["servers:read"], grants: [{ role: "moderator", gameId: "wardogs" }] }) });
    await waitFor(() => { expect(screen.getAllByRole("link", { name: "Fleet" })).toHaveLength(2); });
    expect(screen.getAllByRole("link", { name: "Activity" })).toHaveLength(1);
  });

  it("formats and ranks", () => {
    expect([length(0), length(59 * 60), length(3600), length(3900)]).toEqual(["0 min", "59 min", "1 h", "1 h 5 min"]);
    expect([shade(0, 6), shade(0.4, 6.2), shade(4, 6.2), shade(6.2, 6.2)]).toEqual([0, 1, 3, 4]);
    expect(busiest(month.hours.map((h) => ({ ...h, $typeName: "gravel.hub.v1.ActivityHour" as const })), 2).map((h) => [h.day, h.hour])).toEqual([[3, 20], [6, 21]]);
  });
});
