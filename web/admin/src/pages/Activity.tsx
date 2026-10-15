// Player activity (gravel#208, ADR-0035): when people play and who stopped, from the sessions the
// hub stitches from its polls. stats:read everywhere. Players are shown as the boards show them:
// a member's name when they chose it, else a pseudonym.
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { useQuery } from "@tanstack/react-query";
import { useState } from "react";

import { type ActivityHour, type GetActivityResponse, StatsService } from "../gen/gravel/hub/v1/stats_pb";
import { ServerService } from "../gen/gravel/hub/v1/servers_pb";
import { useClient } from "../transport";

const button = "rounded border border-line bg-surface px-3 py-1";
const primary = "rounded border-2 border-accent bg-surface px-3 py-1 font-bold";

const windows = [
  ["7d", "7 days"],
  ["30d", "30 days"],
  ["90d", "90 days"],
] as const;

const dayNames = ["Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"];

// shades are the heatmap's steps, quietest first: a tint of the accent behind the number.
const shades = ["", "bg-accent/10", "bg-accent/20", "bg-accent/30", "bg-accent/40"];

// shade is a cell's step: 0 for nobody, else a quarter of the busiest hour's average or more.
export function shade(average: number, busiest: number): number {
  if (average <= 0 || busiest <= 0) {
    return 0;
  }
  return Math.min(4, Math.max(1, Math.ceil((average / busiest) * 4)));
}

// length says a duration in hours and minutes.
export function length(seconds: number): string {
  const m = Math.round(seconds / 60);
  if (m < 60) {
    return `${String(m)} min`;
  }
  const h = Math.floor(m / 60);
  return m % 60 === 0 ? `${String(h)} h` : `${String(h)} h ${String(m % 60)} min`;
}

// busiest are the hours with the most players on average, at most n, busiest first.
export function busiest(hours: readonly ActivityHour[], n: number): ActivityHour[] {
  return [...hours]
    .filter((h) => h.average > 0)
    .sort((a, b) => b.average - a.average || a.day - b.day || a.hour - b.hour)
    .slice(0, n);
}

function clock(hour: number): string {
  return `${String(hour).padStart(2, "0")}:00`;
}

export function Activity() {
  const api = useClient(StatsService);
  const [span, setSpan] = useState<string>("30d");
  const q = useQuery({
    queryKey: ["activity", span],
    queryFn: ({ signal }) => api.getActivity({ window: span }, { signal }),
    refetchInterval: 60_000,
    staleTime: 55_000,
  });
  return (
    <>
      <div className="mb-4 flex flex-wrap items-baseline justify-between gap-3">
        <h1 className="text-2xl font-bold">Player activity</h1>
        <div role="group" aria-label="Window" className="flex flex-wrap gap-2">
          {windows.map(([key, label]) => (
            <button key={key} type="button" className={key === span ? primary : button} aria-pressed={key === span} onClick={() => { setSpan(key); }}>
              {label}
            </button>
          ))}
        </div>
      </div>
      {q.error ? (
        <p role={ConnectError.from(q.error).code === Code.PermissionDenied ? undefined : "alert"}>
          {ConnectError.from(q.error).code === Code.PermissionDenied
            ? "Player activity needs stats:read across the whole hub, in person."
            : `The hub did not answer: ${ConnectError.from(q.error).rawMessage}.`}
        </p>
      ) : !q.data ? (
        <p role="status" className="text-muted">
          Loading the activity…
        </p>
      ) : (
        <Report a={q.data} />
      )}
    </>
  );
}

function Report({ a }: { a: GetActivityResponse }) {
  const servers = useClient(ServerService);
  const list = useQuery({ queryKey: ["servers"], queryFn: ({ signal }) => servers.listServers({}, { signal }), staleTime: Infinity, refetchInterval: false });
  const serverName = (id: string) => list.data?.servers.find((s) => s.id === id)?.name ?? id;
  const zone = a.timezone || "UTC";
  const date = (d: Date) => new Intl.DateTimeFormat("en-GB", { day: "numeric", month: "short", year: "numeric", timeZone: zone }).format(d);
  const from = a.from ? timestampDate(a.from) : undefined;
  const to = a.to ? timestampDate(a.to) : undefined;
  const top = busiest(a.hours, 3);
  const max = top[0]?.average ?? 0;
  const byDay = dayNames.map((_, d) => a.hours.filter((h) => h.day === d + 1).sort((x, y) => x.hour - y.hour));
  return (
    <div className="grid min-w-0 gap-6">
      <p className="text-muted">
        {from && to ? `${date(from)} to ${date(to)}, ` : ""}hours and days in {zone}. Counted from the hub’s polls of its servers: a session is a player on one server,
        until they miss more than two polls.
      </p>
      {a.partial && (
        <p className="border-l-4 border-warn pl-2">
          This reaches back past how long the hub keeps sessions (stats.raw_retention_months); only monthly totals are kept before that, so regulars and new players
          there are incomplete.
        </p>
      )}
      <dl className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <Tile label="Players" value={String(a.players)} />
        <Tile label="Sessions" value={String(a.sessions)} />
        <Tile label="Median session" value={a.sessions > 0 ? length(a.medianSessionSeconds) : "–"} />
        <Tile label="Came back on another day" value={a.players > 0 ? `${String(a.returningPlayers)} of ${String(a.players)}` : "–"} />
        <Tile label="Days played, on average" value={a.players > 0 ? a.meanActiveDays.toFixed(1) : "–"} />
        <Tile label="New players" value={String(a.newPlayers)} />
        <Tile label="Not seen since the window before" value={String(a.lostPlayers)} />
        <Tile label="Growth" value={signed(a.newPlayers - a.lostPlayers)} />
      </dl>

      <section aria-labelledby="peak" className="grid min-w-0 gap-2">
        <h2 id="peak" className="text-xl font-bold">
          When people play
        </h2>
        {a.sessions === 0 && a.hours.every((h) => h.average === 0) ? (
          <p>No one played in this window.</p>
        ) : (
          <>
            <p>
              Busiest:{" "}
              {top.map((h, i) => (
                <span key={`${String(h.day)}-${String(h.hour)}`}>
                  {i > 0 ? "; " : ""}
                  {dayNames[h.day - 1]} {clock(h.hour)}, {h.average.toFixed(1)} on average (at most {h.peak})
                </span>
              ))}
              .
            </p>
            {/* A focusable region, so the week can be scrolled sideways from the keyboard on a phone; relative,
                so the cells' screen-reader text stays inside it and doesn't widen the page. */}
            <div className="relative min-w-0 overflow-x-auto" tabIndex={0} role="region" aria-label="The week, hour by hour">
              <table className="border-collapse text-xs">
                <caption className="mb-1 text-left text-sm text-muted">
                  Players on in each hour of the week, on average over the window; a deeper tint is busier.
                </caption>
                <thead>
                  <tr>
                    <th scope="col" className="sr-only">
                      Day
                    </th>
                    {Array.from({ length: 24 }, (_, hour) => (
                      <th key={hour} scope="col" className="px-1 font-normal text-muted">
                        {String(hour).padStart(2, "0")}
                      </th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {byDay.map((hours, d) => (
                    <tr key={dayNames[d]}>
                      <th scope="row" className="pr-2 text-left font-normal">
                        {dayNames[d]?.slice(0, 3)}
                      </th>
                      {Array.from({ length: 24 }, (_, hour) => {
                        const h = hours.find((x) => x.hour === hour);
                        const avg = h?.average ?? 0;
                        return (
                          <td key={hour} className={`h-8 min-w-8 border border-line text-center ${shades[shade(avg, max)] ?? ""}`}>
                            {avg > 0 ? avg.toFixed(1) : ""}
                            <span className="sr-only">{avg > 0 ? ` players at ${clock(hour)}` : `nobody at ${clock(hour)}`}</span>
                          </td>
                        );
                      })}
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </>
        )}
      </section>

      <section aria-labelledby="weeks" className="grid gap-2">
        <h2 id="weeks" className="text-xl font-bold">
          Week by week
        </h2>
        {a.weeks.length === 0 ? (
          <p>No sessions in this window.</p>
        ) : (
          <table className="w-full max-w-md text-left">
            <thead>
              <tr className="border-b border-line">
                <th scope="col">Week of</th>
                <th scope="col" className="text-right">
                  Players
                </th>
                <th scope="col" className="text-right">
                  New
                </th>
              </tr>
            </thead>
            <tbody>
              {a.weeks.map((w) => (
                <tr key={w.start ? String(timestampDate(w.start).getTime()) : ""} className="border-b border-line">
                  <td>{w.start ? date(timestampDate(w.start)) : ""}</td>
                  <td className="text-right">{w.players}</td>
                  <td className="text-right">{w.newPlayers}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      <section aria-labelledby="quiet" className="grid min-w-0 gap-2">
        <h2 id="quiet" className="text-xl font-bold">
          Regulars gone quiet
        </h2>
        <p className="text-muted">
          Played on {a.regularDays} days or more in the {a.regularWeeks} weeks before this window, and not once in it. Names show only for members who chose to be shown on
          the boards.
        </p>
        {a.quiet.length === 0 ? (
          <p>No regular has gone quiet.</p>
        ) : (
          <div className="relative min-w-0 overflow-x-auto" tabIndex={0} role="region" aria-label="Regulars gone quiet, the table">
            <table className="w-full text-left">
              <thead>
                <tr className="border-b border-line">
                  <th scope="col" className="px-2">Player</th>
                  <th scope="col" className="px-2 text-right">
                    Days before
                  </th>
                  <th scope="col" className="px-2 text-right">
                    Hours before
                  </th>
                  <th scope="col" className="px-2">Last seen</th>
                  <th scope="col" className="px-2">On</th>
                </tr>
              </thead>
              <tbody>
                {a.quiet.map((r, i) => (
                  <tr key={`${r.name}-${String(i)}`} className="border-b border-line">
                    <td className="px-2">{r.userId !== "" ? <a href={`/members/${r.userId}`}>{r.name}</a> : r.name}</td>
                    <td className="px-2 text-right">{r.daysBefore}</td>
                    <td className="px-2 text-right">{r.hours.toFixed(1)}</td>
                    <td className="whitespace-nowrap px-2">{r.lastSeen ? date(timestampDate(r.lastSeen)) : ""}</td>
                    <td className="px-2">{serverName(r.lastServerId)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </div>
  );
}

function signed(n: number): string {
  return n > 0 ? `+${String(n)}` : String(n);
}

function Tile({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded border border-line bg-surface p-3">
      <dt className="text-sm text-muted">{label}</dt>
      <dd className="text-2xl font-bold">{value}</dd>
    </div>
  );
}
