import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { axe } from "vitest-axe";

import { member, renderShell } from "../test/rig";
import { columnsOf, statValue } from "./Stats";

const owner = member({ id: "jo", displayName: "Jo", owner: true });

const cs2Stats = [
  { name: "assists", label: "Assists", type: "int", aggregate: "sum" },
  { name: "damage", label: "Damage", type: "int", unit: "HP", aggregate: "sum" },
  { name: "mvps", label: "MVPs", type: "int", aggregate: "sum" },
];

const games = [
  { id: "cs2", name: "Counter-Strike 2", statExtensions: cs2Stats },
  { id: "wardogs", name: "War Dogs", layout: { board: ["kills", "deaths", "kd"] } },
];

const servers = [
  { id: "htg-cs2-1", name: "HTG CS2 #1", gameId: "cs2" },
  { id: "htg-wardogs-1", name: "HTG WARDOGS #1", gameId: "wardogs" },
];

const boards = {
  cs2: {
    statExtensions: cs2Stats,
    entries: [
      { rank: 1, name: "Ana", userId: "u-ana", kills: 61, deaths: 40, kd: 1.525, secondsOn: 7200, matches: 3, extensions: { assists: 12, damage: 9240, mvps: 0 } },
      { rank: 2, name: "Brave Falcon", pseudonymous: true, kills: 20, deaths: 22, kd: 0.909, secondsOn: 3600, matches: 1 },
    ],
  },
  wardogs: { entries: [{ rank: 1, name: "Cy", kills: 30, deaths: 10, kd: 3, secondsOn: 600, matches: 2 }] },
};

const results = [
  { id: "r1", serverId: "htg-cs2-1", team1Name: "Alpha", team2Name: "Bravo", team1Score: 1, team2Score: 0, endedAt: timestampFromDate(new Date("2026-10-14T02:00:00Z")) },
  { id: "r2", serverId: "htg-wardogs-1", team1Name: "X", team2Name: "Y" },
];

describe("game stats", () => {
  it("shows a game's own stats beside the common ones, from the hub's columns", async () => {
    const { container } = renderShell({ me: owner, path: "/stats", games, servers, boards, results, settings: { version: 1, stats: { public: true } } });
    const table = await screen.findByRole("table");
    const heads = within(table).getAllByRole("columnheader").map((h) => h.textContent);
    expect(heads).toEqual(["Rank", "Player", "Kills", "Deaths", "K/D", "Time on", "Matches", "Assists", "Damage (HP)", "MVPs"]);
    const ana = within(table).getByRole("link", { name: "Ana" }).closest("tr") as HTMLElement;
    expect(within(ana).getAllByRole("cell").map((c) => c.textContent)).toEqual(["1", "Ana", "61", "40", "1.52", "2.0 h", "3", "12", "9,240", "0"]);
    // Brave Falcon has no result: a dash, read as "not reported", never a 0.
    const falcon = within(table).getByText("Brave Falcon").closest("tr") as HTMLElement;
    expect(within(falcon).getAllByText("not reported")).toHaveLength(3);
    expect(screen.getByText(/Public: anyone can read the boards/)).toBeTruthy();
    // The latest results are the game's own.
    await waitFor(() => { expect(screen.getByRole("link", { name: "Alpha 1–0 Bravo" }).getAttribute("href")).toBe("/results/r1"); });
    expect(screen.queryByText(/X 0–0 Y/)).toBeNull();
    expect(await axe(container)).toHaveNoViolations();
  });

  it("shows the common stats only for a game that declares none, by its layout", async () => {
    renderShell({ me: owner, path: "/stats", games, servers, boards, results });
    fireEvent.change(await screen.findByRole("combobox", { name: "Game" }), { target: { value: "wardogs" } });
    const table = await screen.findByRole("table", {}, { timeout: 2000 });
    await waitFor(() => { expect(within(table).getByText("Cy")).toBeTruthy(); });
    expect(within(table).getAllByRole("columnheader").map((h) => h.textContent)).toEqual(["Rank", "Player", "Kills", "Deaths", "K/D"]);
    expect(screen.getByText(/Private: the boards need stats:read/)).toBeTruthy();
  });

  it("says a private board needs stats:read", async () => {
    renderShell({ me: member({ id: "m" }), path: "/stats", games, boards: new ConnectError("no", Code.PermissionDenied) });
    expect(await screen.findByText("The boards are private: they need stats:read.")).toBeTruthy();
  });
});

describe("stat values", () => {
  it("formats by type and aggregate, and absent is undefined", () => {
    const e = (over: object) => ({ name: "x", label: "X", type: "int", unit: "", aggregate: "sum", ...over }) as Parameters<typeof statValue>[0];
    expect(statValue(e({}), { x: 1234 })).toBe("1,234");
    expect(statValue(e({ aggregate: "avg" }), { x: 7.25 })).toBe("7.3");
    expect(statValue(e({ type: "float" }), { x: 2 })).toBe("2.0");
    expect(statValue(e({}), { x: 0 })).toBe("0");
    expect(statValue(e({}), {})).toBeUndefined();
  });
  it("takes a game's layout, or every common stat", () => {
    expect(columnsOf(undefined)).toEqual(["kills", "deaths", "kd", "time", "matches"]);
  });
});
