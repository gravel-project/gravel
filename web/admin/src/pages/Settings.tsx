// Settings (gravel#205): the Organization settings without a YAML file. A form for the notifiers
// (with a test send), the Discord server and its core mapping, the leaderboards' visibility and
// local accounts; a YAML tab for the whole document, the rest of Discord included. Every save writes
// the whole document (ADR-0007 §2) and names the one it replaces, so two editors don't overwrite
// each other; a refusal is shown under the field it is about.
import { clone, create } from "@bufbuild/protobuf";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { ConnectError } from "@connectrpc/connect";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, type SubmitEvent, useState } from "react";

import {
  DiscordRolesSchema,
  DiscordSettingsSchema,
  LocalAccountsSettingsSchema,
  type Notifier,
  NotifierEventsSchema,
  NotifierSchema,
  type OrganizationSettings,
  OrganizationSettingsSchema,
  OrganizationService,
  StatsSettingsSchema,
} from "../gen/gravel/hub/v1/organization_pb";
import { conflict, list, noProblems, type NotifierKind, notifierKinds, type Problems, problems } from "../settings";
import { useClient } from "../transport";
import { utc } from "./People";

const field = "block w-full rounded border border-line bg-surface px-2 py-1";
const button = "rounded border border-line bg-surface px-3 py-1 disabled:opacity-60";
// The main action: outlined in the accent, never filled with it (see People).
const primary = "rounded border-2 border-accent bg-surface px-3 py-1 font-bold disabled:opacity-60";

// formPaths are the fields the form shows a message under; any other path is listed at the top.
const formPath = /^(notifiers\[\d+\]\.(name|kind|url|flavour|topic|to|events\.min_severity|events\.kinds)|discord\.(guild_id|roles\.linked|mod_log)|stats\.public|local_accounts\.(registration|min_password_length))$/;

export function Settings() {
  const [tab, setTab] = useState<"form" | "yaml">("form");
  return (
    <>
      <h1 className="mb-4 text-2xl font-bold">Settings</h1>
      <div role="group" aria-label="Edit as" className="mb-4 flex gap-2">
        <button type="button" className={tab === "form" ? primary : button} aria-pressed={tab === "form"} onClick={() => { setTab("form"); }}>
          Form
        </button>
        <button type="button" className={tab === "yaml" ? primary : button} aria-pressed={tab === "yaml"} onClick={() => { setTab("yaml"); }}>
          YAML, the whole document
        </button>
      </div>
      {tab === "form" ? <Form /> : <Yaml />}
    </>
  );
}

// useSettings is the stored document, read once per visit: an editor's copy isn't refreshed under
// them (a change elsewhere is caught at save).
function useSettings() {
  const org = useClient(OrganizationService);
  return useQuery({
    queryKey: ["settings"],
    queryFn: async ({ signal }) => (await org.getOrganizationSettings({}, { signal })).settings ?? create(OrganizationSettingsSchema, {}),
    refetchInterval: false,
    staleTime: Infinity,
  });
}

function Form() {
  const q = useSettings();
  if (q.error) {
    return <p role="alert">The hub did not answer: {ConnectError.from(q.error).rawMessage}.</p>;
  }
  if (!q.data) {
    return (
      <p role="status" className="text-muted">
        Loading the settings…
      </p>
    );
  }
  // Keyed by when the document was written, so a reload after a conflict starts the form afresh.
  return <Editor key={q.data.updatedAt ? String(timestampDate(q.data.updatedAt).getTime()) : "new"} loaded={q.data} />;
}

function Editor({ loaded }: { loaded: OrganizationSettings }) {
  const org = useClient(OrganizationService);
  const qc = useQueryClient();
  const [draft, setDraft] = useState(() => clone(OrganizationSettingsSchema, loaded));
  const [found, setFound] = useState<Problems>(noProblems);
  const [stale, setStale] = useState(false);
  const save = useMutation({
    mutationFn: () => org.updateOrganizationSettings({ settings: draft, ...(loaded.updatedAt ? { ifUpdatedAt: loaded.updatedAt } : {}) }),
    onSuccess: async () => {
      setFound(noProblems);
      await qc.invalidateQueries({ queryKey: ["settings"] });
    },
    onError: (err) => {
      setStale(conflict(err));
      setFound(conflict(err) ? noProblems : problems(err, (p) => formPath.test(p)));
    },
  });
  // edit applies a change to a copy of the draft.
  const edit = (change: (s: OrganizationSettings) => void) => {
    const next = clone(OrganizationSettingsSchema, draft);
    change(next);
    setDraft(next);
  };
  const submit = (e: SubmitEvent<HTMLFormElement>) => {
    e.preventDefault();
    save.mutate();
  };
  const err = (path: string) => found.byPath.get(path);
  return (
    <form onSubmit={submit} aria-label="Settings" noValidate className="grid gap-6">
      {stale && (
        <div role="alert" className="rounded border-2 border-error bg-surface p-3">
          <p className="mb-2">Someone changed the settings since you opened this page, so nothing was saved. Load theirs to start again from them; your edits here are dropped.</p>
          <button type="button" className={button} onClick={() => { void qc.invalidateQueries({ queryKey: ["settings"] }); }}>
            Load the current settings
          </button>
        </div>
      )}
      {found.other.length > 0 && (
        <div role="alert" className="rounded border-2 border-error bg-surface p-3">
          <p className="mb-1">The hub refused the settings:</p>
          <ul className="list-disc pl-6">
            {found.other.map((m) => (
              <li key={m}>{m}</li>
            ))}
          </ul>
        </div>
      )}
      <Notifiers draft={draft} saved={loaded} edit={edit} err={err} />
      <DiscordCore draft={draft} edit={edit} err={err} />
      <Section id="stats" title="Leaderboards">
        <Check
          id="stats-public"
          label="Anyone may see the leaderboards and match lists, signed in or not"
          checked={draft.stats?.public ?? false}
          set={(v) => { edit((s) => { s.stats = { ...(s.stats ?? create(StatsSettingsSchema, {})), public: v }; }); }}
          error={err("stats.public")}
        />
      </Section>
      <Section id="local" title="Local accounts">
        <Field id="local-registration" label="Who may make an account" error={err("local_accounts.registration")}>
          <select
            id="local-registration"
            className={field}
            value={draft.localAccounts?.registration || "closed"}
            onChange={(e) => { edit((s) => { s.localAccounts = { ...(s.localAccounts ?? create(LocalAccountsSettingsSchema, {})), registration: e.target.value }; }); }}
          >
            <option value="closed">Staff make them (People, or gravel users create-local)</option>
            <option value="open">Anyone who reaches the login page</option>
          </select>
        </Field>
        <Field id="local-min" label="Shortest password, in characters (12 when empty)" error={err("local_accounts.min_password_length")}>
          <input
            id="local-min"
            className={field}
            type="number"
            min={8}
            max={128}
            value={draft.localAccounts?.minPasswordLength || ""}
            onChange={(e) => { edit((s) => { s.localAccounts = { ...(s.localAccounts ?? create(LocalAccountsSettingsSchema, {})), minPasswordLength: Number(e.target.value) || 0 }; }); }}
          />
        </Field>
      </Section>
      <div className="flex flex-wrap items-center gap-3">
        <button type="submit" className={primary} disabled={save.isPending}>
          Save the settings
        </button>
        {save.isSuccess && (
          <p role="status">Saved. Everyone sees the change at their next page.</p>
        )}
        {loaded.updatedAt && <p className="text-sm text-muted">Last saved {utc(timestampDate(loaded.updatedAt))}</p>}
      </div>
    </form>
  );
}

function Section({ id, title, children }: { id: string; title: string; children: ReactNode }) {
  return (
    <section aria-labelledby={id} className="grid gap-3 rounded border border-line bg-surface p-4">
      <h2 id={id} className="text-xl font-bold">
        {title}
      </h2>
      {children}
    </section>
  );
}

// Field is a labelled control with the hub's message under it when it refused this field.
function Field({ id, label, error, hint, children }: { id: string; label: string; error?: string | undefined; hint?: string | undefined; children: ReactNode }) {
  return (
    <div className="grid gap-1">
      <label htmlFor={id}>{label}</label>
      {children}
      {hint && <p className="text-sm text-muted">{hint}</p>}
      {error && (
        <p id={id + "-error"} className="border-l-4 border-error pl-2 text-sm font-bold">
          {error}
        </p>
      )}
    </div>
  );
}

function Check({ id, label, checked, set, error }: { id: string; label: string; checked: boolean; set: (v: boolean) => void; error?: string | undefined }) {
  return (
    <div className="grid gap-1">
      <label htmlFor={id} className="flex items-center gap-2">
        <input id={id} type="checkbox" checked={checked} onChange={(e) => { set(e.target.checked); }} />
        {label}
      </label>
      {error && <p className="border-l-4 border-error pl-2 text-sm font-bold">{error}</p>}
    </div>
  );
}

type Edit = (change: (s: OrganizationSettings) => void) => void;

function Notifiers({ draft, saved, edit, err }: { draft: OrganizationSettings; saved: OrganizationSettings; edit: Edit; err: (p: string) => string | undefined }) {
  const savedNames = new Set(saved.notifiers.map((n) => n.name));
  const add = () => {
    edit((s) => {
      s.notifiers.push(create(NotifierSchema, { name: "", kind: "ntfy", url: "https://ntfy.sh", events: create(NotifierEventsSchema, { minSeverity: "warning" }) }));
    });
  };
  return (
    <Section id="notifiers" title="Notifiers">
      <p className="text-muted">
        A notifier carries the hub’s events to people: a push to a phone, a webhook, an email. Each sends what reaches its lowest severity, warning by default. Its secret
        never goes in the settings: it lives in hub.yaml on the hub’s host.
      </p>
      {draft.notifiers.length === 0 && <p>No notifier yet: nobody hears about a server going down unless they’re looking.</p>}
      {draft.notifiers.map((n, i) => (
        <NotifierCard
          key={i}
          i={i}
          n={n}
          saved={savedNames.has(n.name) && n.name !== ""}
          edit={edit}
          err={(f) => err(`notifiers[${i}].${f}`)}
        />
      ))}
      <div>
        <button type="button" className={button} onClick={add}>
          Add a notifier
        </button>
      </div>
    </Section>
  );
}

function NotifierCard({ i, n, saved, edit, err }: { i: number; n: Notifier; saved: boolean; edit: Edit; err: (f: string) => string | undefined }) {
  const org = useClient(OrganizationService);
  const test = useMutation({ mutationFn: () => org.testNotifier({ name: n.name }) });
  const set = (change: (x: Notifier) => void) => {
    edit((s) => {
      const x = s.notifiers[i];
      if (x) {
        change(x);
      }
    });
  };
  const kind = (n.kind in notifierKinds ? n.kind : "ntfy") as NotifierKind;
  const id = (f: string) => `notifier-${i}-${f}`;
  const events = n.events ?? create(NotifierEventsSchema, {});
  return (
    <fieldset className="grid gap-3 rounded border border-line p-3">
      <legend className="px-1 font-bold">{n.name || "A new notifier"}</legend>
      <div className="grid items-start gap-3 sm:grid-cols-2">
        <Field id={id("name")} label="Name" error={err("name")} hint="Lower case; also the key of its secret in hub.yaml">
          <input id={id("name")} className={field} value={n.name} onChange={(e) => { set((x) => { x.name = e.target.value; }); }} />
        </Field>
        <Field id={id("kind")} label="Kind" error={err("kind")} hint={"Secret: " + notifierKinds[kind].secret}>
          <select id={id("kind")} className={field} value={kind} onChange={(e) => { set((x) => { x.kind = e.target.value; }); }}>
            {Object.entries(notifierKinds).map(([k, v]) => (
              <option key={k} value={k}>
                {v.label}
              </option>
            ))}
          </select>
        </Field>
        {kind !== "email" && !(kind === "webhook" && n.flavour === "discord") && (
          <Field id={id("url")} label={kind === "ntfy" ? "Server" : "Address"} error={err("url")}>
            <input id={id("url")} className={field} type="url" value={n.url} onChange={(e) => { set((x) => { x.url = e.target.value; }); }} />
          </Field>
        )}
        {kind === "ntfy" && (
          <Field id={id("topic")} label="Topic" error={err("topic")}>
            <input id={id("topic")} className={field} value={n.topic} onChange={(e) => { set((x) => { x.topic = e.target.value; }); }} />
          </Field>
        )}
        {kind === "webhook" && (
          <Field id={id("flavour")} label="Body" error={err("flavour")} hint={n.flavour === "discord" ? "A Discord webhook's whole URL is its secret: it goes in hub.yaml, not here" : undefined}>
            <select id={id("flavour")} className={field} value={n.flavour} onChange={(e) => { set((x) => { x.flavour = e.target.value; if (e.target.value === "discord") { x.url = ""; } }); }}>
              <option value="">JSON, signed</option>
              <option value="discord">A Discord channel’s webhook</option>
            </select>
          </Field>
        )}
        {kind === "email" && (
          <Field id={id("to")} label="To (commas between addresses)" error={err("to")}>
            <input id={id("to")} className={field} value={n.to.join(", ")} onChange={(e) => { set((x) => { x.to = list(e.target.value); }); }} />
          </Field>
        )}
        <Field id={id("severity")} label="Send from" error={err("events.min_severity")}>
          <select
            id={id("severity")}
            className={field}
            value={events.minSeverity || "warning"}
            onChange={(e) => { set((x) => { x.events = { ...(x.events ?? create(NotifierEventsSchema, {})), minSeverity: e.target.value }; }); }}
          >
            <option value="info">info and above: everything</option>
            <option value="notice">notice and above</option>
            <option value="warning">warning and above</option>
            <option value="critical">critical only</option>
          </select>
        </Field>
        <Field id={id("kinds")} label="Only these kinds or groups (empty for all)" error={err("events.kinds")} hint="server, backup.failed, role">
          <input
            id={id("kinds")}
            className={field}
            value={events.kinds.join(", ")}
            onChange={(e) => { set((x) => { x.events = { ...(x.events ?? create(NotifierEventsSchema, {})), kinds: list(e.target.value) }; }); }}
          />
        </Field>
      </div>
      <Check id={id("disabled")} label="Paused: sends nothing, and the log passes it by" checked={n.disabled} set={(v) => { set((x) => { x.disabled = v; }); }} />
      <div className="flex flex-wrap items-center gap-2">
        <button type="button" className={button} disabled={!saved || test.isPending} onClick={() => { test.mutate(); }}>
          Send a test
        </button>
        <button type="button" className={button} onClick={() => { edit((s) => { s.notifiers.splice(i, 1); }); }}>
          Remove
        </button>
        {!saved && <p className="text-sm text-muted">Save the settings first, then send a test.</p>}
        {test.data && (
          <p role={test.data.delivered ? "status" : "alert"} className="text-sm">
            {test.data.delivered ? "Delivered: check that it arrived." : `Not delivered: ${test.data.error}`}
          </p>
        )}
        {test.error && (
          <p role="alert" className="text-sm">
            The hub refused: {ConnectError.from(test.error).rawMessage}
          </p>
        )}
      </div>
    </fieldset>
  );
}

function DiscordCore({ draft, edit, err }: { draft: OrganizationSettings; edit: Edit; err: (p: string) => string | undefined }) {
  const d = draft.discord ?? create(DiscordSettingsSchema, {});
  const setD = (change: (x: NonNullable<OrganizationSettings["discord"]>) => void) => {
    edit((s) => {
      const x = s.discord ?? create(DiscordSettingsSchema, {});
      change(x);
      s.discord = x;
    });
  };
  const count = (n: number, one: string, many: string) => (n > 0 ? `${String(n)} ${n === 1 ? one : many}` : "");
  const rest = [
    count(d.recognition.length, "recognition rule", "recognition rules"),
    count(d.serverCards.length, "server card", "server cards"),
    count(d.seeding.length, "seeding rule", "seeding rules"),
    count(d.recaps.length, "recap rule", "recap rules"),
    count(d.streams?.streamers.length ?? 0, "streamer's go-live posts", "streamers' go-live posts"),
    count(d.news?.feeds.length ?? 0, "news feed", "news feeds"),
  ].filter((x) => x !== "");
  return (
    <Section id="discord" title="Discord">
      {d.guildId === "" && (
        <p>
          Discord isn’t connected. Put your server’s id below (Discord, Server Settings, Widget, or right-click the server with Developer Mode on), then run gravel’s bot with
          its app’s credentials (gravel-hub apps create, then docs/bot.md). Everything here works without it.
        </p>
      )}
      <div className="grid items-start gap-3 sm:grid-cols-3">
        <Field id="discord-guild" label="Server (guild) id" error={err("discord.guild_id")}>
          <input id="discord-guild" className={field} inputMode="numeric" value={d.guildId} onChange={(e) => { setD((x) => { x.guildId = e.target.value.trim(); }); }} />
        </Field>
        <Field id="discord-linked" label="Role for members who linked an account" error={err("discord.roles.linked")}>
          <input
            id="discord-linked"
            className={field}
            inputMode="numeric"
            value={d.roles?.linked ?? ""}
            onChange={(e) => { setD((x) => { x.roles = { ...(x.roles ?? create(DiscordRolesSchema, {})), linked: e.target.value.trim() }; }); }}
          />
        </Field>
        <Field id="discord-modlog" label="Mod-log channel id" error={err("discord.mod_log")}>
          <input id="discord-modlog" className={field} inputMode="numeric" value={d.modLog} onChange={(e) => { setD((x) => { x.modLog = e.target.value.trim(); }); }} />
        </Field>
      </div>
      <p className="text-sm text-muted">
        Recognition, server cards, seeding, presence, recaps, streamers, news and match results are edited in the YAML tab
        {rest.length > 0 ? `; these settings have ${rest.join(", ")}, and saving here keeps them` : ""}.
      </p>
    </Section>
  );
}

function Yaml() {
  const org = useClient(OrganizationService);
  const qc = useQueryClient();
  const exp = useQuery({ queryKey: ["settings-yaml"], queryFn: ({ signal }) => org.exportSettings({}, { signal }), refetchInterval: false, staleTime: Infinity });
  const [text, setText] = useState<string | undefined>();
  const [result, setResult] = useState<{ ok: boolean; lines: string[] }>();
  const manifest = text ?? exp.data?.manifest ?? "";
  const run = useMutation({
    mutationFn: async (how: "check" | "dry" | "apply") => {
      if (how === "check") {
        return { how, summary: (await org.checkSettings({ manifest })).summary, changes: [] as string[], applied: false };
      }
      const r = await org.applySettings({ manifest, dryRun: how === "dry", ...(exp.data?.updatedAt ? { ifUpdatedAt: exp.data.updatedAt } : {}) });
      return { how, summary: r.summary, changes: r.changes, applied: r.applied };
    },
    onSuccess: async (r) => {
      if (r.how === "check") {
        setResult({ ok: true, lines: [`Valid: ${r.summary}.`] });
      } else if (r.changes.length === 0) {
        setResult({ ok: true, lines: ["Nothing would change: this is the stored document."] });
      } else if (!r.applied) {
        setResult({ ok: true, lines: [`Saving would change: ${r.changes.join(", ")}.`, r.summary] });
      } else {
        setResult({ ok: true, lines: [`Saved: ${r.changes.join(", ")} changed.`] });
        setText(undefined);
        await qc.invalidateQueries({ queryKey: ["settings-yaml"] });
        await qc.invalidateQueries({ queryKey: ["settings"] });
      }
    },
    onError: (err) => {
      if (conflict(err)) {
        setResult({ ok: false, lines: ["Someone changed the settings since you opened this tab, so nothing was saved. Copy your edits, then reload the tab."] });
        return;
      }
      const p = problems(err, () => false);
      setResult({ ok: false, lines: ["The hub refused the document:", ...p.other] });
    },
  });
  if (exp.error) {
    return <p role="alert">The hub did not answer: {ConnectError.from(exp.error).rawMessage}.</p>;
  }
  return (
    <section aria-labelledby="yaml" className="grid gap-3">
      <h2 id="yaml" className="text-xl font-bold">
        The whole document
      </h2>
      <p className="text-muted">The same manifest gravel-hub settings apply and gravel settings apply take. Check it, see what saving would change, then save.</p>
      <label htmlFor="settings-yaml" className="sr-only">
        Settings as YAML
      </label>
      <textarea
        id="settings-yaml"
        className={`${field} min-h-96 font-mono text-sm`}
        spellCheck={false}
        value={manifest}
        onChange={(e) => { setText(e.target.value); setResult(undefined); }}
      />
      <div className="flex flex-wrap gap-2">
        <button type="button" className={button} disabled={run.isPending || !exp.data} onClick={() => { run.mutate("check"); }}>
          Check
        </button>
        <button type="button" className={button} disabled={run.isPending || !exp.data} onClick={() => { run.mutate("dry"); }}>
          Show what would change
        </button>
        <button type="button" className={primary} disabled={run.isPending || !exp.data} onClick={() => { run.mutate("apply"); }}>
          Save the document
        </button>
      </div>
      {result && (
        <div role={result.ok ? "status" : "alert"} className={`rounded border-2 bg-surface p-3 ${result.ok ? "border-ok" : "border-error"}`}>
          {result.lines.map((l) => (
            <p key={l}>{l}</p>
          ))}
        </div>
      )}
    </section>
  );
}
