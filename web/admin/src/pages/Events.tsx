// Events: the hub's events log as a feed (gravel#193, ADR-0028), the respond half of
// notify-and-respond. Newest first: the first read takes the log's head, then the page asks for
// what came after it every poll (no push to browsers, ADR-0028 §6) and pages back on request. The
// filters live in the address, so a link (a fleet alarm's, a row's) opens the feed filtered. What a
// game-scoped member sees is the hub's to decide: it answers their games' events only.
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useCallback, useEffect, useState } from "react";
import { Link, useSearchParams } from "wouter";

import { type Filters, filtered, filtersFrom, newer, noFilters, older, request, search, severity } from "../events";
import { clock, stale } from "../fleet";
import { type Event, type EventKind, OrganizationService } from "../gen/gravel/hub/v1/organization_pb";
import { ServerService } from "../gen/gravel/hub/v1/servers_pb";
import { isFinal } from "../queries";
import { useClient } from "../transport";

// pageSize is how many events the first read and each older page bring.
export const pageSize = 50;

// followBatch is the most one poll asks for; a fuller answer is asked again at once.
const followBatch = 500;

export function Events() {
  const [params, setParams] = useSearchParams();
  const f = filtersFrom(params);
  const feed = useFeed(f);
  const now = useNow(1_000);
  const set = (next: Filters) => {
    setParams(new URLSearchParams(search(next)));
  };
  return (
    <>
      <div className="mb-4 flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1">
        <h1 className="text-2xl font-bold">Events</h1>
        {feed.state === "ready" && <Following polledAt={feed.polledAt} now={now} failed={feed.failed} />}
      </div>
      {feed.state === "refused" ? (
        <p>Your roles do not include reading the events log (events:read). An owner or admin grants roles on the hub.</p>
      ) : (
        <>
          <FilterForm f={f} set={set} />
          {feed.state === "ready" ? (
            <Feed f={f} feed={feed} />
          ) : feed.state === "error" ? (
            <p role="alert">The hub did not answer: {feed.message}. Trying again shortly.</p>
          ) : (
            <p role="status" className="text-muted">
              Loading the events…
            </p>
          )}
        </>
      )}
    </>
  );
}

type FeedState =
  | { state: "loading" }
  | { state: "refused" }
  | { state: "error"; message: string }
  | { state: "ready"; events: Event[]; before: bigint; polledAt: number; failed: boolean; olderBusy: boolean; olderError: string; more: () => void };

// useFeed reads the log for f: the newest page, then what comes after the head every poll (the
// dashboard's refetchInterval, so a background tab and a test's rig set it alike), and older pages
// when asked. A change of filter starts again.
function useFeed(f: Filters): FeedState {
  const client = useClient(OrganizationService);
  const queryClient = useQueryClient();
  const every = queryClient.getDefaultOptions().queries?.refetchInterval;
  const interval = typeof every === "number" ? every : 0;
  const key = search(f);
  // The state carries the filters it was read for: after a change, until the new first read
  // answers, the old feed is not shown (the page says it is loading).
  const [tagged, setS] = useState<{ key: string } & (Exclude<FeedState, { state: "ready" }> | Omit<Extract<FeedState, { state: "ready" }>, "more">)>({
    key,
    state: "loading",
  });
  const s = tagged.key === key ? tagged : ({ key, state: "loading" } as const);

  useEffect(() => {
    const ac = new AbortController();
    const req = request(filtersFrom(new URLSearchParams(key)));
    let after = 0n;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const later = (fn: () => Promise<void>) => {
      if (interval > 0) {
        timer = setTimeout(() => void fn(), interval);
      }
    };
    const first = async () => {
      try {
        const r = await client.listEvents({ ...req, limit: pageSize }, { signal: ac.signal });
        after = r.headId;
        setS({ key, state: "ready", events: r.events, before: r.nextBeforeId, polledAt: Date.now(), failed: false, olderBusy: false, olderError: "" });
        later(poll);
      } catch (err) {
        if (ac.signal.aborted) {
          return;
        }
        const e = ConnectError.from(err);
        if (e.code === Code.PermissionDenied) {
          setS({ key, state: "refused" });
          return;
        }
        setS({ key, state: "error", message: e.rawMessage });
        if (!isFinal(err)) {
          later(first);
        }
      }
    };
    const poll = async () => {
      if (document.visibilityState === "hidden") {
        later(poll);
        return;
      }
      try {
        for (;;) {
          const r = await client.listEvents({ ...req, afterId: after, limit: followBatch }, { signal: ac.signal });
          if (r.events.length > 0) {
            setS((cur) => (cur.key === key && cur.state === "ready" ? { ...cur, events: newer(cur.events, r.events) } : cur));
          }
          after = r.nextAfterId > after ? r.nextAfterId : after;
          if (r.events.length < followBatch) {
            break;
          }
        }
        setS((cur) => (cur.key === key && cur.state === "ready" ? { ...cur, polledAt: Date.now(), failed: false } : cur));
      } catch {
        if (ac.signal.aborted) {
          return;
        }
        setS((cur) => (cur.key === key && cur.state === "ready" ? { ...cur, failed: true } : cur));
      }
      later(poll);
    };
    void first();
    return () => {
      ac.abort();
      clearTimeout(timer);
    };
  }, [client, key, interval]);

  const before = s.state === "ready" ? s.before : 0n;
  const more = useCallback(() => {
    if (before === 0n) {
      return;
    }
    setS((cur) => (cur.key === key && cur.state === "ready" ? { ...cur, olderBusy: true, olderError: "" } : cur));
    client
      .listEvents({ ...request(filtersFrom(new URLSearchParams(key))), beforeId: before, limit: pageSize })
      .then((r) => {
        setS((cur) => (cur.key === key && cur.state === "ready" ? { ...cur, events: older(cur.events, r.events), before: r.nextBeforeId, olderBusy: false } : cur));
      })
      .catch((err: unknown) => {
        setS((cur) => (cur.key === key && cur.state === "ready" ? { ...cur, olderBusy: false, olderError: ConnectError.from(err).rawMessage } : cur));
      });
  }, [client, key, before]);
  return s.state === "ready" ? { ...s, more } : s;
}

function useNow(ms: number): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => {
      setNow(Date.now());
    }, ms);
    return () => {
      clearInterval(t);
    };
  }, [ms]);
  return now;
}

function Following({ polledAt, now, failed }: { polledAt: number; now: number; failed: boolean }) {
  if (!stale(polledAt, now, failed)) {
    return <p className="text-sm text-muted">Following · checked {clock(new Date(polledAt))}</p>;
  }
  return (
    <p className="text-sm">
      <span className="mr-2 rounded bg-warn px-2 py-0.5 font-bold text-black">Stale</span>
      The hub has not answered since {clock(new Date(polledAt))}; newer events may be missing.
    </p>
  );
}

const severities = [
  ["", "Every severity"],
  ["notice", "Notice and above"],
  ["warning", "Warning and above"],
  ["critical", "Critical only"],
] as const;

function FilterForm({ f, set }: { f: Filters; set: (f: Filters) => void }) {
  const org = useClient(OrganizationService);
  const servers = useClient(ServerService);
  const once = { staleTime: Infinity, refetchInterval: false } as const;
  const kinds = useQuery({ queryKey: ["event-kinds"], queryFn: ({ signal }) => org.listEventKinds({}, { signal }), ...once });
  const list = useQuery({ queryKey: ["servers"], queryFn: ({ signal }) => servers.listServers({}, { signal }), ...once });
  const games = useQuery({ queryKey: ["games"], queryFn: ({ signal }) => servers.listGames({}, { signal }), ...once });
  const groups = groupKinds(kinds.data?.kinds ?? []);
  const field = "block w-full rounded border border-line bg-surface px-2 py-1";
  return (
    <form aria-label="Filter events" className="mb-4 grid gap-3 sm:grid-cols-2 lg:grid-cols-4" onSubmit={(e) => { e.preventDefault(); }}>
      <label className="text-sm">
        <span className="text-muted">Kind</span>
        <select className={field} value={f.kind} onChange={(e) => { set({ ...f, kind: e.target.value }); }}>
          <option value="">Every kind</option>
          {f.kind !== "" && !groups.some(([g, ks]) => g === f.kind || ks.some((k) => k.name === f.kind)) && <option value={f.kind}>{f.kind}</option>}
          {groups.map(([group, ks]) => (
            <optgroup key={group} label={group}>
              <option value={group}>Every {group} event</option>
              {ks.map((k) => (
                <option key={k.name} value={k.name}>
                  {k.name}
                </option>
              ))}
            </optgroup>
          ))}
        </select>
      </label>
      <label className="text-sm">
        <span className="text-muted">Severity</span>
        <select className={field} value={f.severity === "info" ? "" : f.severity} onChange={(e) => { set({ ...f, severity: e.target.value }); }}>
          {severities.map(([v, label]) => (
            <option key={v} value={v}>
              {label}
            </option>
          ))}
        </select>
      </label>
      <label className="text-sm">
        <span className="text-muted">Server</span>
        <select className={field} value={f.server} onChange={(e) => { set({ ...f, server: e.target.value }); }}>
          <option value="">Every server</option>
          {f.server !== "" && !list.data?.servers.some((s) => s.id === f.server) && <option value={f.server}>{f.server}</option>}
          {list.data?.servers.map((s) => (
            <option key={s.id} value={s.id}>
              {s.name || s.id}
            </option>
          ))}
        </select>
      </label>
      <label className="text-sm">
        <span className="text-muted">Game</span>
        <select className={field} value={f.game} onChange={(e) => { set({ ...f, game: e.target.value }); }}>
          <option value="">Every game</option>
          {f.game !== "" && !games.data?.games.some((g) => g.id === f.game) && <option value={f.game}>{f.game}</option>}
          {games.data?.games.map((g) => (
            <option key={g.id} value={g.id}>
              {g.name || g.id}
            </option>
          ))}
        </select>
      </label>
      {(f.actorUser !== "" || f.actorApp !== "" || filtered(f)) && (
        <p className="flex flex-wrap items-center gap-x-4 gap-y-1 text-sm sm:col-span-2 lg:col-span-4">
          {(f.actorUser !== "" || f.actorApp !== "") && (
            <span>
              Only what one {f.actorUser !== "" ? "member" : "app"} did.{" "}
              <Link href={"/events" + search({ ...f, actorUser: "", actorApp: "" })}>Everyone&apos;s</Link>
            </span>
          )}
          {filtered(f) && <Link href={"/events" + search(noFilters)}>Clear the filters</Link>}
        </p>
      )}
    </form>
  );
}

// groupKinds is the registry by family, in its order.
function groupKinds(kinds: readonly EventKind[]): [string, EventKind[]][] {
  const out: [string, EventKind[]][] = [];
  for (const k of kinds) {
    const last = out[out.length - 1];
    if (last?.[0] === k.group) {
      last[1].push(k);
    } else {
      out.push([k.group, [k]]);
    }
  }
  return out;
}

function Feed({ f, feed }: { f: Filters; feed: Extract<FeedState, { state: "ready" }> }) {
  if (feed.events.length === 0) {
    return (
      <section aria-labelledby="none" className="rounded border border-line bg-surface p-4">
        <h2 id="none" className="mb-2 text-xl font-bold">
          {filtered(f) ? "No events match" : "No events yet"}
        </h2>
        <p>{filtered(f) ? "Nothing in the log matches these filters. New ones show here as they happen." : "What the hub does shows here as it happens."}</p>
      </section>
    );
  }
  return (
    <section aria-label="The events log">
      <ol className="border-t border-line">
        {feed.events.map((e) => (
          <Row key={e.id.toString()} e={e} f={f} />
        ))}
      </ol>
      <div className="mt-4">
        {feed.before !== 0n ? (
          <button type="button" className="rounded border border-line bg-surface px-3 py-1" disabled={feed.olderBusy} onClick={feed.more}>
            {feed.olderBusy ? "Loading older events…" : "Older events"}
          </button>
        ) : (
          <p className="text-sm text-muted">That is the start of the log{filtered(f) ? " for these filters" : ""}.</p>
        )}
        {feed.olderError !== "" && <p role="alert">The hub did not answer: {feed.olderError}.</p>}
      </div>
    </section>
  );
}

function Row({ e, f }: { e: Event; f: Filters }) {
  const sev = severity(e.severity);
  const at = e.at ? timestampDate(e.at) : undefined;
  const meta: ReactNode[] = [];
  const only = (next: Partial<Filters>) => "/events" + search({ ...f, ...next });
  if (e.serverId) {
    meta.push(
      <span key="server">
        <a href={`/servers/${encodeURIComponent(e.serverId)}`}>{e.serverId}</a>{" "}
        {f.server !== e.serverId && (
          <Link href={only({ server: e.serverId })} aria-label={`Only ${e.serverId}'s events`} className="text-muted">
            (only)
          </Link>
        )}
      </span>,
    );
  }
  if (e.gameId && f.game !== e.gameId) {
    meta.push(
      <Link key="game" href={only({ game: e.gameId })}>
        {e.gameId}
      </Link>,
    );
  }
  if (e.actorName || e.actorUserId || e.actorAppId) {
    const who = e.actorName || "someone gone";
    const mine = e.actorUserId ? { actorUser: e.actorUserId, actorApp: "" } : { actorUser: "", actorApp: e.actorAppId };
    meta.push(
      f.actorUser === mine.actorUser && f.actorApp === mine.actorApp ? (
        <span key="actor">by {who}</span>
      ) : (
        <span key="actor">
          by{" "}
          <Link href={only(mine)} aria-label={`Only what ${who} did`}>
            {who}
          </Link>
        </span>
      ),
    );
  }
  const detail = Object.entries(e.detail).sort(([a], [b]) => a.localeCompare(b));
  return (
    <li className="grid gap-x-4 gap-y-1 border-b border-line py-3 md:grid-cols-[6.5rem_minmax(0,1fr)_auto]">
      <div>
        <span className={`inline-block rounded px-2 py-0.5 text-sm font-bold ${sev.pill}`}>{sev.label}</span>
      </div>
      <div className="min-w-0">
        <p className="break-words">{e.summary || e.kind}</p>
        <p className="text-sm text-muted">
          <Link href={only({ kind: e.kind })} className="font-mono">
            {e.kind}
          </Link>
          {meta.map((m, i) => (
            <span key={i}> · {m}</span>
          ))}
        </p>
        {detail.length > 0 && (
          <p className="text-sm break-words text-muted">
            {detail.map(([k, v]) => (
              <span key={k} className="mr-3">
                {k}: <span className="text-text">{v}</span>
              </span>
            ))}
          </p>
        )}
      </div>
      <div className="text-sm text-muted md:text-right">{at && <time dateTime={at.toISOString()}>{when(at)}</time>}</div>
    </li>
  );
}

// when is a date and time in the viewer's zone, to the second.
function when(d: Date): string {
  return d.toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", second: "2-digit" });
}
