// Game stats (gravel#206, ADR-0033): a game's board with the stats its Game spec declares beside the
// common ones, and its latest match results. stats:read, or anyone when stats are public. Nothing
// here knows a game: the columns are the game's layout and the stat extensions the hub returns.
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { useQuery } from "@tanstack/react-query";
import { useState } from "react";

import { OrganizationService } from "../gen/gravel/hub/v1/organization_pb";
import { type Game, ServerService, type StatExtension } from "../gen/gravel/hub/v1/servers_pb";
import { type BoardEntry, StatsService } from "../gen/gravel/hub/v1/stats_pb";
import { useClient } from "../transport";

const button = "rounded border border-line bg-surface px-3 py-1";
const primary = "rounded border-2 border-accent bg-surface px-3 py-1 font-bold";

const windows = [
  ["week", "This week"],
  ["month", "This month"],
  ["all", "All time"],
] as const;

// The common stats a board can show and rank by (GetBoardRequest.metric), with their headings.
const common: Record<string, string> = { kills: "Kills", deaths: "Deaths", kd: "K/D", time: "Time on", matches: "Matches" };
const defaultColumns = ["kills", "deaths", "kd", "time", "matches"];

// MaxEntries is how many players the board shows.
const maxEntries = 50;

// columnsOf are a game's board columns: its layout's, or every common stat.
export function columnsOf(g: Game | undefined): string[] {
  const own = (g?.layout?.board ?? []).filter((c) => c in common);
  return own.length > 0 ? own : defaultColumns;
}

// heading is an extension's column heading: its label, and its unit if any.
export function heading(e: StatExtension): string {
  return e.unit ? `${e.label} (${e.unit})` : e.label;
}

// statValue is one of a game's own stats as the board shows it: a whole number for an int that is
// a total or a best, one decimal for a float or an average; undefined when the player has none.
export function statValue(e: StatExtension, values: Record<string, number>): string | undefined {
  const v = values[e.name];
  if (v === undefined) {
    return undefined;
  }
  const decimals = e.type === "float" || e.aggregate === "avg" ? 1 : 0;
  return new Intl.NumberFormat("en-US", { minimumFractionDigits: decimals, maximumFractionDigits: decimals }).format(v);
}

function commonValue(c: string, e: BoardEntry): string {
  switch (c) {
    case "kills":
      return String(e.kills);
    case "deaths":
      return String(e.deaths);
    case "kd":
      return e.kd.toFixed(2);
    case "time":
      return `${(e.secondsOn / 3600).toFixed(1)} h`;
    default:
      return String(e.matches);
  }
}

export function Stats() {
  const servers = useClient(ServerService);
  const games = useQuery({ queryKey: ["games"], queryFn: ({ signal }) => servers.listGames({}, { signal }), staleTime: Infinity, refetchInterval: false });
  const [picked, setPicked] = useState("");
  // A game whose stats make no combat board (a server-less game's participation, gravel#2) has
  // nothing to rank here.
  const all = games.data?.games ?? [];
  const list = all.filter((g) => g.combatBoards);
  const game = list.find((g) => g.id === picked) ?? list[0];
  return (
    <>
      <div className="mb-4 flex flex-wrap items-baseline justify-between gap-3">
        <h1 className="text-2xl font-bold">Game stats</h1>
        <Visibility />
      </div>
      {games.error ? (
        <p role="alert">The hub did not answer: {ConnectError.from(games.error).rawMessage}.</p>
      ) : !games.data ? (
        <p role="status" className="text-muted">
          Loading the games…
        </p>
      ) : all.length === 0 ? (
        <p>This hub runs no game yet: name one in servers.yaml.</p>
      ) : !game ? (
        <p>This hub’s games have no combat boards: their stats are their communities’ participation, on each game’s page.</p>
      ) : (
        <>
          {list.length > 1 && (
            <label className="mb-4 flex flex-wrap items-center gap-2">
              Game
              <select className="rounded border border-line bg-surface px-2 py-1" value={game.id} onChange={(e) => { setPicked(e.target.value); }}>
                {list.map((g) => (
                  <option key={g.id} value={g.id}>
                    {g.name}
                  </option>
                ))}
              </select>
            </label>
          )}
          <Board key={game.id} game={game} />
        </>
      )}
    </>
  );
}

// Visibility says who can read the boards: the Organization settings' stats.public.
function Visibility() {
  const org = useClient(OrganizationService);
  const q = useQuery({ queryKey: ["settings"], queryFn: ({ signal }) => org.getOrganizationSettings({}, { signal }), refetchInterval: false });
  if (!q.data) {
    return null;
  }
  const open = q.data.settings?.stats?.public ?? false;
  return (
    <p className={`rounded border-l-4 pl-2 ${open ? "border-ok" : "border-line"}`}>
      {open ? "Public: anyone can read the boards and results" : "Private: the boards need stats:read"}{" "}
      <a href="/admin/settings" className="underline">
        (settings)
      </a>
    </p>
  );
}

function Board({ game }: { game: Game }) {
  const api = useClient(StatsService);
  const columns = columnsOf(game);
  const [span, setSpan] = useState<string>("all");
  const [metric, setMetric] = useState<string>(columns[0] ?? "kills");
  const q = useQuery({
    queryKey: ["board", game.id, span, metric],
    queryFn: ({ signal }) => api.getBoard({ gameId: game.id, window: span, metric, pageSize: maxEntries }, { signal }),
    refetchInterval: 60_000,
    staleTime: 55_000,
  });
  const exts = q.data?.statExtensions ?? game.statExtensions;
  return (
    <div className="grid min-w-0 gap-6">
      <section aria-labelledby="board" className="grid min-w-0 gap-3">
        <div className="flex flex-wrap items-baseline justify-between gap-3">
          <h2 id="board" className="text-xl font-bold">
            {game.name}: the board
          </h2>
          <div className="flex flex-wrap items-center gap-3">
            <div role="group" aria-label="Window" className="flex flex-wrap gap-2">
              {windows.map(([key, label]) => (
                <button key={key} type="button" className={key === span ? primary : button} aria-pressed={key === span} onClick={() => { setSpan(key); }}>
                  {label}
                </button>
              ))}
            </div>
            <label className="flex items-center gap-2">
              Rank by
              <select className="rounded border border-line bg-surface px-2 py-1" value={metric} onChange={(e) => { setMetric(e.target.value); }}>
                {columns.map((c) => (
                  <option key={c} value={c}>
                    {common[c]}
                  </option>
                ))}
              </select>
            </label>
          </div>
        </div>
        <p className="text-muted">
          Official servers&rsquo; matches{exts.length > 0 ? `; ${game.name}’s own stats come from its match results, each a total over the player’s maps` : ""}. A dash is a
          stat the player has no result for.
        </p>
        {q.error ? (
          <p role={ConnectError.from(q.error).code === Code.PermissionDenied ? undefined : "alert"}>
            {ConnectError.from(q.error).code === Code.PermissionDenied
              ? "The boards are private: they need stats:read."
              : `The hub did not answer: ${ConnectError.from(q.error).rawMessage}.`}
          </p>
        ) : !q.data ? (
          <p role="status" className="text-muted">
            Loading the board…
          </p>
        ) : q.data.entries.length === 0 ? (
          <p>No one has played {game.name} in this window.</p>
        ) : (
          <div className="relative min-w-0 overflow-x-auto" tabIndex={0} role="region" aria-label={`${game.name}: the board, the table`}>
            <table className="w-full text-left">
              <thead>
                <tr className="border-b border-line">
                  <th scope="col" className="px-2 text-right">
                    Rank
                  </th>
                  <th scope="col" className="px-2">
                    Player
                  </th>
                  {columns.map((c) => (
                    <th key={c} scope="col" className="px-2 text-right" aria-sort={c === metric ? "descending" : undefined}>
                      {common[c]}
                    </th>
                  ))}
                  {exts.map((e) => (
                    <th key={e.name} scope="col" className="px-2 text-right">
                      {heading(e)}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {q.data.entries.map((e) => (
                  <tr key={`${String(e.rank)}-${e.name}`} className="border-b border-line">
                    <td className="px-2 text-right">{e.rank}</td>
                    <td className="px-2">{e.userId !== "" ? <a href={`/members/${e.userId}`}>{e.name}</a> : e.name}</td>
                    {columns.map((c) => (
                      <td key={c} className="whitespace-nowrap px-2 text-right">
                        {commonValue(c, e)}
                      </td>
                    ))}
                    {exts.map((x) => {
                      const v = statValue(x, e.extensions);
                      return (
                        <td key={x.name} className="px-2 text-right">
                          {v ?? (
                            <>
                              <span aria-hidden="true">–</span>
                              <span className="sr-only">not reported</span>
                            </>
                          )}
                        </td>
                      );
                    })}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
      <Results game={game} />
    </div>
  );
}

// Results are the game's latest finished match results, each linking to its page.
function Results({ game }: { game: Game }) {
  const api = useClient(StatsService);
  const servers = useClient(ServerService);
  const list = useQuery({ queryKey: ["servers"], queryFn: ({ signal }) => servers.listServers({}, { signal }), staleTime: Infinity, refetchInterval: false });
  const q = useQuery({
    queryKey: ["results"],
    queryFn: ({ signal }) => api.listMatchResults({ endedOnly: true, pageSize: 50 }, { signal }),
    refetchInterval: 60_000,
    staleTime: 55_000,
  });
  if (!list.data || !q.data) {
    return null;
  }
  const mine = new Map(list.data.servers.filter((s) => s.gameId === game.id).map((s) => [s.id, s.name]));
  const results = q.data.results.filter((r) => mine.has(r.serverId)).slice(0, 10);
  if (results.length === 0) {
    return null;
  }
  const date = (d: Date) => new Intl.DateTimeFormat("en-GB", { day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" }).format(d);
  return (
    <section aria-labelledby="results" className="grid min-w-0 gap-2">
      <h2 id="results" className="text-xl font-bold">
        Latest results
      </h2>
      <ul className="grid gap-1">
        {results.map((r) => (
          <li key={r.id}>
            <a href={`/results/${r.id}`} className="underline">
              {r.team1Name || "Team 1"} {r.team1Score}–{r.team2Score} {r.team2Name || "Team 2"}
            </a>{" "}
            <span className="text-muted">
              on {mine.get(r.serverId)}
              {r.endedAt ? `, ${date(timestampDate(r.endedAt))}` : ""}
            </span>
          </li>
        ))}
      </ul>
    </section>
  );
}
