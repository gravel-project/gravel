// What a viewer may do to a server (gravel#202): the capabilities the hub checks, for the server's
// game (ADR-0027 §4), and what the host's state allows. The page offers only these; the hub decides
// every call anyway and its refusal is shown in its own words.
import type { User } from "./gen/gravel/hub/v1/identity_pb";

// bundles are the capabilities each grantable role brings (internal/authz bundles); an admin holds
// every capability but org:transfer.
const bundles: Record<string, readonly string[]> = {
  operator: ["servers:read", "servers:operate", "servers:configure", "servers:lease", "events:read", "stats:read", "metrics:read"],
  moderator: ["servers:read", "servers:moderate", "servers:lease", "identity:read", "events:read", "stats:read"],
};

// holdsFor reports whether me holds capability for a server of game: everywhere (the owner, or
// an instance-wide grant), or through a grant for that game whose role brings it.
export function holdsFor(me: User, capability: string, game: string): boolean {
  if (me.owner || me.capabilities.includes(capability)) {
    return true;
  }
  return me.grants.some((g) => g.gameId !== "" && g.gameId === game && (g.role === "admin" || (bundles[g.role] ?? []).includes(capability)));
}

export type Action = "start" | "park" | "reboot" | "expect" | "resize" | "unpin" | "snapshot" | "destroy";

// provisioning are the actions that change spend or remove data: servers:provision, behind a
// confirmation the viewer types (the server's id).
export const provisioning: readonly Action[] = ["resize", "unpin", "snapshot", "destroy"];

// actions is what the page offers me on a host in state, with its pinned size: start and park,
// reboot and the expectation with servers:operate; resize and unpin, snapshot and destroy with
// servers:provision. A host between states offers nothing: the hub is carrying out a request.
export function actions(me: User, game: string, state: string, pinned: string): Action[] {
  const operate = holdsFor(me, "servers:operate", game);
  const provision = holdsFor(me, "servers:provision", game);
  const out: Action[] = [];
  switch (state) {
    case "running":
      if (operate) {
        out.push("park", "reboot", "expect");
      }
      if (provision) {
        out.push("resize");
        if (pinned !== "") {
          out.push("unpin");
        }
      }
      break;
    case "parked":
      if (operate) {
        out.push("start");
      }
      if (provision) {
        out.push("snapshot", "destroy");
      }
      break;
    case "absent":
      if (operate) {
        out.push("start");
      }
      break;
  }
  return out;
}

// manages reports whether me may act on a server of game at all: the Fleet's Manage link.
export function manages(me: User, game: string): boolean {
  return ["servers:operate", "servers:provision", "servers:lease"].some((c) => holdsFor(me, c, game));
}
