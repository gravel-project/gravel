// The dashboard's pages, each behind the capability its procedures check (ADR-0027). A member sees
// only what they can use; the hub refuses the rest anyway, the nav only saves them the refusal.
import type { User } from "./gen/gravel/hub/v1/identity_pb";
import type { Organization } from "./gen/gravel/hub/v1/organization_pb";

export interface NavItem {
  // to is the route under /admin.
  to: string;
  label: string;
  // capability is what the page needs, "" for every member.
  capability: string;
  // roles are the roles whose game-scoped grant brings the capability for some game (ADR-0027 §4),
  // so a member staffing one game sees the page too.
  roles?: readonly string[];
  // nominee shows the page to the member the owner nominated to take the hub over, whatever
  // they hold: it is where they accept.
  nominee?: boolean;
}

export const pages: readonly NavItem[] = [
  { to: "/", label: "Fleet", capability: "servers:read", roles: ["admin", "operator", "moderator"] },
  { to: "/events", label: "Events", capability: "events:read", roles: ["admin", "operator", "moderator"] },
  { to: "/people", label: "People", capability: "identity:read", nominee: true },
  { to: "/metrics", label: "Metrics", capability: "metrics:read" },
  { to: "/stats", label: "Stats", capability: "stats:read" },
  { to: "/activity", label: "Activity", capability: "stats:read" },
  { to: "/settings", label: "Settings", capability: "settings:write" },
  { to: "/apps", label: "Apps", capability: "apps:manage" },
  { to: "/access", label: "Your access", capability: "" },
];

// holds reports whether a member holds a capability instance-wide or, through a game-scoped grant
// of one of roles, for some game: what the member pages' holdsSomewhere checks.
export function holds(me: User, capability: string, roles: readonly string[] = []): boolean {
  if (capability === "" || me.owner || me.capabilities.includes(capability)) {
    return true;
  }
  return me.grants.some((g) => g.gameId !== "" && roles.includes(g.role));
}

export function visible(me: User, items: readonly NavItem[] = pages, org?: Organization): NavItem[] {
  const nominated = org !== undefined && org.pendingOwnerUserId !== "" && org.pendingOwnerUserId === me.id;
  return items.filter((p) => holds(me, p.capability, p.roles) || (p.nominee === true && nominated));
}
