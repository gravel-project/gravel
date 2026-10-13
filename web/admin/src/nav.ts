// The dashboard's pages, each behind the capability its procedures check (ADR-0027). A member sees
// only what they can use; the hub refuses the rest anyway, the nav only saves them the refusal.
import type { User } from "./gen/gravel/hub/v1/identity_pb";

export interface NavItem {
  // to is the route under /admin.
  to: string;
  label: string;
  // capability is what the page needs, "" for every member.
  capability: string;
  // roles are the roles whose game-scoped grant brings the capability for some game (ADR-0027 §4),
  // so a member staffing one game sees the page too.
  roles?: readonly string[];
}

export const pages: readonly NavItem[] = [{ to: "/", label: "Overview", capability: "" }];

// holds reports whether a member holds a capability instance-wide or, through a game-scoped grant
// of one of roles, for some game: what the member pages' holdsSomewhere checks.
export function holds(me: User, capability: string, roles: readonly string[] = []): boolean {
  if (capability === "" || me.owner || me.capabilities.includes(capability)) {
    return true;
  }
  return me.grants.some((g) => g.gameId !== "" && roles.includes(g.role));
}

export function visible(me: User, items: readonly NavItem[] = pages): NavItem[] {
  return items.filter((p) => holds(me, p.capability, p.roles));
}
