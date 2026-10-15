import { Code, ConnectError } from "@connectrpc/connect";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { axe } from "vitest-axe";

import { format, statOf, stepFor } from "../metrics";
import { member, renderShell } from "../test/rig";

const operator = member({ id: "op", displayName: "Op", roles: ["operator"], capabilities: ["servers:read", "metrics:read"] });

const hub = {
  uid: "gravel-hub",
  title: "gravel hub",
  rows: [
    {
      title: "Overview",
      panels: [
        { id: "r0p0", title: "Up", kind: "stat", width: 4, mappings: [{ value: "1", text: "Up", color: "green" }, { value: "0", text: "Down", color: "red" }] },
        { id: "r0p1", title: "Backup age", kind: "stat", unit: "s", width: 4, thresholds: [{ value: 0, color: "green" }, { value: 93_600, color: "red" }] },
      ],
    },
    {
      title: "API",
      panels: [
        { id: "r1p0", title: "Calls by procedure", kind: "timeseries", unit: "reqps", width: 8 },
        { id: "r1p1", title: "Errors by procedure and code", kind: "timeseries", unit: "reqps", width: 8, optional: "no call has failed in the range" },
        { id: "r1p2", title: "p95 latency", kind: "timeseries", unit: "s", width: 8 },
      ],
    },
  ],
};
const bot = { uid: "gravel-bot", title: "gravel bot", rows: [{ title: "Overview", panels: [{ id: "r0p0", title: "Bot up", kind: "stat", width: 6, mappings: [{ value: "1", text: "Up", color: "green" }] }] }] };

const t0 = 1_792_058_400_000n;
const panelSeries = {
  r0p0: [{ name: "hub:9090", times: [t0], values: [1] }],
  r0p1: [{ name: "hub:9090", times: [t0], values: [100_000] }],
  r1p0: [
    { name: "/gravel.hub.v1.ServerService/ListServers", times: [t0, t0 + 60_000n], values: [1.5, 2.25] },
    { name: "/gravel.hub.v1.IdentityService/GetMe", times: [t0, t0 + 60_000n], values: [0.4, 0.5] },
  ],
  r1p2: new ConnectError("the metrics store did not answer in time", Code.Unavailable),
};

describe("metrics", () => {
  it("draws the hub's panels from the store, through the hub", async () => {
    const { container, queries } = renderShell({ me: operator, path: "/metrics", dashboards: [hub, bot], panelSeries });
    expect(await screen.findByRole("heading", { name: "API" })).toBeTruthy();
    const up = (await screen.findByText("Up", { selector: "figcaption" })).closest("figure") as HTMLElement;
    expect(await within(up).findByText("Up", { selector: "span" })).toHaveProperty("className", expect.stringContaining("border-ok") as unknown);
    const age = screen.getByText("Backup age").closest("figure") as HTMLElement;
    expect((await within(age).findByText("27.8 h")).className).toContain("border-error");
    const calls = screen.getByText("Calls by procedure").closest("figure") as HTMLElement;
    expect(await within(calls).findByText("Latest: /gravel.hub.v1.ServerService/ListServers: 2.25/s; /gravel.hub.v1.IdentityService/GetMe: 0.5/s")).toBeTruthy();
    expect(await screen.findByText("No data: no call has failed in the range.")).toBeTruthy();
    expect(await screen.findByText("the metrics store did not answer in time")).toBeTruthy();
    // Each panel asked by its name, over six hours by default, at about 240 points.
    await waitFor(() => { expect(queries.length).toBe(5); });
    const first = queries[0];
    expect(first?.dashboardUid).toBe("gravel-hub");
    expect(Number((first?.end?.seconds ?? 0n) - (first?.start?.seconds ?? 0n))).toBe(6 * 3600);
    expect(Number(first?.step?.seconds)).toBe(90);
    expect(await axe(container)).toHaveNoViolations();
  });

  it("asks again over another range, and shows the bot's dashboard", async () => {
    const { queries } = renderShell({ me: operator, path: "/metrics", dashboards: [hub, bot], panelSeries });
    await screen.findByRole("heading", { name: "API" });
    await waitFor(() => { expect(queries.length).toBe(5); });
    fireEvent.change(screen.getByLabelText("Range"), { target: { value: "7d" } });
    await waitFor(() => { expect(queries.length).toBe(10); });
    expect(Number((queries[9]?.end?.seconds ?? 0n) - (queries[9]?.start?.seconds ?? 0n))).toBe(7 * 24 * 3600);
    fireEvent.click(screen.getByRole("button", { name: "gravel bot" }));
    expect(await screen.findByText("Bot up")).toBeTruthy();
    await waitFor(() => { expect(queries.at(-1)?.dashboardUid).toBe("gravel-bot"); });
  });

  it("says how to configure a store when there is none", async () => {
    const { container, queries } = renderShell({ me: operator, path: "/metrics", dashboards: [hub], metricsStore: false });
    expect(await screen.findByRole("heading", { name: "No metrics store is configured" })).toBeTruthy();
    expect(screen.getByText(/query_url: http:\/\/127\.0\.0\.1:8428/)).toBeTruthy();
    expect(screen.queryByLabelText("Range")).toBeNull();
    expect(queries).toEqual([]);
    expect(await axe(container)).toHaveNoViolations();
  });

  it("is in the nav for whoever reads metrics", async () => {
    renderShell({ me: operator });
    expect(await screen.findByRole("link", { name: "Metrics" })).toBeTruthy();
    renderShell({ me: member({ capabilities: ["servers:read"], grants: [{ role: "operator", gameId: "cs2" }] }) });
    await waitFor(() => { expect(screen.getAllByRole("link", { name: "Metrics" })).toHaveLength(1); });
  });
});

describe("the metrics helpers", () => {
  it("formats units as people read them", () => {
    expect([format(0.0042, "s"), format(1.5, "s"), format(5400, "s"), format(259_200, "s"), format(3 * 1024 * 1024, "bytes"), format(0.257, "percentunit"), format(2250, "reqps"), format(0.5, "reqps"), format(12, "")]).toEqual([
      "4.2 ms",
      "1.5 s",
      "90 min",
      "3 d",
      "3 MiB",
      "25.7%",
      "2.25 k/s",
      "0.5/s",
      "12",
    ]);
  });

  it("maps a stat to its text and colour, and picks a step", () => {
    const p = { mappings: [{ value: "1", text: "Up", color: "green" }], thresholds: [{ value: 0, color: "green" }, { value: 10, color: "red" }], unit: "" } as unknown as Parameters<typeof statOf>[0];
    expect(statOf(p, 1)).toEqual({ text: "Up", color: "green" });
    expect(statOf(p, 12)).toEqual({ text: "12", color: "red" });
    expect(statOf(p, undefined)).toEqual({ text: "No data", color: "" });
    expect([stepFor(3_600_000), stepFor(30 * 24 * 3_600_000)]).toEqual([15_000, 10_800_000]);
  });
});
