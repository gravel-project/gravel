import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";

import { RoleGrantSchema, UserSchema } from "./gen/gravel/hub/v1/identity_pb";
import { holds, type NavItem, visible } from "./nav";

const items: NavItem[] = [
  { to: "/", label: "Overview", capability: "" },
  { to: "/people", label: "People", capability: "members:manage" },
  { to: "/servers", label: "Servers", capability: "servers:operate", roles: ["admin", "operator"] },
];

describe("the nav", () => {
  it("shows a member only the pages every member may use", () => {
    expect(visible(create(UserSchema, {}), items).map((i) => i.label)).toEqual(["Overview"]);
  });

  it("shows a page for a capability held everywhere", () => {
    const me = create(UserSchema, { capabilities: ["members:manage"] });
    expect(visible(me, items).map((i) => i.label)).toEqual(["Overview", "People"]);
  });

  it("shows a page a game-scoped grant brings, and only for the roles named", () => {
    const operator = create(UserSchema, { grants: [create(RoleGrantSchema, { role: "operator", gameId: "cs2" })] });
    expect(holds(operator, "servers:operate", ["admin", "operator"])).toBe(true);
    const moderator = create(UserSchema, { grants: [create(RoleGrantSchema, { role: "moderator", gameId: "cs2" })] });
    expect(holds(moderator, "servers:operate", ["admin", "operator"])).toBe(false);
    // An instance-wide grant counts through capabilities, never through grants.
    const wide = create(UserSchema, { grants: [create(RoleGrantSchema, { role: "operator" })] });
    expect(holds(wide, "servers:operate", ["operator"])).toBe(false);
  });

  it("shows the owner everything", () => {
    expect(visible(create(UserSchema, { owner: true }), items)).toHaveLength(3);
  });
});
