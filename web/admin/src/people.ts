// Who may do what on People (gravel#203): the rules the hub applies (internal/identity/roles.go,
// ADR-0027 §5), mirrored so the page offers only what will be allowed. The hub refuses the rest
// anyway; the page only saves the viewer the refusal.
import type { RoleGrant, User } from "./gen/gravel/hub/v1/identity_pb";

// roles are the roles a grant carries, highest first.
export const roles = ["admin", "operator", "moderator"] as const;

// grantsFor reports whether me holds roles:grant for a scope: "" is instance-wide, else one game.
// An instance-wide grant needs it everywhere; a game's, everywhere or through an admin grant for
// that game (roles:grant is in the admin bundle and is scopable).
export function grantsFor(me: User, game: string): boolean {
  if (me.owner || me.capabilities.includes("roles:grant")) {
    return true;
  }
  return game !== "" && me.grants.some((g) => g.gameId === game && g.role === "admin");
}

// mayChange reports whether me may grant or revoke role, for game, on member: never their own,
// an admin grant only as the owner, and roles:grant for the scope.
export function mayChange(me: User, member: User, role: string, game: string): boolean {
  if (member.id === me.id) {
    return false;
  }
  if (role === "admin" && !me.owner) {
    return false;
  }
  return grantsFor(me, game);
}

// grantable is what the grant form offers me for member: the roles and the scopes ("" for every
// game, then each game id) it may grant, both empty when it may grant nothing.
export function grantable(me: User, member: User, games: readonly string[]): { roles: string[]; scopes: string[] } {
  if (member.id === me.id) {
    return { roles: [], scopes: [] };
  }
  const scopes = ["", ...games].filter((g) => grantsFor(me, g));
  if (scopes.length === 0) {
    return { roles: [], scopes: [] };
  }
  return { roles: roles.filter((r) => r !== "admin" || me.owner), scopes };
}

// manages reports whether me makes local accounts, their links, and ends members' sessions.
export function manages(me: User): boolean {
  return me.owner || me.capabilities.includes("members:manage");
}

// readsMembers reports whether me may list the members (ListUsers needs identity:read).
export function readsMembers(me: User): boolean {
  return me.owner || me.capabilities.includes("identity:read");
}

// grantLabel is how the page names a grant: "moderator", or "operator · Counter-Strike 2".
export function grantLabel(g: Pick<RoleGrant, "role" | "gameId">, gameNames: ReadonlyMap<string, string>): string {
  if (g.gameId === "") {
    return g.role;
  }
  return g.role + " · " + (gameNames.get(g.gameId) ?? g.gameId);
}

// localUsername is the member's local account, if they have one: what a reset link is for.
export function localUsername(member: User): string | undefined {
  return member.identities.find((i) => i.provider === "local")?.subject;
}
