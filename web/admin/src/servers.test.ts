import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";

import { type User, UserSchema } from "./gen/gravel/hub/v1/identity_pb";
import { actions, holdsFor, manages } from "./servers";

const user = (fields: Parameters<typeof create<typeof UserSchema>>[1] = {}): User => create(UserSchema, { id: "x", ...fields });

const owner = user({ owner: true });
const operator = user({ capabilities: ["servers:read", "servers:operate", "servers:lease"], roles: ["operator"] });
const moderator = user({ capabilities: ["servers:read", "servers:moderate", "servers:lease"], roles: ["moderator"] });
const cs2Admin = user({ grants: [{ role: "admin", gameId: "cs2" }] });
const cs2Operator = user({ grants: [{ role: "operator", gameId: "cs2" }] });
const member = user({ capabilities: ["servers:read"] });

describe("what a viewer may do to a server (the visibility matrix)", () => {
  it("offers start and park by host state, and provisioning to whoever provisions", () => {
    expect(actions(owner, "cs2", "running", "")).toEqual(["park", "reboot", "expect", "resize"]);
    expect(actions(owner, "cs2", "running", "c-4")).toEqual(["park", "reboot", "expect", "resize", "unpin"]);
    expect(actions(owner, "cs2", "parked", "")).toEqual(["start", "snapshot", "destroy"]);
    expect(actions(owner, "cs2", "absent", "")).toEqual(["start"]);
  });

  it("gives an operator the lifecycle and nothing that costs or removes", () => {
    expect(actions(operator, "cs2", "running", "c-4")).toEqual(["park", "reboot", "expect"]);
    expect(actions(operator, "cs2", "parked", "")).toEqual(["start"]);
  });

  it("gives a moderator and a member no buttons", () => {
    for (const who of [moderator, member]) {
      expect(actions(who, "cs2", "running", "c-4")).toEqual([]);
      expect(actions(who, "cs2", "parked", "")).toEqual([]);
    }
  });

  it("scopes a game's grant to that game's servers", () => {
    expect(actions(cs2Admin, "cs2", "parked", "")).toEqual(["start", "snapshot", "destroy"]);
    expect(actions(cs2Admin, "wardogs", "parked", "")).toEqual([]);
    expect(actions(cs2Operator, "cs2", "running", "")).toEqual(["park", "reboot", "expect"]);
    expect(holdsFor(cs2Operator, "servers:provision", "cs2")).toBe(false);
  });

  it("offers nothing while a host moves between states", () => {
    for (const state of ["creating", "booting", "starting", "parking", "resizing", "rebooting", "destroying"]) {
      expect(actions(owner, "cs2", state, "")).toEqual([]);
    }
  });

  it("links Manage for whoever may act on the game's servers", () => {
    expect([owner, operator, moderator, cs2Admin].map((u) => manages(u, "cs2"))).toEqual([true, true, true, true]);
    expect(manages(member, "cs2")).toBe(false);
    expect(manages(cs2Admin, "wardogs")).toBe(false);
  });
});
