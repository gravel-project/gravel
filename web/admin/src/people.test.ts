import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";

import { type User, UserSchema } from "./gen/gravel/hub/v1/identity_pb";
import { grantable, grantLabel, localUsername, manages, mayChange, readsMembers } from "./people";

const user = (fields: Parameters<typeof create<typeof UserSchema>>[1] = {}): User => create(UserSchema, { id: "x", ...fields });

const owner = user({ id: "owner", owner: true });
const admin = user({ id: "admin", capabilities: ["roles:grant", "identity:read", "members:manage"], roles: ["admin"] });
const cs2Admin = user({ id: "cs2admin", grants: [{ role: "admin", gameId: "cs2" }] });
const cs2Operator = user({ id: "cs2op", grants: [{ role: "operator", gameId: "cs2" }] });
const sam = user({ id: "sam" });

describe("the granter rules (ADR-0027 §5)", () => {
  it("lets the owner grant anything, anywhere, to anyone but themselves", () => {
    expect(grantable(owner, sam, ["cs2", "wardogs"])).toEqual({ roles: ["admin", "operator", "moderator"], scopes: ["", "cs2", "wardogs"] });
    expect(grantable(owner, owner, ["cs2"])).toEqual({ roles: [], scopes: [] });
  });

  it("keeps admin grants to the owner", () => {
    expect(grantable(admin, sam, ["cs2"])).toEqual({ roles: ["operator", "moderator"], scopes: ["", "cs2"] });
    expect(mayChange(admin, sam, "admin", "")).toBe(false);
    expect(mayChange(admin, sam, "admin", "cs2")).toBe(false);
    expect(mayChange(owner, sam, "admin", "")).toBe(true);
  });

  it("lets a game's admin grant within that game only", () => {
    expect(grantable(cs2Admin, sam, ["cs2", "wardogs"])).toEqual({ roles: ["operator", "moderator"], scopes: ["cs2"] });
    expect(mayChange(cs2Admin, sam, "moderator", "cs2")).toBe(true);
    expect(mayChange(cs2Admin, sam, "moderator", "wardogs")).toBe(false);
    expect(mayChange(cs2Admin, sam, "moderator", "")).toBe(false);
  });

  it("gives a role without roles:grant nothing to grant", () => {
    expect(grantable(cs2Operator, sam, ["cs2"])).toEqual({ roles: [], scopes: [] });
    expect(mayChange(cs2Operator, sam, "moderator", "cs2")).toBe(false);
  });

  it("never lets anyone change their own grants", () => {
    expect(mayChange(owner, owner, "moderator", "")).toBe(false);
    expect(mayChange(admin, admin, "operator", "cs2")).toBe(false);
  });
});

describe("what else the page offers", () => {
  it("manages members with members:manage, and reads them with identity:read", () => {
    expect([manages(owner), manages(admin), manages(cs2Admin)]).toEqual([true, true, false]);
    expect([readsMembers(owner), readsMembers(admin), readsMembers(cs2Admin)]).toEqual([true, true, false]);
  });

  it("names grants by the game's name, and finds a local account", () => {
    const names = new Map([["cs2", "Counter-Strike 2"]]);
    expect(grantLabel({ role: "operator", gameId: "cs2" }, names)).toBe("operator · Counter-Strike 2");
    expect(grantLabel({ role: "operator", gameId: "pubg" }, names)).toBe("operator · pubg");
    expect(grantLabel({ role: "admin", gameId: "" }, names)).toBe("admin");
    expect(localUsername(user({ identities: [{ provider: "discord", subject: "1" }, { provider: "local", subject: "sam" }] }))).toBe("sam");
    expect(localUsername(user({ identities: [{ provider: "discord", subject: "1" }] }))).toBeUndefined();
  });
});
