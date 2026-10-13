import { describe, expect, it } from "vitest";

import { ago, duration, health, stale, staleAfter, trouble } from "./fleet";

const t0 = new Date("2026-10-13T22:00:00Z");
const after = (s: number) => new Date(t0.getTime() + s * 1000);

describe("the fleet's words", () => {
  it("says how long ago, to the unit that matters", () => {
    expect([0, 0.5, 8, 59, 60, 3599, 3600, 86399, 86400 * 4].map((s) => ago(t0, after(s)))).toEqual([
      "just now",
      "just now",
      "8 s ago",
      "59 s ago",
      "1 min ago",
      "59 min ago",
      "1 h ago",
      "23 h ago",
      "4 d ago",
    ]);
    expect(ago(after(5), t0)).toBe("just now"); // a moment after now, from a clock that runs ahead
    expect(duration(t0, after(180))).toBe("3 min");
  });

  it("knows every health and calls the rest unknown", () => {
    expect(["up", "degraded", "down", "idle", "unknown", "exploded"].map(health)).toEqual(["up", "degraded", "down", "idle", "unknown", "unknown"]);
    expect(["down", "degraded", "unknown", "up", "idle"].map(trouble)).toEqual([true, true, false, false, false]);
  });

  it("calls a summary stale after three polls without an answer, or at once after a failed one", () => {
    expect(stale(0, staleAfter, false)).toBe(false);
    expect(stale(0, staleAfter + 1, false)).toBe(true);
    expect(stale(0, 1, true)).toBe(true);
  });
});
