import { describe, expect, it } from "vitest";

import { gapWords, span } from "./gaps";

const none = { events: 0, kills: 0, deaths: 0, players: 0 };

describe("gap words", () => {
  it("says each reason the hub writes", () => {
    expect(gapWords({ ...none, reason: "hub_down" })).toBe("The hub was down, and the events the server sent meanwhile were lost.");
    expect(gapWords({ ...none, reason: "silence" })).toBe("The feed went quiet during a full match.");
    expect(gapWords({ ...none, reason: "dropped", events: 7 })).toBe("The server gave up on 7 events it couldn't send.");
    expect(gapWords({ ...none, reason: "dropped", events: 1 })).toBe("The server gave up on 1 event it couldn't send.");
    expect(gapWords({ reason: "reconcile", events: 0, kills: 3, deaths: 1, players: 2 })).toBe("The feed was short 3 kills and 1 death for 2 players in a match.");
    expect(gapWords({ reason: "reconcile", events: 0, kills: 1, deaths: 0, players: 1 })).toBe("The feed was short 1 kill and 0 deaths for 1 player in a match.");
  });

  it("says only that data is missing for a reason it doesn't know", () => {
    expect(gapWords({ ...none, reason: "later" })).toBe("Some of the feed is missing.");
    expect(gapWords({ ...none, reason: "" })).toBe("Some of the feed is missing.");
  });
});

describe("gap span", () => {
  const at = new Date("2026-10-19T12:00:00Z");
  it("reads minutes, then hours and minutes", () => {
    expect(span(at, new Date(at.getTime() + 9 * 60_000))).toBe("9m");
    expect(span(at, new Date(at.getTime() + 95 * 60_000))).toBe("1h 35m");
    expect(span(at, new Date(at.getTime() + 3 * 3_600_000 + 5 * 60_000))).toBe("3h 05m");
  });
  it("never goes below nothing", () => {
    expect(span(at, at)).toBe("0m");
    expect(span(at, new Date(at.getTime() - 60_000))).toBe("0m");
  });
});
