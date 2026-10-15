// Discord in the settings form (gravel#205, #253): the server and its role mapping, then each stock
// module's part of the `discord` section, so every Discord setting is edited without the YAML tab.
// Servers and games are picked from the hub's own lists; a value the hub doesn't know stays as it
// is, named as such. The hub checks the document on save and its messages land under the fields.
import { create } from "@bufbuild/protobuf";
import { useQuery } from "@tanstack/react-query";
import { useState } from "react";

import {
  type DiscordSeedingHours,
  DiscordMatchResultsSchema,
  DiscordNewsFeedSchema,
  DiscordNewsSchema,
  DiscordPresenceGameSchema,
  DiscordPresenceSchema,
  DiscordRecapSchema,
  DiscordRecognitionSchema,
  DiscordRolesSchema,
  DiscordSeedingHoursSchema,
  DiscordSeedingSchema,
  DiscordServerCardSchema,
  type DiscordSettings,
  DiscordSettingsSchema,
  DiscordStreamerSchema,
  DiscordStreamsSchema,
  type OrganizationSettings,
} from "../gen/gravel/hub/v1/organization_pb";
import { ServerService } from "../gen/gravel/hub/v1/servers_pb";
import { useClient } from "../transport";
import { Add, Check, Entry, type Err, Field, field, ListInput, Message, Part, Section } from "./form";

type Edit = (change: (s: OrganizationSettings) => void) => void;

// The hub's bounds (internal/org/settings.go), so Add stops where the hub would refuse.
const max = { recognition: 10, cards: 10, seeding: 10, recaps: 10, games: 20, streamers: 10, feeds: 20, windows: 14 };

const days = ["mon", "tue", "wed", "thu", "fri", "sat", "sun"] as const;
const dayNames: Record<string, string> = { mon: "Mon", tue: "Tue", wed: "Wed", thu: "Thu", fri: "Fri", sat: "Sat", sun: "Sun" };

// The providers a Discord role may follow (org.MappedProviders).
const providers = [
  ["steam", "Steam"],
  ["discord", "Discord"],
] as const;

const once = { staleTime: Infinity, refetchInterval: false } as const;

export function Discord({ draft, edit, err, errIn }: { draft: OrganizationSettings; edit: Edit; err: Err; errIn: Err }) {
  const api = useClient(ServerService);
  const servers = useQuery({ queryKey: ["servers"], queryFn: ({ signal }) => api.listServers({}, { signal }), ...once });
  const games = useQuery({ queryKey: ["games"], queryFn: ({ signal }) => api.listGames({}, { signal }), ...once });
  // gen remounts the lists after a removal, so an item's typed text never moves to its neighbour.
  const [gen, setGen] = useState(0);
  const d = draft.discord ?? create(DiscordSettingsSchema, {});
  const setD = (change: (x: DiscordSettings) => void) => {
    edit((s) => {
      const x = s.discord ?? create(DiscordSettingsSchema, {});
      change(x);
      s.discord = x;
    });
  };
  const removeFrom = (change: (x: DiscordSettings) => void) => {
    setD(change);
    setGen((g) => g + 1);
  };
  const known = (servers.data?.servers ?? []).map((s) => ({ id: s.id, name: s.name }));
  const zone = draft.stats?.timezone || "UTC";
  const k = (i: number) => `${String(gen)}-${String(i)}`;
  const roles = d.roles ?? create(DiscordRolesSchema, {});
  const presence = d.presence ?? create(DiscordPresenceSchema, {});
  const streams = d.streams ?? create(DiscordStreamsSchema, {});
  const news = d.news ?? create(DiscordNewsSchema, {});
  const results = d.matchResults ?? create(DiscordMatchResultsSchema, {});
  return (
    <Section id="discord" title="Discord">
      {d.guildId === "" && (
        <p>
          Discord isn’t connected. Put your server’s id below (Discord, Server Settings, Widget, or right-click the server with Developer Mode on), then run gravel’s bot with
          its app’s credentials (gravel-hub apps create, then docs/bot.md). Everything here works without it.
        </p>
      )}
      <p className="text-sm text-muted">Every id here is a Discord id: with Developer Mode on, right-click a server, channel or role and copy its id.</p>
      <div className="grid items-start gap-3 sm:grid-cols-2 lg:grid-cols-3">
        <Field id="discord-guild" label="Server (guild) id" error={err("discord.guild_id")}>
          <Snowflake id="discord-guild" value={d.guildId} set={(v) => { setD((x) => { x.guildId = v; }); }} />
        </Field>
        <Field id="discord-modlog" label="Mod-log channel id" error={err("discord.mod_log")} hint="Every moderation action is posted here">
          <Snowflake id="discord-modlog" value={d.modLog} set={(v) => { setD((x) => { x.modLog = v; }); }} />
        </Field>
        <Field id="discord-linked" label="Role for members who linked an account" error={err("discord.roles.linked")} hint="Any account besides Discord">
          <Snowflake
            id="discord-linked"
            value={roles.linked}
            set={(v) => { setD((x) => { x.roles = { ...(x.roles ?? create(DiscordRolesSchema, {})), linked: v }; }); }}
          />
        </Field>
        {providers.map(([p, name]) => (
          <Field key={p} id={`discord-provider-${p}`} label={`Role for members who linked ${name}`} error={err(`discord.roles.providers.${p}`)}>
            <Snowflake
              id={`discord-provider-${p}`}
              value={roles.providers[p] ?? ""}
              set={(v) => {
                setD((x) => {
                  const r = x.roles ?? create(DiscordRolesSchema, {});
                  // An emptied field drops its provider, so the document keeps no empty mapping.
                  const next = Object.fromEntries(Object.entries(r.providers).filter(([q]) => q !== p));
                  x.roles = { ...r, providers: v === "" ? next : { ...next, [p]: v } };
                });
              }}
            />
          </Field>
        ))}
      </div>

      <Part id="discord-recognition" title="Recognition" intro="Roles earned once and kept: role sync adds them and never takes them away." error={err("discord.recognition")}>
        {d.recognition.map((r, i) => {
          const p = `discord.recognition[${String(i)}]`;
          const id = (f: string) => `recognition-${String(i)}-${f}`;
          return (
            <Entry key={k(i)} legend={`Recognition ${String(i + 1)}`} error={err(p)} remove={() => { removeFrom((x) => { x.recognition.splice(i, 1); }); }} removeLabel="Remove this role">
              <div className="grid items-start gap-3 sm:grid-cols-3">
                <Field id={id("role")} label="Role id" error={err(p + ".role")}>
                  <Snowflake id={id("role")} value={r.role} set={(v) => { setD((x) => { const y = x.recognition[i]; if (y) { y.role = v; } }); }} />
                </Field>
                <Field id={id("rule")} label="Earned by" error={err(p + ".rule")}>
                  <select id={id("rule")} className={field} value={r.rule} onChange={(e) => { setD((x) => { const y = x.recognition[i]; if (y) { y.rule = e.target.value; } }); }}>
                    {r.rule !== "first_members" && <option value={r.rule}>{r.rule || "Choose a rule"}</option>}
                    <option value="first_members">The first members to register</option>
                  </select>
                </Field>
                <Field id={id("count")} label="How many" error={err(p + ".count")}>
                  <Count id={id("count")} value={r.count} set={(v) => { setD((x) => { const y = x.recognition[i]; if (y) { y.count = v; } }); }} />
                </Field>
              </div>
            </Entry>
          );
        })}
        <Add label="Add a recognition role" count={d.recognition.length} max={max.recognition} add={() => { setD((x) => { x.recognition.push(create(DiscordRecognitionSchema, { rule: "first_members", count: 100 })); }); }} />
      </Part>

      <Part id="discord-cards" title="Server cards" intro="A live status card per server: one message the bot edits in place." error={err("discord.server_cards")}>
        {d.serverCards.map((c, i) => {
          const p = `discord.server_cards[${String(i)}]`;
          const id = (f: string) => `card-${String(i)}-${f}`;
          const set = (change: (y: typeof c) => void) => { setD((x) => { const y = x.serverCards[i]; if (y) { change(y); } }); };
          return (
            <Entry key={k(i)} legend={`Card for ${c.server || "a server"}`} error={err(p)} remove={() => { removeFrom((x) => { x.serverCards.splice(i, 1); }); }} removeLabel="Remove this card">
              <div className="grid items-start gap-3 sm:grid-cols-2">
                <Field id={id("server")} label="Server" error={err(p + ".server")}>
                  <ServerPicker id={id("server")} servers={known} value={c.server} set={(v) => { set((y) => { y.server = v; }); }} />
                </Field>
                <Field id={id("channel")} label="Channel id" error={err(p + ".channel")}>
                  <Snowflake id={id("channel")} value={c.channel} set={(v) => { set((y) => { y.channel = v; }); }} />
                </Field>
              </div>
              <Note id={id("note")} value={c.note} error={err(p + ".note")} set={(v) => { set((y) => { y.note = v; }); }} hint="Under the numbers: “Matches start at 20 players.”" />
            </Entry>
          );
        })}
        <Add label="Add a server card" count={d.serverCards.length} max={max.cards} add={() => { setD((x) => { x.serverCards.push(create(DiscordServerCardSchema, {})); }); }} />
      </Part>

      <Part
        id="discord-seeding"
        title="Seeding"
        intro={`Inside a rule’s weekly hours (${zone}), while its server is up and under its floor, the bot pings an opt-in seeder role, at most once per cooldown.`}
        error={err("discord.seeding")}
      >
        {d.seeding.map((r, i) => {
          const p = `discord.seeding[${String(i)}]`;
          const id = (f: string) => `seeding-${String(i)}-${f}`;
          const set = (change: (y: typeof r) => void) => { setD((x) => { const y = x.seeding[i]; if (y) { change(y); } }); };
          return (
            <Entry key={k(i)} legend={`Seeding for ${r.server || "a server"}`} error={err(p)} remove={() => { removeFrom((x) => { x.seeding.splice(i, 1); }); }} removeLabel="Remove this rule">
              <div className="grid items-start gap-3 sm:grid-cols-2 lg:grid-cols-3">
                <Field id={id("server")} label="Server" error={err(p + ".server")}>
                  <ServerPicker id={id("server")} servers={known} value={r.server} set={(v) => { set((y) => { y.server = v; }); }} />
                </Field>
                <Field id={id("channel")} label="Channel id" error={err(p + ".channel")}>
                  <Snowflake id={id("channel")} value={r.channel} set={(v) => { set((y) => { y.channel = v; }); }} />
                </Field>
                <Field id={id("role")} label="Seeder role id" error={err(p + ".role")} hint="An opt-in role, never @everyone">
                  <Snowflake id={id("role")} value={r.role} set={(v) => { set((y) => { y.role = v; }); }} />
                </Field>
                <Field id={id("below")} label="Call while players are under" error={err(p + ".below")} hint="2 to 1000: the game’s floor">
                  <Count id={id("below")} value={r.below} set={(v) => { set((y) => { y.below = v; }); }} />
                </Field>
                <Field id={id("cooldown")} label="Least time between calls" error={err(p + ".cooldown")} hint="15m to 24h, as 2h or 90m; empty is 2h">
                  <input id={id("cooldown")} className={field} value={r.cooldown} onChange={(e) => { set((y) => { y.cooldown = e.target.value.trim(); }); }} />
                </Field>
              </div>
              <Check id={id("start")} label="Call at the start of each window, without waiting out a cooldown" checked={r.callAtStart} set={(v) => { set((y) => { y.callAtStart = v; }); }} />
              <Note id={id("note")} value={r.note} error={err(p + ".note")} set={(v) => { set((y) => { y.note = v; }); }} hint="Appended to the call: how to join." />
              <Windows id={id("hours")} path={p + ".hours"} zone={zone} value={r.hours} err={err} errIn={errIn} set={(change) => { set((y) => { change(y.hours); }); }} />
            </Entry>
          );
        })}
        <Add label="Add a seeding rule" count={d.seeding.length} max={max.seeding} add={() => { setD((x) => { x.seeding.push(create(DiscordSeedingSchema, { below: 20 })); }); }} />
      </Part>

      <Part
        id="discord-recaps"
        title="Session recaps"
        intro={`After each of a rule’s weekly sessions (${zone}), the bot posts what its server’s session held. Nothing is posted when nobody came.`}
        error={err("discord.recaps")}
      >
        {d.recaps.map((r, i) => {
          const p = `discord.recaps[${String(i)}]`;
          const id = (f: string) => `recap-${String(i)}-${f}`;
          const set = (change: (y: typeof r) => void) => { setD((x) => { const y = x.recaps[i]; if (y) { change(y); } }); };
          return (
            <Entry key={k(i)} legend={`Recap for ${r.server || "a server"}`} error={err(p)} remove={() => { removeFrom((x) => { x.recaps.splice(i, 1); }); }} removeLabel="Remove this rule">
              <div className="grid items-start gap-3 sm:grid-cols-2 lg:grid-cols-4">
                <Field id={id("server")} label="Server" error={err(p + ".server")}>
                  <ServerPicker id={id("server")} servers={known} value={r.server} set={(v) => { set((y) => { y.server = v; }); }} />
                </Field>
                <Field id={id("channel")} label="Channel id" error={err(p + ".channel")}>
                  <Snowflake id={id("channel")} value={r.channel} set={(v) => { set((y) => { y.channel = v; }); }} />
                </Field>
                <Field id={id("title")} label="Session name" error={err(p + ".title")} hint="Its name in the post: “Wardogs Wednesday”">
                  <input id={id("title")} className={field} value={r.title} onChange={(e) => { set((y) => { y.title = e.target.value; }); }} />
                </Field>
                <Field id={id("floor")} label="Player floor" error={err(p + ".floor")} hint="For “minutes at or above”; 0 leaves it out">
                  <Count id={id("floor")} value={r.floor} set={(v) => { set((y) => { y.floor = v; }); }} />
                </Field>
              </div>
              <Note id={id("note")} value={r.note} error={err(p + ".note")} set={(v) => { set((y) => { y.note = v; }); }} hint="Appended to the post." />
              <Windows id={id("hours")} path={p + ".hours"} zone={zone} value={r.hours} err={err} errIn={errIn} set={(change) => { set((y) => { change(y.hours); }); }} />
            </Entry>
          );
        })}
        <Add label="Add a recap rule" count={d.recaps.length} max={max.recaps} add={() => { setD((x) => { x.recaps.push(create(DiscordRecapSchema, {})); }); }} />
      </Part>

      <Part
        id="discord-presence"
        title="Who’s playing now"
        intro="One card with how many members are playing each game, from Discord presence: counts only, never who. Leave everything empty for no card."
      >
        {(() => {
          const setP = (change: (y: typeof presence) => void) => { setD((x) => { const y = x.presence ?? create(DiscordPresenceSchema, {}); change(y); x.presence = y; }); };
          return (
            <>
              <div className="grid items-start gap-3 sm:grid-cols-2">
                <Field id="presence-channel" label="Channel id" error={err("discord.presence.channel")}>
                  <Snowflake id="presence-channel" value={presence.channel} set={(v) => { setP((y) => { y.channel = v; }); }} />
                </Field>
                <Note id="presence-note" value={presence.note} error={err("discord.presence.note")} set={(v) => { setP((y) => { y.note = v; }); }} hint="Under the counts." />
              </div>
              <Message text={err("discord.presence.games")} />
              <datalist id="presence-game-ids">
                {(games.data?.games ?? []).map((g) => (
                  <option key={g.id} value={g.id}>
                    {g.name}
                  </option>
                ))}
              </datalist>
              {presence.games.map((g, i) => {
                const p = `discord.presence.games[${String(i)}]`;
                const id = (f: string) => `presence-${String(i)}-${f}`;
                const set = (change: (y: typeof g) => void) => { setP((y) => { const z = y.games[i]; if (z) { change(z); } }); };
                return (
                  <Entry key={k(i)} legend={g.label || "A game"} error={err(p)} remove={() => { removeFrom((x) => { x.presence?.games.splice(i, 1); }); }} removeLabel="Remove this game">
                    <div className="grid items-start gap-3 sm:grid-cols-2">
                      <Field id={id("game")} label="Key" error={err(p + ".game")} hint="The hub’s game id where there is one">
                        <input id={id("game")} className={field} list="presence-game-ids" value={g.game} onChange={(e) => { set((z) => { z.game = e.target.value.trim(); }); }} />
                      </Field>
                      <Field id={id("label")} label="Name on the card" error={err(p + ".label")}>
                        <input id={id("label")} className={field} value={g.label} onChange={(e) => { set((z) => { z.label = e.target.value; }); }} />
                      </Field>
                      <Field id={id("activities")} label="Activity names (commas between)" error={errIn(p + ".activities")} hint="As Discord shows them: WARDOGS">
                        <ListInput id={id("activities")} value={g.activities} set={(v) => { set((z) => { z.activities = v; }); }} />
                      </Field>
                      <Field id={id("apps")} label="Application ids (commas between)" error={errIn(p + ".application_ids")}>
                        <ListInput id={id("apps")} value={g.applicationIds} set={(v) => { set((z) => { z.applicationIds = v; }); }} />
                      </Field>
                    </div>
                  </Entry>
                );
              })}
              <Add label="Add a game to the card" count={presence.games.length} max={max.games} add={() => { setP((y) => { y.games.push(create(DiscordPresenceGameSchema, {})); }); }} />
            </>
          );
        })()}
      </Part>

      <Part
        id="discord-streams"
        title="Go-live posts"
        intro="When a listed streamer goes live on Twitch or Kick, the bot posts once and edits the post when the stream ends. The bot needs each platform’s app credentials in bot.yaml. Leave everything empty for no posts."
      >
        {(() => {
          const setS = (change: (y: typeof streams) => void) => { setD((x) => { const y = x.streams ?? create(DiscordStreamsSchema, {}); change(y); x.streams = y; }); };
          return (
            <>
              <div className="grid items-start gap-3 sm:grid-cols-2 lg:grid-cols-3">
                <Field id="streams-channel" label="Channel id" error={err("discord.streams.channel")}>
                  <Snowflake id="streams-channel" value={streams.channel} set={(v) => { setS((y) => { y.channel = v; }); }} />
                </Field>
                <Field id="streams-role" label="Role to ping (optional)" error={err("discord.streams.role")} hint="An opt-in role, never @everyone">
                  <Snowflake id="streams-role" value={streams.role} set={(v) => { setS((y) => { y.role = v; }); }} />
                </Field>
                <Note id="streams-note" value={streams.note} error={err("discord.streams.note")} set={(v) => { setS((y) => { y.note = v; }); }} hint="Under each post." />
              </div>
              <Message text={err("discord.streams.streamers")} />
              {streams.streamers.map((st, i) => {
                const p = `discord.streams.streamers[${String(i)}]`;
                const id = (f: string) => `streamer-${String(i)}-${f}`;
                const set = (change: (y: typeof st) => void) => { setS((y) => { const z = y.streamers[i]; if (z) { change(z); } }); };
                return (
                  <Entry key={k(i)} legend={st.label || st.login || "A streamer"} error={err(p)} remove={() => { removeFrom((x) => { x.streams?.streamers.splice(i, 1); }); }} removeLabel="Remove this streamer">
                    <div className="grid items-start gap-3 sm:grid-cols-3">
                      <Field id={id("platform")} label="Platform" error={err(p + ".platform")}>
                        <select id={id("platform")} className={field} value={st.platform} onChange={(e) => { set((z) => { z.platform = e.target.value; }); }}>
                          {st.platform !== "twitch" && st.platform !== "kick" && <option value={st.platform}>{st.platform || "Choose a platform"}</option>}
                          <option value="twitch">Twitch</option>
                          <option value="kick">Kick</option>
                        </select>
                      </Field>
                      <Field id={id("login")} label="Channel name" error={err(p + ".login")} hint="As in its address, lower case">
                        <input id={id("login")} className={field} value={st.login} onChange={(e) => { set((z) => { z.login = e.target.value.trim(); }); }} />
                      </Field>
                      <Field id={id("label")} label="Name in the post (optional)" error={err(p + ".label")}>
                        <input id={id("label")} className={field} value={st.label} onChange={(e) => { set((z) => { z.label = e.target.value; }); }} />
                      </Field>
                    </div>
                  </Entry>
                );
              })}
              <Add label="Add a streamer" count={streams.streamers.length} max={max.streamers} add={() => { setS((y) => { y.streamers.push(create(DiscordStreamerSchema, { platform: "twitch" })); }); }} />
            </>
          );
        })()}
      </Part>

      <Part id="discord-news" title="Steam news" intro="Each feed posts one Steam game’s announcements into one channel." error={err("discord.news.feeds")}>
        {news.feeds.map((f, i) => {
          const p = `discord.news.feeds[${String(i)}]`;
          const id = (x: string) => `news-${String(i)}-${x}`;
          const set = (change: (y: typeof f) => void) => {
            setD((x) => { const n = x.news ?? create(DiscordNewsSchema, {}); const y = n.feeds[i]; if (y) { change(y); } x.news = n; });
          };
          return (
            <Entry key={k(i)} legend={f.label || "A news feed"} error={err(p)} remove={() => { removeFrom((x) => { x.news?.feeds.splice(i, 1); }); }} removeLabel="Remove this feed">
              <div className="grid items-start gap-3 sm:grid-cols-3">
                <Field id={id("app")} label="Steam app id" error={err(p + ".app_id")} hint="As in its store address: 730 is Counter-Strike 2">
                  <Count id={id("app")} value={f.appId} set={(v) => { set((y) => { y.appId = v; }); }} />
                </Field>
                <Field id={id("channel")} label="Channel id" error={err(p + ".channel")}>
                  <Snowflake id={id("channel")} value={f.channel} set={(v) => { set((y) => { y.channel = v; }); }} />
                </Field>
                <Field id={id("label")} label="Game’s name in the footer" error={err(p + ".label")}>
                  <input id={id("label")} className={field} value={f.label} onChange={(e) => { set((y) => { y.label = e.target.value; }); }} />
                </Field>
                <Field id={id("feeds")} label="Steam feeds (commas between)" error={errIn(p + ".feeds")} hint="Empty is the game’s own announcements">
                  <ListInput id={id("feeds")} value={f.feeds} set={(v) => { set((y) => { y.feeds = v; }); }} />
                </Field>
              </div>
              <Colour id={id("color")} value={f.color} error={err(p + ".color")} set={(v) => { set((y) => { y.color = v; }); }} />
            </Entry>
          );
        })}
        <Add
          label="Add a news feed"
          count={news.feeds.length}
          max={max.feeds}
          add={() => { setD((x) => { const n = x.news ?? create(DiscordNewsSchema, {}); n.feeds.push(create(DiscordNewsFeedSchema, {})); x.news = n; }); }}
        />
      </Part>

      <Part id="discord-results" title="Match results" intro="Each finished series a server’s match plugin reports, posted once. Leave the channel empty for no posts.">
        {(() => {
          const setR = (change: (y: typeof results) => void) => { setD((x) => { const y = x.matchResults ?? create(DiscordMatchResultsSchema, {}); change(y); x.matchResults = y; }); };
          const listed = [...known, ...results.servers.filter((s) => !known.some((x) => x.id === s)).map((s) => ({ id: s, name: "" }))];
          return (
            <>
              <Field id="results-channel" label="Channel id" error={err("discord.match_results.channel")}>
                <Snowflake id="results-channel" value={results.channel} set={(v) => { setR((y) => { y.channel = v; }); }} />
              </Field>
              <fieldset className="grid gap-1">
                <legend>Servers whose results are posted (none ticked is every server)</legend>
                {listed.length === 0 && <p className="text-sm text-muted">This hub has no servers yet.</p>}
                {listed.map((s) => (
                  <Check
                    key={s.id}
                    id={`results-server-${s.id}`}
                    label={s.name ? `${s.name} (${s.id})` : `${s.id} (not a server on this hub)`}
                    checked={results.servers.includes(s.id)}
                    set={(v) => { setR((y) => { y.servers = v ? [...y.servers, s.id] : y.servers.filter((x) => x !== s.id); }); }}
                  />
                ))}
                <Message text={errIn("discord.match_results.servers")} />
              </fieldset>
            </>
          );
        })()}
      </Part>
    </Section>
  );
}

// Snowflake is a Discord id typed in a text field (they are too long for a number).
function Snowflake({ id, value, set }: { id: string; value: string; set: (v: string) => void }) {
  return <input id={id} className={field} inputMode="numeric" autoComplete="off" value={value} onChange={(e) => { set(e.target.value.trim()); }} />;
}

// Count is a whole number; empty is 0.
function Count({ id, value, set }: { id: string; value: number; set: (v: number) => void }) {
  return <input id={id} className={field} type="number" min={0} value={value || ""} onChange={(e) => { set(Math.trunc(Number(e.target.value)) || 0); }} />;
}

function Note({ id, value, error, set, hint }: { id: string; value: string; error?: string | undefined; set: (v: string) => void; hint: string }) {
  return (
    <Field id={id} label="Note (optional, one line)" error={error} hint={hint}>
      <input id={id} className={field} maxLength={200} value={value} onChange={(e) => { set(e.target.value); }} />
    </Field>
  );
}

// ServerPicker chooses one of the hub's servers; a value the hub doesn't list stays, named as such.
function ServerPicker({ id, servers, value, set }: { id: string; servers: { id: string; name: string }[]; value: string; set: (v: string) => void }) {
  return (
    <select id={id} className={field} value={value} onChange={(e) => { set(e.target.value); }}>
      <option value="">Choose a server</option>
      {value !== "" && !servers.some((s) => s.id === value) && <option value={value}>{value} (not a server on this hub)</option>}
      {servers.map((s) => (
        <option key={s.id} value={s.id}>
          {s.name} ({s.id})
        </option>
      ))}
    </select>
  );
}

// Colour is a news feed's embed colour, or the bot's own (0).
function Colour({ id, value, error, set }: { id: string; value: number; error?: string | undefined; set: (v: number) => void }) {
  const hex = "#" + Math.max(0, value).toString(16).padStart(6, "0").slice(-6);
  return (
    <div className="grid gap-2">
      <Check id={id + "-own"} label="Its own colour, instead of Steam’s blue" checked={value !== 0} set={(v) => { set(v ? 0x1b2838 : 0); }} error={error} />
      {value !== 0 && (
        <Field id={id} label="Colour">
          <input id={id} className="h-9 w-20 rounded border border-line bg-surface" type="color" value={hex} onChange={(e) => { set(parseInt(e.target.value.slice(1), 16) || 1); }} />
        </Field>
      )}
    </div>
  );
}

// Windows are a rule's weekly hours: the days each starts on, and from and to (to before from ends
// the next day).
function Windows({ id, path, zone, value, err, errIn, set }: {
  id: string;
  path: string;
  zone: string;
  value: DiscordSeedingHours[];
  err: Err;
  errIn: Err;
  set: (change: (hours: DiscordSeedingHours[]) => void) => void;
}) {
  return (
    <fieldset className="grid gap-2">
      <legend className="font-bold">Weekly hours, in {zone}</legend>
      {value.length === 0 && <p className="text-sm text-muted">No hours yet: this rule never posts.</p>}
      <Message text={err(path)} />
      {value.map((w, j) => {
        const p = `${path}[${String(j)}]`;
        const wid = (f: string) => `${id}-${String(j)}-${f}`;
        const setW = (change: (y: DiscordSeedingHours) => void) => { set((h) => { const y = h[j]; if (y) { change(y); } }); };
        return (
          <div key={j} className="grid gap-2 rounded border border-line p-2">
            <Message text={err(p)} />
            <fieldset className="flex flex-wrap gap-x-3 gap-y-1">
              <legend className="text-sm">Days window {j + 1} starts on</legend>
              {days.map((day) => (
                <label key={day} htmlFor={wid(day)} className="flex items-center gap-1">
                  <input
                    id={wid(day)}
                    type="checkbox"
                    checked={w.days.includes(day)}
                    onChange={(e) => { setW((y) => { y.days = e.target.checked ? [...days.filter((x) => x === day || y.days.includes(x)), ...y.days.filter((x) => !(days as readonly string[]).includes(x))] : y.days.filter((x) => x !== day); }); }}
                  />
                  {dayNames[day]}
                </label>
              ))}
            </fieldset>
            <Message text={errIn(p + ".days")} />
            <div className="grid items-start gap-3 sm:grid-cols-3">
              <Field id={wid("from")} label={`Window ${String(j + 1)} from`} error={err(p + ".from")}>
                <input id={wid("from")} className={field} type="time" value={w.from} onChange={(e) => { setW((y) => { y.from = e.target.value; }); }} />
              </Field>
              <Field id={wid("to")} label={`Window ${String(j + 1)} to`} error={err(p + ".to")} hint="Before from: it ends the next day">
                <input id={wid("to")} className={field} type="time" value={w.to} onChange={(e) => { setW((y) => { y.to = e.target.value; }); }} />
              </Field>
              <div className="self-end">
                <button type="button" className="rounded border border-line bg-surface px-3 py-1" onClick={() => { set((h) => { h.splice(j, 1); }); }}>
                  Remove window {j + 1}
                </button>
              </div>
            </div>
          </div>
        );
      })}
      <Add label="Add a window" count={value.length} max={max.windows} add={() => { set((h) => { h.push(create(DiscordSeedingHoursSchema, { days: [], from: "18:00", to: "23:00" })); }); }} />
    </fieldset>
  );
}
