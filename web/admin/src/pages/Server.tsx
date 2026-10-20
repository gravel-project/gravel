// A server's controls (gravel#202): its host and what the viewer may do to it, at
// /admin/servers/{id}. Start, park, reboot and the expectation need servers:operate; resize, unpin,
// snapshot and destroy need servers:provision and the server's id typed to confirm; a live lease
// can be released with servers:lease. Buttons follow the capability for the server's game and the
// host's state. Each answer names the event it wrote; a refusal is shown in the hub's words.
// Moderation stays on the member page (/servers/{id}). The feed's gaps in the last 30 days (ADR-0011
// §5, gravel#3) close the page for whoever may read the stats.
import { timestampDate, timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, type SubmitEvent, useState } from "react";
import { Link } from "wouter";

import type { User } from "../gen/gravel/hub/v1/identity_pb";
import { type ServerHost, ServerHostService } from "../gen/gravel/hub/v1/hosts_pb";
import { ServerLeaseService } from "../gen/gravel/hub/v1/leases_pb";
import { ServerService } from "../gen/gravel/hub/v1/servers_pb";
import { StatsService } from "../gen/gravel/hub/v1/stats_pb";
import { gapWords, span } from "../gaps";
import { type Action, actions, holdsFor, provisioning } from "../servers";
import { useClient } from "../transport";
import { utc } from "./People";

const field = "block w-full rounded border border-line bg-surface px-2 py-1";
const button = "rounded border border-line bg-surface px-3 py-1 disabled:opacity-60";
// The main action: outlined in the accent, never filled with it (see People).
const primary = "rounded border-2 border-accent bg-surface px-3 py-1 font-bold disabled:opacity-60";

// Outcome is what the last action left: what happened and the event it wrote, or the refusal.
type Outcome = { ok: true; text: string; eventId: bigint } | { ok: false; text: string } | undefined;

function message(err: unknown): string {
  return ConnectError.from(err).rawMessage;
}

export function Server({ id, me }: { id: string; me: User }) {
  const servers = useClient(ServerService);
  const hosts = useClient(ServerHostService);
  const status = useQuery({ queryKey: ["server", id], queryFn: ({ signal }) => servers.getServerStatus({ serverId: id }, { signal }) });
  const host = useQuery({ queryKey: ["host", id], queryFn: ({ signal }) => hosts.getServerHost({ serverId: id }, { signal }) });
  const [outcome, setOutcome] = useState<Outcome>();
  const server = status.data?.server;
  if (status.error && ConnectError.from(status.error).code === Code.NotFound) {
    return (
      <>
        <h1 className="mb-4 text-2xl font-bold">No such server</h1>
        <p>
          The hub has no server {id}. <Link href="/" className="underline">Back to the fleet</Link>.
        </p>
      </>
    );
  }
  const game = server?.gameId ?? "";
  const notCloudVM = host.error !== null && ConnectError.from(host.error).code === Code.NotFound;
  return (
    <>
      <p className="mb-2 text-sm">
        <Link href="/" className="underline">
          Fleet
        </Link>{" "}
        / {server?.name ?? id}
      </p>
      <h1 className="mb-1 text-2xl font-bold">{server?.name ?? id}</h1>
      <p className="mb-4 text-muted">
        {server ? `${server.gameId} · ${server.driver} · ${server.status?.state ?? "unknown"}` : "Loading…"} ·{" "}
        <a href={`/servers/${encodeURIComponent(id)}`} className="underline">
          member page and moderation
        </a>
      </p>
      {outcome && <OutcomeNote outcome={outcome} server={id} />}
      {notCloudVM ? (
        <p>
          This server is not a cloud-VM server: the hub does not start, park or size its machine. Moderate it on its member page.
        </p>
      ) : host.data?.host ? (
        <>
          <HostFacts host={host.data.host} />
          <Controls id={id} me={me} game={game} host={host.data.host} done={setOutcome} />
        </>
      ) : host.error ? (
        <p role="alert">The hub did not answer: {message(host.error)}. Trying again shortly.</p>
      ) : (
        <p role="status" className="text-muted">
          Loading the host…
        </p>
      )}
      {holdsFor(me, "servers:lease", game) && <Lease id={id} done={setOutcome} />}
      <Gaps id={id} />
    </>
  );
}

// Gaps is what the server's event feed is known to have missed in the last 30 days. Stats the
// viewer may not read show nothing; any other failure says so.
function Gaps({ id }: { id: string }) {
  const stats = useClient(StatsService);
  const q = useQuery({ queryKey: ["gaps", id], queryFn: ({ signal }) => stats.listGaps({ serverId: id }, { signal }) });
  if (q.error && [Code.PermissionDenied, Code.Unauthenticated].includes(ConnectError.from(q.error).code)) {
    return null;
  }
  return (
    <section aria-labelledby="gaps" className="mb-6">
      <h2 id="gaps" className="mb-2 text-xl font-bold">
        Missing data
      </h2>
      <p className="mb-2 text-muted">
        What the server’s event feed is known to have missed in the last 30 days. Only a server with a feed is watched; the boards count from the hub’s polls, so
        they aren’t affected.
      </p>
      {q.error ? (
        <p role="alert">The hub did not answer: {message(q.error)}.</p>
      ) : !q.data ? (
        <p className="text-muted">Loading the gaps…</p>
      ) : q.data.gaps.length === 0 ? (
        <p>No gaps recorded.</p>
      ) : (
        <>
          <table className="w-full text-left">
            <thead>
              <tr>
                <th scope="col">From</th>
                <th scope="col">Length</th>
                <th scope="col">What happened</th>
              </tr>
            </thead>
            <tbody>
              {q.data.gaps.map((g, i) => {
                const from = g.from ? timestampDate(g.from) : undefined;
                const to = g.to ? timestampDate(g.to) : from;
                return (
                  <tr key={i}>
                    <td>{from ? utc(from) : "—"}</td>
                    <td>{from && to ? span(from, to) : "—"}</td>
                    <td>{gapWords(g)}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
          {q.data.more && <p className="mt-2 text-muted">Only the oldest {q.data.gaps.length} are listed.</p>}
        </>
      )}
    </section>
  );
}

function OutcomeNote({ outcome, server }: { outcome: NonNullable<Outcome>; server: string }) {
  if (!outcome.ok) {
    return (
      <p role="alert" className="mb-4 rounded border-2 border-error bg-surface p-3">
        {outcome.text}
      </p>
    );
  }
  return (
    <p role="status" className="mb-4 rounded border-2 border-ok bg-surface p-3">
      {outcome.text}
      {outcome.eventId > 0n && (
        <>
          {" "}
          <Link href={`/events?server=${encodeURIComponent(server)}`} className="underline">
            Event #{outcome.eventId.toString()}
          </Link>
          .
        </>
      )}
    </p>
  );
}

function HostFacts({ host: h }: { host: ServerHost }) {
  const rows: [string, ReactNode][] = [
    ["State", h.step !== "" ? `${h.state} (${h.step})` : h.state],
    ["Asked", h.desired],
    ["Size", h.pinnedSize !== "" ? `${h.size} (pinned)` : h.targetSize !== "" ? `${h.size} → ${h.targetSize}` : h.size || "—"],
    ["Address", h.address || "—"],
  ];
  if (h.expectedPlayers > 0 && h.expectedUntil) {
    rows.push(["Expecting", `${h.expectedPlayers} players until ${utc(timestampDate(h.expectedUntil))}${h.expectedReason ? ` (${h.expectedReason})` : ""}`]);
  }
  if (h.errorCode !== "") {
    rows.push(["Failing", `${h.errorCode} (attempt ${h.attempts})`]);
  }
  rows.push(["This month", `$${h.monthCostUsd.toFixed(2)} at $${h.hourlyPriceUsd.toFixed(3)} an hour`]);
  return (
    <section aria-labelledby="host" className="mb-6">
      <h2 id="host" className="mb-2 text-xl font-bold">
        Host
      </h2>
      <dl className="grid grid-cols-[8rem_minmax(0,1fr)] gap-x-4 gap-y-1">
        {rows.map(([k, v]) => (
          <div key={k} className="contents">
            <dt className="text-muted">{k}</dt>
            <dd className="break-words">{v}</dd>
          </div>
        ))}
      </dl>
    </section>
  );
}

const labels: Record<Action, string> = {
  start: "Start",
  park: "Park",
  reboot: "Reboot",
  expect: "Expect players",
  resize: "Resize",
  unpin: "Unpin the size",
  snapshot: "Snapshot the volume",
  destroy: "Destroy",
};

function Controls({ id, me, game, host, done }: { id: string; me: User; game: string; host: ServerHost; done: (o: Outcome) => void }) {
  const hosts = useClient(ServerHostService);
  const qc = useQueryClient();
  const [open, setOpen] = useState<Action | "">("");
  const offer = actions(me, game, host.state, host.pinnedSize);
  const run = useMutation({
    mutationFn: async ({ act, fields }: { act: Action; fields: Fields }): Promise<{ text: string; eventId: bigint }> => {
      const reason = fields.reason.trim();
      switch (act) {
        case "start":
          return { text: "Starting: the host comes up in a few minutes.", eventId: (await hosts.setServerHostDesired({ serverId: id, desired: "running", reason })).eventId };
        case "park":
          return { text: "Parking: the players are warned, then the machine stops.", eventId: (await hosts.setServerHostDesired({ serverId: id, desired: "parked", reason })).eventId };
        case "reboot":
          return { text: "Rebooting: the players are warned, then the machine stops and starts again.", eventId: (await hosts.rebootServerHost({ serverId: id, force: fields.force, reason })).eventId };
        case "expect": {
          const until = timestampFromDate(new Date(Date.now() + fields.hours * 3_600_000));
          const r = await hosts.setServerHostExpectation({ serverId: id, players: fields.players, until, reason });
          return { text: fields.players > 0 ? `Expecting ${fields.players} players for ${fields.hours} h.` : "The expectation is cleared.", eventId: r.eventId };
        }
        case "resize":
          return { text: `Resizing to ${fields.size}${fields.pin ? ", pinned" : ""}.`, eventId: (await hosts.resizeServerHost({ serverId: id, size: fields.size, pin: fields.pin, force: fields.force, reason })).eventId };
        case "unpin":
          return { text: "The scaling policy sizes the host again.", eventId: (await hosts.unpinServerHostSize({ serverId: id, reason })).eventId };
        case "snapshot": {
          const r = await hosts.snapshotServerHostVolume({ serverId: id, name: fields.name.trim(), reason });
          return { text: `Snapshot ${r.snapshot?.name ?? ""} taken.`, eventId: r.eventId };
        }
        case "destroy":
          return {
            text: `Destroying the host${fields.deleteVolume ? ", its volume" : ""}${fields.releaseAddress ? ", its address" : ""}.`,
            eventId: (await hosts.destroyServerHost({ serverId: id, deleteVolume: fields.deleteVolume, releaseAddress: fields.releaseAddress, reason })).eventId,
          };
      }
    },
    onSuccess: async (r) => {
      done({ ok: true, ...r });
      setOpen("");
      await qc.invalidateQueries({ queryKey: ["host", id] });
    },
    onError: (err) => {
      done({ ok: false, text: "The hub refused: " + message(err) });
    },
  });
  if (offer.length === 0) {
    const moving = !["running", "parked", "absent"].includes(host.state);
    return (
      <p className="mb-6 text-muted">
        {moving ? `The host is ${host.state}: actions return when it settles.` : "Your roles give you no actions on this server."}
      </p>
    );
  }
  return (
    <section aria-labelledby="actions" className="mb-6">
      <h2 id="actions" className="mb-2 text-xl font-bold">
        Actions
      </h2>
      <div className="mb-3 flex flex-wrap gap-2">
        {offer.map((a) => (
          <button key={a} type="button" className={button} aria-expanded={open === a} disabled={run.isPending} onClick={() => { setOpen(open === a ? "" : a); }}>
            {labels[a]}
          </button>
        ))}
      </div>
      {open !== "" && (
        <ActionForm
          key={open}
          act={open}
          id={id}
          host={host}
          busy={run.isPending}
          cancel={() => { setOpen(""); }}
          submit={(fields) => { run.mutate({ act: open, fields }); }}
        />
      )}
    </section>
  );
}

interface Fields {
  reason: string;
  force: boolean;
  players: number;
  hours: number;
  size: string;
  pin: boolean;
  name: string;
  deleteVolume: boolean;
  releaseAddress: boolean;
}

// ActionForm is one action's form: what it needs, a reason, and for a provisioning action the
// server's id typed to confirm.
function ActionForm({ act, id, host, busy, cancel, submit }: { act: Action; id: string; host: ServerHost; busy: boolean; cancel: () => void; submit: (f: Fields) => void }) {
  const sizes = host.sizes.filter((z) => z.slug !== host.size || act !== "resize");
  const [f, setF] = useState<Fields>({
    reason: "",
    force: false,
    players: host.expectedPlayers || 10,
    hours: 3,
    size: sizes.find((z) => z.slug !== host.size)?.slug ?? host.size,
    pin: false,
    name: "",
    deleteVolume: false,
    releaseAddress: false,
  });
  const [typed, setTyped] = useState("");
  const typedOK = !provisioning.includes(act) || typed.trim() === id;
  const on = (e: SubmitEvent<HTMLFormElement>) => {
    e.preventDefault();
    if (typedOK) {
      submit(f);
    }
  };
  const set = (patch: Partial<Fields>) => { setF({ ...f, ...patch }); };
  return (
    <form onSubmit={on} aria-label={labels[act]} className="grid gap-3 rounded border-2 border-warn bg-surface p-3">
      <p>{explain[act]}</p>
      {act === "reboot" && (
        <Check label="Even with players on (they are warned first)" checked={f.force} set={(v) => { set({ force: v }); }} />
      )}
      {act === "expect" && (
        <div className="grid gap-3 sm:grid-cols-2">
          <label>
            Players (0 clears it)
            <input className={field} type="number" min={0} max={1000} value={f.players} onChange={(e) => { set({ players: Number(e.target.value) }); }} />
          </label>
          <label>
            For how many hours
            <input className={field} type="number" min={1} max={72} value={f.hours} onChange={(e) => { set({ hours: Number(e.target.value) }); }} />
          </label>
        </div>
      )}
      {act === "resize" && (
        <>
          <label>
            Size
            <select className={field} value={f.size} onChange={(e) => { set({ size: e.target.value }); }}>
              {host.sizes.map((z) => (
                <option key={z.slug} value={z.slug}>
                  {z.slug} · {z.vcpus} vCPU · {Math.round(z.memoryMib / 1024)} GB · ${z.priceHourlyUsd.toFixed(3)}/h
                </option>
              ))}
            </select>
          </label>
          <Check label="Pin it: the scaling policy leaves it at this size" checked={f.pin} set={(v) => { set({ pin: v }); }} />
          <Check label="Even with players on (they are warned first)" checked={f.force} set={(v) => { set({ force: v }); }} />
        </>
      )}
      {act === "snapshot" && (
        <label>
          Snapshot name (empty for the default)
          <input className={field} value={f.name} onChange={(e) => { set({ name: e.target.value }); }} />
        </label>
      )}
      {act === "destroy" && (
        <>
          <Check label="Delete the data volume too (the game's files and its data are gone)" checked={f.deleteVolume} set={(v) => { set({ deleteVolume: v }); }} />
          <Check label="Release the reserved address too" checked={f.releaseAddress} set={(v) => { set({ releaseAddress: v }); }} />
        </>
      )}
      <label>
        Reason (kept in the audit log)
        <input className={field} maxLength={200} value={f.reason} onChange={(e) => { set({ reason: e.target.value }); }} />
      </label>
      {provisioning.includes(act) && (
        <label>
          Type <span className="font-mono">{id}</span> to confirm
          <input className={field} autoComplete="off" value={typed} onChange={(e) => { setTyped(e.target.value); }} />
        </label>
      )}
      <div className="flex flex-wrap gap-2">
        <button type="submit" className={primary} disabled={busy || !typedOK}>
          {labels[act]}
        </button>
        <button type="button" className={button} onClick={cancel}>
          Cancel
        </button>
      </div>
    </form>
  );
}

const explain: Record<Action, string> = {
  start: "Start the host: the hub makes or wakes the machine and starts the game.",
  park: "Park the host: the players are warned, the game stops and the machine is parked; it costs less while parked.",
  reboot: "Reboot the machine: the players are warned, then it stops and starts again, with the same disk and address.",
  expect: "Tell the host how many players to expect: the scaling policy sizes for them and an empty host isn't parked meanwhile.",
  resize: "Move the host to another size now: the game stops, the machine is resized and started again.",
  unpin: "Let the scaling policy size the host again.",
  snapshot: "Copy the parked host's data volume into a snapshot a new server can start from.",
  destroy: "Remove the parked host's machine for good. The server stays; starting it again makes a new machine.",
};

function Check({ label, checked, set }: { label: string; checked: boolean; set: (v: boolean) => void }) {
  return (
    <label className="flex items-center gap-2">
      <input type="checkbox" checked={checked} onChange={(e) => { set(e.target.checked); }} />
      {label}
    </label>
  );
}

// Lease is the server's live lease, if any, and its release.
function Lease({ id, done }: { id: string; done: (o: Outcome) => void }) {
  const leases = useClient(ServerLeaseService);
  const qc = useQueryClient();
  const [asking, setAsking] = useState(false);
  const live = useQuery({ queryKey: ["lease", id], queryFn: ({ signal }) => leases.listServerLeases({ serverId: id, liveOnly: true, limit: 1 }, { signal }) });
  const release = useMutation({
    mutationFn: (leaseId: string) => leases.releaseServerLease({ leaseId, reason: "released from the dashboard" }),
    onSuccess: async (r) => {
      done({ ok: true, text: "The lease is released: the server is given back and parks.", eventId: r.eventId });
      setAsking(false);
      await qc.invalidateQueries({ queryKey: ["lease", id] });
      await qc.invalidateQueries({ queryKey: ["host", id] });
    },
    onError: (err) => {
      done({ ok: false, text: "The hub refused: " + message(err) });
      setAsking(false);
    },
  });
  const lease = live.data?.leases[0];
  if (!lease) {
    return null;
  }
  return (
    <section aria-labelledby="lease" className="mb-6">
      <h2 id="lease" className="mb-2 text-xl font-bold">
        Lease
      </h2>
      <p className="mb-2">
        Held by {lease.holder}
        {lease.pool !== "" && ` from pool ${lease.pool}`} · {lease.state}
        {lease.createdAt && ` since ${utc(timestampDate(lease.createdAt))}`}
      </p>
      {asking ? (
        <div role="group" aria-label="Release the lease" className="flex flex-wrap items-center gap-2 rounded border-2 border-warn p-2">
          <p className="w-full">Release the lease? The match on it ends, and the server parks.</p>
          <button type="button" className={primary} disabled={release.isPending} onClick={() => { release.mutate(lease.id); }}>
            Yes, release it
          </button>
          <button type="button" className={button} onClick={() => { setAsking(false); }}>
            Cancel
          </button>
        </div>
      ) : (
        <button type="button" className={button} onClick={() => { setAsking(true); }}>
          Release the lease
        </button>
      )}
    </section>
  );
}
