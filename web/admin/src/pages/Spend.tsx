// Cloud spend (gravel#207, ADR-0034): what the cloud costs this month, by day and by cause, and
// where the month ends. The cloud's own bill where a location's token may read it, the hub's
// compute estimate beside it. servers:provision everywhere.
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { useQuery } from "@tanstack/react-query";
import { useState } from "react";

import { type GetSpendResponse, ServerHostService } from "../gen/gravel/hub/v1/hosts_pb";
import { ServerService } from "../gen/gravel/hub/v1/servers_pb";
import { useClient } from "../transport";

const kinds = [
  ["compute", "Compute"],
  ["storage", "Storage"],
  ["network", "Network"],
  ["other", "Other"],
] as const;

// widths are a bar's widths in tenths, as classes Tailwind sees in the source (no style prop).
const widths = ["w-0", "w-[10%]", "w-[20%]", "w-[30%]", "w-[40%]", "w-[50%]", "w-[60%]", "w-[70%]", "w-[80%]", "w-[90%]", "w-full"];

// bar is a value's bar class against the peak, at least a sliver when it's above zero.
export function bar(v: number, peak: number): string {
  if (v <= 0 || peak <= 0) {
    return widths[0] ?? "w-0";
  }
  return widths[Math.max(1, Math.round((v / peak) * 10))] ?? "w-full";
}

export function usd(v: number): string {
  return new Intl.NumberFormat("en-US", { style: "currency", currency: "USD" }).format(v);
}

// months are this month and the eleven before it, newest first, as "YYYY-MM".
export function months(now: Date): string[] {
  return Array.from({ length: 12 }, (_, i) => {
    const d = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth() - i, 1));
    return `${String(d.getUTCFullYear())}-${String(d.getUTCMonth() + 1).padStart(2, "0")}`;
  });
}

function monthName(m: string): string {
  const [y, mo] = m.split("-").map(Number);
  return new Intl.DateTimeFormat("en-GB", { month: "long", year: "numeric", timeZone: "UTC" }).format(new Date(Date.UTC(y ?? 2000, (mo ?? 1) - 1, 1)));
}

export function Spend() {
  const api = useClient(ServerHostService);
  const [month, setMonth] = useState("");
  const q = useQuery({
    queryKey: ["spend", month],
    queryFn: ({ signal }) => api.getSpend({ month }, { signal }),
    refetchInterval: 300_000,
    staleTime: 290_000,
  });
  const choices = months(new Date());
  return (
    <>
      <div className="mb-4 flex flex-wrap items-baseline justify-between gap-3">
        <h1 className="text-2xl font-bold">Cloud spend</h1>
        <label className="flex items-center gap-2">
          Month
          <select className="rounded border border-line bg-surface px-2 py-1" value={month || (choices[0] ?? "")} onChange={(e) => { setMonth(e.target.value === choices[0] ? "" : e.target.value); }}>
            {choices.map((m) => (
              <option key={m} value={m}>
                {monthName(m)}
              </option>
            ))}
          </select>
        </label>
      </div>
      {q.error ? (
        <p role={ConnectError.from(q.error).code === Code.PermissionDenied ? undefined : "alert"}>
          {ConnectError.from(q.error).code === Code.PermissionDenied
            ? "Cloud spend needs servers:provision across the whole hub."
            : `The hub did not answer: ${ConnectError.from(q.error).rawMessage}.`}
        </p>
      ) : !q.data ? (
        <p role="status" className="text-muted">
          Loading the spend…
        </p>
      ) : !q.data.available ? (
        <p>This hub has no cloud location: nothing it runs is billed by a cloud.</p>
      ) : (
        <Report s={q.data} />
      )}
    </>
  );
}

function Report({ s }: { s: GetSpendResponse }) {
  const servers = useClient(ServerService);
  const list = useQuery({ queryKey: ["servers"], queryFn: ({ signal }) => servers.listServers({}, { signal }), staleTime: Infinity, refetchInterval: false });
  const serverName = (id: string) => list.data?.servers.find((x) => x.id === id)?.name ?? id;
  const zone = s.timezone || "UTC";
  const asOf = s.billedAsOf
    ? new Intl.DateTimeFormat("en-GB", { day: "numeric", month: "short", hour: "2-digit", minute: "2-digit", timeZone: zone }).format(timestampDate(s.billedAsOf))
    : "";
  const headline = s.billed ? s.billedUsd : s.estimatedUsd;
  const peak = Math.max(0, ...s.days.map((d) => Math.max(d.hasBilled ? d.billedUsd : 0, d.estimatedUsd)));
  const unbilled = s.locations.filter((l) => !l.billed);
  return (
    <div className="grid min-w-0 gap-6">
      <dl className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <Tile label={s.billed ? "Billed so far" : "Estimated so far"} value={usd(headline)} />
        <Tile label={s.current ? "Projected for the month" : "The month's total"} value={usd(s.projectedUsd)} />
        <Tile label="Estimated compute" value={usd(s.estimatedUsd)} />
        <Tile label="Bill as of" value={s.billed ? asOf : "–"} />
      </dl>
      <p className="text-muted">
        {s.billed
          ? `From the cloud’s own bill, which lags a day; the projection adds the recent days’ average for the days left. Days in ${zone}.`
          : `The hub’s estimate: its servers’ compute at the prices recorded when each run began. A floor: disks, addresses, snapshots and anything the hub didn’t make bill too. Days in ${zone}.`}
      </p>
      {unbilled.map((l) => (
        <p key={l.id} className="border-l-4 border-warn pl-2">
          {l.id}: {l.reason}.
        </p>
      ))}

      <section aria-labelledby="causes" className="grid min-w-0 gap-2">
        <h2 id="causes" className="text-xl font-bold">
          What it&rsquo;s for
        </h2>
        {s.causes.length === 0 ? (
          <p>Nothing has cost anything this month.</p>
        ) : (
          <div className="relative min-w-0 overflow-x-auto" tabIndex={0} role="region" aria-label="What it’s for, the table">
            <table className="w-full text-left">
              <thead>
                <tr className="border-b border-line">
                  <th scope="col" className="px-2">
                    Server or resource
                  </th>
                  {s.billed &&
                    kinds.map(([k, label]) => (
                      <th key={k} scope="col" className="px-2 text-right">
                        {label}
                      </th>
                    ))}
                  {s.billed && (
                    <th scope="col" className="px-2 text-right">
                      Billed
                    </th>
                  )}
                  <th scope="col" className="px-2 text-right">
                    Estimated compute
                  </th>
                </tr>
              </thead>
              <tbody>
                {s.causes.map((c) => (
                  <tr key={c.serverId || `name:${c.name}`} className="border-b border-line">
                    <td className="px-2">
                      {c.serverId ? <a href={`/admin/servers/${c.serverId}`}>{serverName(c.serverId)}</a> : <span>{c.name || "Unnamed"}</span>}
                    </td>
                    {s.billed &&
                      kinds.map(([k]) => (
                        <td key={k} className="px-2 text-right">
                          {c.kinds[k] ? usd(c.kinds[k]) : "–"}
                        </td>
                      ))}
                    {s.billed && <td className="px-2 text-right">{usd(c.billedUsd)}</td>}
                    <td className="px-2 text-right">{c.serverId ? usd(c.estimatedUsd) : "–"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      <section aria-labelledby="days" className="grid min-w-0 gap-2">
        <h2 id="days" className="text-xl font-bold">
          Day by day
        </h2>
        <div className="relative min-w-0 overflow-x-auto" tabIndex={0} role="region" aria-label="Day by day, the table">
          <table className="w-full text-left">
            <thead>
              <tr className="border-b border-line">
                <th scope="col" className="px-2">
                  Day
                </th>
                {s.billed && (
                  <th scope="col" className="px-2 text-right">
                    Billed
                  </th>
                )}
                <th scope="col" className="px-2 text-right">
                  Estimated compute
                </th>
                <th scope="col" className="w-1/2 px-2">
                  <span className="sr-only">As a bar</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {[...s.days].reverse().map((d) => {
                const v = s.billed && d.hasBilled ? d.billedUsd : d.estimatedUsd;
                return (
                  <tr key={d.date} className="border-b border-line">
                    <td className="whitespace-nowrap px-2">{d.date}</td>
                    {s.billed && <td className="px-2 text-right">{d.hasBilled ? usd(d.billedUsd) : "–"}</td>}
                    <td className="px-2 text-right">{usd(d.estimatedUsd)}</td>
                    <td className="px-2" aria-hidden="true">
                      <div className={`h-3 rounded bg-accent/40 ${bar(v, peak)}`} />
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      </section>
    </div>
  );
}

function Tile({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded border border-line bg-surface p-3">
      <dt className="text-sm text-muted">{label}</dt>
      <dd className="text-2xl font-bold">{value}</dd>
    </div>
  );
}
