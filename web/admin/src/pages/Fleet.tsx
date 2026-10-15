// Fleet: the landing page (gravel#192). What is up, who is playing, what is wrong, trouble first:
// a summary strip, the open alarms, then every server in the hub's own order (down, degraded,
// unknown, up, idle). It polls GetFleetSummary like everything else and says when what it shows
// is old. A server's name links to its member page, where its controls are.
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { useQuery } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { Link } from "wouter";

import { noFilters, search } from "../events";
import { ago, clock, duration, health, healthStyle, stale, trouble } from "../fleet";
import type { User } from "../gen/gravel/hub/v1/identity_pb";
import { type FleetAlarm, type FleetServer, type GetFleetSummaryResponse, ServerService } from "../gen/gravel/hub/v1/servers_pb";
import { manages } from "../servers";
import { useClient } from "../transport";

export function Fleet({ me }: { me: User }) {
  const servers = useClient(ServerService);
  const q = useQuery({
    queryKey: ["fleet"],
    queryFn: ({ signal }) => servers.getFleetSummary({}, { signal }),
  });
  const now = useNow(1_000);
  const refused = q.error !== null && ConnectError.from(q.error).code === Code.PermissionDenied;
  return (
    <>
      <div className="mb-4 flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1">
        <h1 className="text-2xl font-bold">Fleet</h1>
        {q.data && <AsOf summary={q.data} fetchedAt={q.dataUpdatedAt} now={now} failed={q.isError} />}
      </div>
      {refused ? (
        <p>Your roles do not include seeing the fleet (servers:read). An owner or admin grants roles on the hub.</p>
      ) : q.data ? (
        <Summary summary={q.data} me={me} />
      ) : q.isError ? (
        <p role="alert">The hub did not answer: {ConnectError.from(q.error).rawMessage}. Trying again shortly.</p>
      ) : (
        <p role="status" className="text-muted">
          Loading the fleet…
        </p>
      )}
    </>
  );
}

// useNow re-renders every ms, so "polled 8 s ago" and the stale notice keep up between polls.
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

function AsOf({ summary, fetchedAt, now, failed }: { summary: GetFleetSummaryResponse; fetchedAt: number; now: number; failed: boolean }) {
  const asOf = summary.asOf ? timestampDate(summary.asOf) : new Date(fetchedAt);
  if (!stale(fetchedAt, now, failed)) {
    return <p className="text-sm text-muted">As of {clock(asOf)}</p>;
  }
  return (
    <p className="text-sm">
      <span className="mr-2 rounded bg-warn px-2 py-0.5 font-bold text-black">Stale</span>
      The hub has not answered since {clock(new Date(fetchedAt))}; this is the fleet as of {clock(asOf)}.
    </p>
  );
}

function Summary({ summary, me }: { summary: GetFleetSummaryResponse; me: User }) {
  const now = summary.asOf ? timestampDate(summary.asOf) : new Date();
  const t = summary.totals;
  if (!t || t.servers === 0) {
    return (
      <section aria-labelledby="empty" className="rounded border border-line bg-surface p-4">
        <h2 id="empty" className="mb-2 text-xl font-bold">
          No servers yet
        </h2>
        <p>
          Servers come from a <code className="font-mono">servers.yaml</code> manifest. On the hub&apos;s host, run{" "}
          <code className="font-mono">gravel-hub servers apply servers.yaml</code>, and they show here within a poll.
        </p>
      </section>
    );
  }
  const counts = [
    [t.down, "down"],
    [t.degraded, "degraded"],
    [t.unknown, "unknown"],
    [t.idle, "idle"],
  ] as const;
  return (
    <>
      <dl className="mb-6 grid grid-cols-3 gap-2 sm:gap-4">
        <Tile label="Up" value={t.up} of={t.servers - t.idle}>
          {counts
            .filter(([n]) => n > 0)
            .map(([n, what]) => `${n} ${what}`)
            .join(" · ")}
        </Tile>
        <Tile label="Players" value={t.players} of={t.capacity} />
        <Tile label="In alarm" value={summary.alarms.length} loud={summary.alarms.length > 0} />
      </dl>
      {summary.alarms.length > 0 && <Alarms alarms={summary.alarms} now={now} />}
      <section aria-labelledby="servers">
        <h2 id="servers" className="mb-2 text-xl font-bold">
          Servers
        </h2>
        <ul className="border-t border-line">
          {summary.servers.map((s) => (
            <Row key={s.id} server={s} now={now} me={me} />
          ))}
        </ul>
      </section>
    </>
  );
}

// Tile is one number of the strip, out of of when of is above 0, with a line under it.
function Tile({ label, value, of = 0, loud = false, children = "" }: { label: string; value: number; of?: number; loud?: boolean; children?: string }) {
  return (
    <div className={`rounded border bg-surface p-3 ${loud ? "border-2 border-error" : "border-line"}`}>
      <dt className="text-sm text-muted">{label}</dt>
      <dd>
        <span className="text-2xl font-bold sm:text-3xl">{value}</span>
        {of > 0 && <span className="text-muted"> of {of}</span>}
      </dd>
      {children !== "" && <dd className="text-sm text-muted">{children}</dd>}
    </div>
  );
}

function Alarms({ alarms, now }: { alarms: FleetAlarm[]; now: Date }) {
  return (
    <section aria-labelledby="alarms" className="mb-6 rounded border-2 border-error bg-surface p-3">
      <h2 id="alarms" className="mb-2 text-xl font-bold">
        {alarms.length === 1 ? "1 alarm" : `${alarms.length} alarms`}
      </h2>
      <ul className="space-y-2">
        {alarms.map((a) => (
          <li key={a.kind + "/" + a.serverId} className="flex flex-wrap items-baseline gap-x-2">
            <span className={`rounded px-2 py-0.5 text-sm font-bold text-black ${a.severity === "critical" ? "bg-error" : "bg-warn"}`}>
              {a.severity === "critical" ? "Critical" : "Warning"}
            </span>
            <span className="font-mono text-sm">{a.kind}</span>
            {a.serverId && <a href={`/servers/${encodeURIComponent(a.serverId)}`}>{a.serverId}</a>}
            <span className="basis-full sm:basis-auto">{a.detail}</span>
            {a.since && <span className="text-sm text-muted">since {ago(timestampDate(a.since), now)}</span>}
            {alarmEvents(a) !== "" && (
              <Link href={alarmEvents(a)} className="text-sm">
                Events
              </Link>
            )}
          </li>
        ))}
      </ul>
    </section>
  );
}

function Row({ server: s, now, me }: { server: FleetServer; now: Date; me: User }) {
  const h = healthStyle[health(s.health)];
  const held = s.healthSince ? duration(timestampDate(s.healthSince), now) : "";
  const host = [s.hostState && `host ${s.hostState}${s.hostErrorCode ? ` (${s.hostErrorCode})` : ""}`, s.leaseState && `lease ${s.leaseState}`].filter(Boolean);
  return (
    <li
      className={`grid gap-x-4 gap-y-1 border-b border-l-4 border-b-line py-3 pl-3 md:grid-cols-[7rem_minmax(0,2fr)_minmax(0,1fr)_minmax(0,1fr)] md:items-center ${
        trouble(s.health) ? "border-l-error" : "border-l-transparent"
      }`}
    >
      <div>
        <span className={`inline-block rounded px-2 py-0.5 text-sm font-bold text-black ${h.pill}`}>{h.label}</span>
      </div>
      <div className="min-w-0">
        <a href={`/servers/${encodeURIComponent(s.id)}`} className="font-bold break-words">
          {s.name || s.id}
        </a>
        <p className="text-sm text-muted">
          {s.gameName || s.gameId} · {s.reason}
          {held && ` · for ${held}`}
        </p>
      </div>
      <div className="min-w-0">
        {s.observedAt && s.maxPlayers > 0 ? (
          <>
            <p>
              {s.players} <span className="text-muted">/ {s.maxPlayers} players</span>
            </p>
            <meter aria-hidden="true" className="h-2 w-full max-w-48" min={0} max={s.maxPlayers} value={s.players} />
          </>
        ) : (
          <p className="text-muted">{s.observedAt ? `${s.players} players` : "no players known"}</p>
        )}
      </div>
      <div className="min-w-0 text-sm text-muted">
        {s.map && <p className="text-text">{s.map}</p>}
        {s.observedAt ? (
          <p>
            polled {ago(timestampDate(s.observedAt), now)} · {s.pollMs} ms
          </p>
        ) : (
          s.health !== "idle" && <p>never answered</p>
        )}
        {host.length > 0 && <p>{host.join(" · ")}</p>}
        {manages(me, s.gameId) && (
          <p>
            <Link href={`/servers/${encodeURIComponent(s.id)}`} className="text-accent underline" aria-label={`Manage ${s.name || s.id}`}>
              Manage
            </Link>
          </p>
        )}
      </div>
    </li>
  );
}

// alarmEvents is the feed filtered to what an alarm is about: its server's events, or the backups';
// "" for an alarm no event family matches (the hub unowned, WAL archiving).
function alarmEvents(a: FleetAlarm): string {
  if (a.serverId !== "") {
    return "/events" + search({ ...noFilters, server: a.serverId });
  }
  return a.kind.startsWith("backup.") ? "/events" + search({ ...noFilters, kind: "backup" }) : "";
}
