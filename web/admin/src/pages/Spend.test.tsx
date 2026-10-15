import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { axe } from "vitest-axe";

import { member, renderShell } from "../test/rig";
import { bar, months, usd } from "./Spend";

const owner = member({ id: "jo", displayName: "Jo", owner: true });

const servers = [{ id: "htg-cs2-1", name: "HTG CS2 #1", gameId: "cs2" }];

const billed = {
  month: "2026-10",
  timezone: "America/Chicago",
  current: true,
  available: true,
  billed: true,
  billedUsd: 49.4,
  billedAsOf: timestampFromDate(new Date("2026-10-15T11:00:00Z")),
  estimatedUsd: 30,
  projectedUsd: 107.47,
  projectedFrom: "billed",
  locations: [
    { id: "do-sfo3", provider: "digitalocean", billed: true },
    { id: "do-nyc3", provider: "digitalocean", billed: false, reason: "the location's token may not read the bill (it needs billing:read)" },
  ],
  days: [
    { date: "2026-10-13", billedUsd: 3.8, hasBilled: true, estimatedUsd: 2 },
    { date: "2026-10-14", billedUsd: 3.8, hasBilled: true, estimatedUsd: 2 },
    { date: "2026-10-15", estimatedUsd: 1 },
  ],
  causes: [
    { serverId: "htg-cs2-1", kinds: { compute: 32.5, storage: 5.5 }, billedUsd: 38, estimatedUsd: 30 },
    { name: "htg-hub-1", kinds: { compute: 11.4 }, billedUsd: 11.4 },
  ],
};

describe("cloud spend", () => {
  it("shows the bill, the projection, the causes and the days", async () => {
    const { container } = renderShell({ me: owner, path: "/spend", servers, spend: { "": billed } });
    const tile = async (label: string) => (await screen.findByText(label, { selector: "dt" })).nextElementSibling?.textContent;
    expect(await tile("Billed so far")).toBe("$49.40");
    expect(await tile("Projected for the month")).toBe("$107.47");
    expect(await tile("Estimated compute")).toBe("$30.00");
    expect(await tile("Bill as of")).toBe("15 Oct, 06:00");
    expect(screen.getByText(/do-nyc3: the location's token may not read the bill/)).toBeTruthy();
    const causes = screen.getByRole("region", { name: "What it’s for, the table" });
    const cs2 = (await within(causes).findByRole("link", { name: "HTG CS2 #1" })).closest("tr") as HTMLElement;
    expect(within(cs2).getAllByRole("cell").map((c) => c.textContent)).toEqual(["HTG CS2 #1", "$32.50", "$5.50", "–", "–", "$38.00", "$30.00"]);
    expect(within(causes).getByText("htg-hub-1")).toBeTruthy();
    const days = screen.getByRole("region", { name: "Day by day, the table" });
    const rows = within(days).getAllByRole("row");
    // Newest first; the 15th has no bill yet.
    expect(within(rows[1] as HTMLElement).getAllByRole("cell").map((c) => c.textContent).slice(0, 3)).toEqual(["2026-10-15", "–", "$1.00"]);
    expect(await axe(container)).toHaveNoViolations();
  });

  it("falls back to the estimate, and says what it leaves out", async () => {
    const est = { ...billed, billed: false, billedUsd: 0, billedAsOf: undefined, projectedFrom: "estimated", projectedUsd: 63,
      causes: [{ serverId: "htg-cs2-1", estimatedUsd: 30 }] };
    renderShell({ me: owner, path: "/spend", servers, spend: { "": est } });
    expect((await screen.findByText("Estimated so far", { selector: "dt" })).nextElementSibling?.textContent).toBe("$30.00");
    expect(screen.getByText(/A floor: disks, addresses, snapshots/)).toBeTruthy();
    const causes = screen.getByRole("region", { name: "What it’s for, the table" });
    expect(within(causes).getAllByRole("columnheader").map((h) => h.textContent)).toEqual(["Server or resource", "Estimated compute"]);
  });

  it("asks for another month, and says when the hub has no cloud", async () => {
    const m = months(new Date())[1] ?? "";
    renderShell({ me: owner, path: "/spend", spend: { "": { available: false }, [m]: { ...billed, month: m, current: false } } });
    expect(await screen.findByText(/This hub has no cloud location/)).toBeTruthy();
    const pick = screen.getByRole("combobox", { name: "Month" });
    fireEvent.change(pick, { target: { value: m } });
    await waitFor(() => { expect(screen.getByText("The month's total", { selector: "dt" })).toBeTruthy(); });
  });

  it("says spend needs servers:provision", async () => {
    renderShell({ me: owner, path: "/spend", spend: new ConnectError("no", Code.PermissionDenied) });
    expect(await screen.findByText("Cloud spend needs servers:provision across the whole hub.")).toBeTruthy();
  });
});

describe("spend helpers", () => {
  it("formats dollars, names months and sizes bars", () => {
    expect(usd(1234.5)).toBe("$1,234.50");
    expect(months(new Date("2026-01-15T12:00:00Z")).slice(0, 3)).toEqual(["2026-01", "2025-12", "2025-11"]);
    expect([bar(0, 10), bar(0.1, 10), bar(5, 10), bar(10, 10), bar(3, 0)]).toEqual(["w-0", "w-[10%]", "w-[50%]", "w-full", "w-0"]);
  });
});
