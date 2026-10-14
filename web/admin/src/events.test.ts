import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";

import { filtered, filtersFrom, newer, noFilters, older, request, search } from "./events";
import { type Event, EventSchema } from "./gen/gravel/hub/v1/organization_pb";

const ev = (id: number): Event => create(EventSchema, { id: BigInt(id), kind: "server.down" });
const ids = (evs: Event[]) => evs.map((e) => Number(e.id));

describe("the cursor merge", () => {
  it("puts what came after the cursor on top, newest first, once", () => {
    const feed = [ev(5), ev(4), ev(3)];
    // A poll answers oldest first, and may repeat what the feed has (a retried poll).
    expect(ids(newer(feed, [ev(4), ev(5), ev(6), ev(7)]))).toEqual([7, 6, 5, 4, 3]);
    expect(ids(newer(feed, []))).toEqual([5, 4, 3]);
    expect(ids(newer([], [ev(1), ev(2)]))).toEqual([2, 1]);
  });

  it("puts an older page under the feed, once", () => {
    const feed = [ev(9), ev(8), ev(7)];
    expect(ids(older(feed, [ev(8), ev(7), ev(6), ev(5)]))).toEqual([9, 8, 7, 6, 5]);
    expect(ids(older([], [ev(3), ev(2)]))).toEqual([3, 2]);
  });

  it("has no gaps or duplicates however polls and pages interleave", () => {
    // The log is 1..60; the first read took 41..60, polls bring 61..70 in pieces, older pages
    // bring 1..40 in pieces, some of each repeated.
    let feed = Array.from({ length: 20 }, (_, i) => ev(60 - i));
    feed = newer(feed, [ev(61), ev(62)]);
    feed = older(feed, Array.from({ length: 20 }, (_, i) => ev(42 - i))); // overlaps 41, 42
    feed = newer(feed, [ev(62), ev(63), ev(64), ev(65), ev(66), ev(67), ev(68), ev(69), ev(70)]);
    feed = older(feed, Array.from({ length: 22 }, (_, i) => ev(22 - i))); // before 23: 22..1
    expect(ids(feed)).toEqual(Array.from({ length: 70 }, (_, i) => 70 - i));
  });
});

describe("the filters", () => {
  it("round-trip through the address, empty ones left out", () => {
    const f = { ...noFilters, kind: "server", severity: "warning", server: "htg-wardogs-1", actorUser: "u1" };
    expect(search(f)).toBe("?kind=server&severity=warning&server=htg-wardogs-1&actor_user=u1");
    expect(filtersFrom(new URLSearchParams(search(f)))).toEqual(f);
    expect(search(noFilters)).toBe("");
    expect(filtered(noFilters)).toBe(false);
    expect(filtered({ ...noFilters, game: "cs2" })).toBe(true);
  });

  it("compose into one request", () => {
    const f = filtersFrom(new URLSearchParams("kind=host&severity=info&game=cs2&actor_app=a1&server=cs2-1"));
    expect(request(f)).toEqual({ kinds: ["host"], minSeverity: "", serverId: "cs2-1", gameId: "cs2", actorUserId: "", actorAppId: "a1" });
    expect(request(noFilters).kinds).toEqual([]);
  });
});
