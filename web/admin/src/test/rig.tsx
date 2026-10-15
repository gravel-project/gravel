// The shell over fake services: a router transport (no network) answering as the test says, a
// memory location under /admin, and a browser that records where the shell sent the viewer.
import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";
import { render } from "@testing-library/react";
import { QueryClient } from "@tanstack/react-query";
import { memoryLocation } from "wouter/memory-location";

import { App } from "../App";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";

import { IdentityService, PasswordLinkSchema, RoleGrantSchema, UserSchema } from "../gen/gravel/hub/v1/identity_pb";
import {
  type Event,
  EventKindSchema,
  EventSchema,
  type ListEventsRequest,
  OrganizationSchema,
  OrganizationService,
} from "../gen/gravel/hub/v1/organization_pb";
import { GameSchema, GetFleetSummaryResponseSchema, ServerSchema, ServerService } from "../gen/gravel/hub/v1/servers_pb";
import { ServerHostSchema, ServerHostService } from "../gen/gravel/hub/v1/hosts_pb";
import { ServerLeaseSchema, ServerLeaseService } from "../gen/gravel/hub/v1/leases_pb";
import type { Browser } from "../session";

export interface RigOptions {
  // me is the caller; undefined means no session.
  me?: Member;
  demo?: boolean;
  // path is the route under /admin.
  path?: string;
  // fleet is GetFleetSummary's answer, or the error it throws; an empty fleet when unset.
  fleet?: Fleet | ConnectError;
  // log is the events log ListEvents reads, oldest first; a test may push to it while the page
  // follows. An error is what ListEvents throws.
  log?: LogEvent[] | ConnectError;
  kinds?: MessageInitShape<typeof EventKindSchema>[];
  servers?: MessageInitShape<typeof ServerSchema>[];
  games?: MessageInitShape<typeof GameSchema>[];
  // poll is the dashboard's refetch interval in ms; off (0) unless a test follows.
  poll?: number;
  // users are ListUsers' members, one page of pageSize at a time; the people actions change them.
  users?: Member[];
  pageSize?: number;
  // org adds to GetOrganization's answer: a pending nomination.
  org?: MessageInitShape<typeof OrganizationSchema>;
  // refuse names the procedures that answer permission_denied ("listUsers", "setUserRole", …).
  refuse?: readonly string[];
  // hosts are the cloud-VM servers' hosts by server id; a server without one is not a cloud-VM
  // server (GetServerHost: not_found). The host actions change them.
  hosts?: Record<string, MessageInitShape<typeof ServerHostSchema>>;
  // leases are the live leases by server id.
  leases?: Record<string, MessageInitShape<typeof ServerLeaseSchema>>;
}

// Call is one of the people actions as the page sent it.
export type Call = { method: string; req: unknown };

export type LogEvent = MessageInitShape<typeof EventSchema>;

export type Fleet = MessageInitShape<typeof GetFleetSummaryResponseSchema>;

type Member = MessageInitShape<typeof UserSchema>;

export function member(fields: Member = {}): Member {
  return { id: "u1", displayName: "Jo", ...fields };
}

export function renderShell({ me, demo = false, path = "/", fleet = { totals: {} }, log = [], kinds = [], servers = [], games = [], poll = 0,
  users = [], pageSize = 100, org = {}, refuse = [], hosts = {}, leases = {} }: RigOptions = {}) {
  // calls are the ListEvents requests, in order, for a test to check the cursors.
  const calls: ListEventsRequest[] = [];
  // actions are the people actions, in order; people and the organization are what they changed.
  const actions: Call[] = [];
  const people = users.map((u) => create(UserSchema, u));
  const organization = create(OrganizationSchema, { id: "o1", name: "Hidden Token Gaming", owned: true, demo, ...org });
  const expires = timestampFromDate(new Date("2026-10-15T12:00:00Z"));
  const act = (method: string, req: unknown) => {
    if (refuse.includes(method)) {
      throw new ConnectError("not allowed to change that grant: only the owner changes an admin grant", Code.PermissionDenied);
    }
    actions.push({ method, req });
  };
  const hostRows = new Map(Object.entries(hosts).map(([id, h]) => [id, create(ServerHostSchema, { serverId: id, ...h })]));
  const leaseRows = new Map(Object.entries(leases).map(([id, l]) => [id, create(ServerLeaseSchema, { serverId: id, ...l })]));
  let nextEvent = 100n;
  // hostAct records a host action, applies change to the host and answers it with an event id.
  const hostAct = (method: string, req: { serverId: string }, change: (h: ReturnType<typeof create<typeof ServerHostSchema>>) => void = () => undefined) => {
    act(method, req);
    const h = hostRows.get(req.serverId);
    if (!h) {
      throw new ConnectError("not a cloud-VM server", Code.NotFound);
    }
    change(h);
    nextEvent += 1n;
    return { host: h, auditId: nextEvent - 90n, eventId: nextEvent };
  };
  const find = (id: string) => {
    const u = people.find((p) => p.id === id);
    if (!u) {
      throw new ConnectError("no such member", Code.NotFound);
    }
    return u;
  };
  const transport = createRouterTransport(({ service }) => {
    service(IdentityService, {
      getMe: () => {
        if (me === undefined) {
          throw new ConnectError("log in first", Code.Unauthenticated);
        }
        return { user: create(UserSchema, me) };
      },
      listUsers: (req) => {
        if (refuse.includes("listUsers")) {
          throw new ConnectError("needs identity:read", Code.PermissionDenied);
        }
        const from = req.pageToken === "" ? 0 : Number(req.pageToken);
        const size = Math.min(req.pageSize || 100, pageSize);
        const next = from + size < people.length ? String(from + size) : "";
        return { users: people.slice(from, from + size), nextPageToken: next };
      },
      setUserRole: (req) => {
        act("setUserRole", req);
        const u = find(req.userId);
        u.grants = u.grants.filter((g) => !(g.role === req.role && g.gameId === req.gameId));
        if (req.granted) {
          u.grants.push(create(RoleGrantSchema, { userId: u.id, role: req.role, gameId: req.gameId }));
        }
        return { user: u };
      },
      createLocalAccount: (req) => {
        act("createLocalAccount", req);
        const u = create(UserSchema, { id: "new-" + req.username, displayName: req.username, identities: [{ provider: "local", subject: req.username }] });
        people.push(u);
        return { user: u, link: create(PasswordLinkSchema, { url: "/auth/local/reset/set-" + req.username, purpose: "set", expiresAt: expires }) };
      },
      issuePasswordReset: (req) => {
        act("issuePasswordReset", req);
        return { link: create(PasswordLinkSchema, { url: "https://hub.example.org/auth/local/reset/reset-" + req.username, purpose: "reset", expiresAt: expires }) };
      },
      revokeUserSessions: (req) => {
        act("revokeUserSessions", req);
        find(req.userId);
        return { revoked: 3 };
      },
    });
    service(OrganizationService, {
      getOrganization: () => ({ organization: organization }),
      nominateOwner: (req) => {
        act("nominateOwner", req);
        const u = find(req.userId);
        organization.pendingOwnerUserId = u.id;
        organization.pendingOwnerName = u.displayName;
        organization.pendingOwnerExpiresAt = timestampFromDate(new Date("2026-10-21T12:00:00Z"));
        return { organization };
      },
      acceptOwnership: (req) => {
        act("acceptOwnership", req);
        organization.ownerUserId = organization.pendingOwnerUserId;
        organization.pendingOwnerUserId = "";
        return { organization };
      },
      cancelOwnershipTransfer: (req) => {
        act("cancelOwnershipTransfer", req);
        organization.pendingOwnerUserId = "";
        return { organization };
      },
      listEvents: (req) => {
        calls.push(req);
        if (log instanceof ConnectError) {
          throw log;
        }
        return readLog(log.map((e) => create(EventSchema, e)), req);
      },
      listEventKinds: () => ({ kinds: kinds.map((k) => create(EventKindSchema, k)) }),
    });
    service(ServerService, {
      getFleetSummary: () => {
        if (fleet instanceof ConnectError) {
          throw fleet;
        }
        return create(GetFleetSummaryResponseSchema, fleet);
      },
      listServers: () => ({ servers: servers.map((x) => create(ServerSchema, x)) }),
      listGames: () => ({ games: games.map((g) => create(GameSchema, g)) }),
      getServerStatus: (req) => {
        const s = servers.find((x) => x.id === req.serverId);
        if (!s) {
          throw new ConnectError("no such server", Code.NotFound);
        }
        return { server: create(ServerSchema, s) };
      },
    });
    service(ServerHostService, {
      getServerHost: (req) => {
        const h = hostRows.get(req.serverId);
        if (!h) {
          throw new ConnectError("not a cloud-VM server", Code.NotFound);
        }
        return { host: h };
      },
      setServerHostDesired: (req) =>
        hostAct("setServerHostDesired", req, (h) => {
          h.desired = req.desired;
          h.state = req.desired === "parked" ? "parking" : "creating";
        }),
      rebootServerHost: (req) =>
        hostAct("rebootServerHost", req, (h) => {
          h.state = "rebooting";
        }),
      setServerHostExpectation: (req) =>
        hostAct("setServerHostExpectation", req, (h) => {
          h.expectedPlayers = req.players;
          h.expectedUntil = req.until;
        }),
      resizeServerHost: (req) =>
        hostAct("resizeServerHost", req, (h) => {
          h.targetSize = req.size;
          h.pinnedSize = req.pin ? req.size : h.pinnedSize;
        }),
      unpinServerHostSize: (req) =>
        hostAct("unpinServerHostSize", req, (h) => {
          h.pinnedSize = "";
        }),
      snapshotServerHostVolume: (req) => ({ ...hostAct("snapshotServerHostVolume", req), snapshot: { name: req.name || req.serverId + "-data" } }),
      destroyServerHost: (req) =>
        hostAct("destroyServerHost", req, (h) => {
          h.state = "destroying";
        }),
    });
    service(ServerLeaseService, {
      listServerLeases: (req) => {
        const l = leaseRows.get(req.serverId);
        return { leases: l ? [l] : [] };
      },
      releaseServerLease: (req) => {
        act("releaseServerLease", req);
        const [sid] = [...leaseRows.entries()].find(([, l]) => l.id === req.leaseId) ?? [];
        if (sid === undefined) {
          throw new ConnectError("no such lease", Code.NotFound);
        }
        const lease = leaseRows.get(sid);
        leaseRows.delete(sid);
        nextEvent += 1n;
        return { lease, eventId: nextEvent };
      },
    });
  });
  const left: string[] = [];
  const here = "/admin" + (path === "/" ? "" : path);
  const browser: Browser = { here: () => here, leave: (to) => left.push(to) };
  // No retries and no polling unless asked: a test sees the first answer.
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, refetchInterval: poll > 0 ? poll : false } } });
  const { hook } = memoryLocation({ path: here });
  const view = render(<App transport={transport} queryClient={queryClient} browser={browser} location={hook} />);
  return { ...view, left, calls, actions, people, organization, hostRows };
}

// readLog answers ListEvents as the hub does (internal/api/events.go): after_id oldest first,
// otherwise newest first before before_id; the filters the feed sends; head_id the log's newest.
function readLog(all: Event[], req: ListEventsRequest) {
  const severities = ["info", "notice", "warning", "critical"];
  const limit = req.limit || 100;
  const match = all.filter(
    (e) =>
      (req.kinds.length === 0 || req.kinds.some((k) => e.kind === k || e.kind.startsWith(k + "."))) &&
      (req.minSeverity === "" || severities.indexOf(e.severity) >= severities.indexOf(req.minSeverity)) &&
      (req.serverId === "" || e.serverId === req.serverId) &&
      (req.gameId === "" || e.gameId === req.gameId) &&
      (req.actorUserId === "" || e.actorUserId === req.actorUserId) &&
      (req.actorAppId === "" || e.actorAppId === req.actorAppId),
  );
  const headId = all.reduce((m, e) => (e.id > m ? e.id : m), 0n);
  if (req.afterId > 0n) {
    const events = match.filter((e) => e.id > req.afterId).slice(0, limit);
    return { events, headId, nextAfterId: events[events.length - 1]?.id ?? req.afterId };
  }
  const events = match
    .filter((e) => req.beforeId === 0n || e.id < req.beforeId)
    .reverse()
    .slice(0, limit);
  const nextBeforeId = events.length === limit ? (events[events.length - 1]?.id ?? 0n) : 0n;
  return { events, headId, nextAfterId: events[0]?.id ?? 0n, nextBeforeId };
}
