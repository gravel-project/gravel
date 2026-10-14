// The shell over fake services: a router transport (no network) answering as the test says, a
// memory location under /admin, and a browser that records where the shell sent the viewer.
import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";
import { render } from "@testing-library/react";
import { QueryClient } from "@tanstack/react-query";
import { memoryLocation } from "wouter/memory-location";

import { App } from "../App";
import { IdentityService, UserSchema } from "../gen/gravel/hub/v1/identity_pb";
import {
  type Event,
  EventKindSchema,
  EventSchema,
  type ListEventsRequest,
  OrganizationSchema,
  OrganizationService,
} from "../gen/gravel/hub/v1/organization_pb";
import { GameSchema, GetFleetSummaryResponseSchema, ServerSchema, ServerService } from "../gen/gravel/hub/v1/servers_pb";
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
}

export type LogEvent = MessageInitShape<typeof EventSchema>;

export type Fleet = MessageInitShape<typeof GetFleetSummaryResponseSchema>;

type Member = MessageInitShape<typeof UserSchema>;

export function member(fields: Member = {}): Member {
  return { id: "u1", displayName: "Jo", ...fields };
}

export function renderShell({ me, demo = false, path = "/", fleet = { totals: {} }, log = [], kinds = [], servers = [], games = [], poll = 0 }: RigOptions = {}) {
  // calls are the ListEvents requests, in order, for a test to check the cursors.
  const calls: ListEventsRequest[] = [];
  const transport = createRouterTransport(({ service }) => {
    service(IdentityService, {
      getMe: () => {
        if (me === undefined) {
          throw new ConnectError("log in first", Code.Unauthenticated);
        }
        return { user: create(UserSchema, me) };
      },
    });
    service(OrganizationService, {
      getOrganization: () => ({ organization: create(OrganizationSchema, { id: "o1", name: "Hidden Token Gaming", owned: true, demo }) }),
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
    });
  });
  const left: string[] = [];
  const here = "/admin" + (path === "/" ? "" : path);
  const browser: Browser = { here: () => here, leave: (to) => left.push(to) };
  // No retries and no polling unless asked: a test sees the first answer.
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, refetchInterval: poll > 0 ? poll : false } } });
  const { hook } = memoryLocation({ path: here });
  const view = render(<App transport={transport} queryClient={queryClient} browser={browser} location={hook} />);
  return { ...view, left, calls };
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
