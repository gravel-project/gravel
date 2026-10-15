// Apps (gravel#205): the first-party apps registered on the hub (ADR-0008, ADR-0015): register one
// and see its secret once, list them, revoke one. apps:manage, in person.
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { ConnectError } from "@connectrpc/connect";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type SubmitEvent, useState } from "react";

import { type App, AppService } from "../gen/gravel/hub/v1/apps_pb";
import { list } from "../settings";
import { useClient } from "../transport";
import { utc } from "./People";

const field = "block w-full rounded border border-line bg-surface px-2 py-1";
const button = "rounded border border-line bg-surface px-3 py-1 disabled:opacity-60";
const primary = "rounded border-2 border-accent bg-surface px-3 py-1 font-bold disabled:opacity-60";

// scopes are what an app may hold (internal/authz AppScopes), with what each lets it do.
export const scopes: readonly (readonly [string, string])[] = [
  ["identity:read", "read members and their linked accounts"],
  ["servers:read", "read who is on each server"],
  ["servers:moderate", "kick, ban and message players"],
  ["servers:operate", "park, start and reboot servers"],
  ["servers:lease", "take a server for a match"],
  ["servers:configure", "change a server's configuration"],
  ["roles:grant", "grant and revoke the moderator role (Discord role sync)"],
  ["events:read", "read the events log"],
  ["stats:read", "read private boards and match results"],
  ["participation:write", "record participation"],
];

function message(err: unknown): string {
  return ConnectError.from(err).rawMessage;
}

export function Apps() {
  const apps = useClient(AppService);
  const q = useQuery({ queryKey: ["apps"], queryFn: ({ signal }) => apps.listApps({}, { signal }) });
  return (
    <>
      <h1 className="mb-4 text-2xl font-bold">Apps</h1>
      <p className="mb-6 max-w-prose text-muted">
        An app calls the hub with its own credentials: gravel’s Discord bot, a match organiser, a site of yours. It holds only the scopes given here, and none of the
        owner’s or the operators’ own powers.
      </p>
      <Register />
      <section aria-labelledby="registered">
        <h2 id="registered" className="mb-2 text-xl font-bold">
          Registered
        </h2>
        {q.error ? (
          <p role="alert">The hub did not answer: {message(q.error)}.</p>
        ) : !q.data ? (
          <p role="status" className="text-muted">
            Loading the apps…
          </p>
        ) : q.data.apps.length === 0 ? (
          <p>No apps yet.</p>
        ) : (
          <ul className="border-t border-line">
            {q.data.apps.map((a) => (
              <AppRow key={a.clientId} app={a} />
            ))}
          </ul>
        )}
      </section>
    </>
  );
}

function Register() {
  const apps = useClient(AppService);
  const qc = useQueryClient();
  const [name, setName] = useState("");
  const [picked, setPicked] = useState<string[]>([]);
  const [host, setHost] = useState("");
  const [redirects, setRedirects] = useState("");
  const create = useMutation({
    mutationFn: () =>
      apps.createApp({
        name: name.trim(),
        scopes: [...picked, ...list(host)],
        redirectUris: redirects.split("\n").map((x) => x.trim()).filter((x) => x !== ""),
      }),
    onSuccess: async () => {
      setName("");
      setPicked([]);
      setHost("");
      setRedirects("");
      await qc.invalidateQueries({ queryKey: ["apps"] });
    },
  });
  const submit = (e: SubmitEvent<HTMLFormElement>) => {
    e.preventDefault();
    create.mutate();
  };
  const toggle = (s: string, on: boolean) => { setPicked(on ? [...picked, s] : picked.filter((x) => x !== s)); };
  return (
    <section aria-labelledby="register" className="mb-8 grid gap-3 rounded border border-line bg-surface p-4">
      <h2 id="register" className="text-xl font-bold">
        Register an app
      </h2>
      {create.data ? (
        <Secret clientId={create.data.app?.clientId ?? ""} secret={create.data.clientSecret} done={() => { create.reset(); }} />
      ) : (
        <form onSubmit={submit} aria-label="Register an app" className="grid gap-3">
          <label htmlFor="app-name">
            Name
            <input id="app-name" className={field} required maxLength={64} value={name} onChange={(e) => { setName(e.target.value); }} />
          </label>
          <fieldset className="grid gap-1">
            <legend className="mb-1">What it may do</legend>
            {scopes.map(([s, what]) => (
              <label key={s} htmlFor={`scope-${s}`} className="flex items-baseline gap-2">
                <input id={`scope-${s}`} type="checkbox" checked={picked.includes(s)} onChange={(e) => { toggle(s, e.target.checked); }} />
                <span>
                  <span className="font-mono text-sm">{s}</span> <span className="text-muted">{what}</span>
                </span>
              </label>
            ))}
          </fieldset>
          <label htmlFor="app-host">
            Its own scopes, host:&lt;name&gt; (commas between; optional)
            <input id="app-host" className={field} value={host} onChange={(e) => { setHost(e.target.value); }} />
          </label>
          <label htmlFor="app-redirects">
            Where its pages receive a member who signed in (one https address a line; optional)
            <textarea id="app-redirects" className={`${field} font-mono text-sm`} rows={2} value={redirects} onChange={(e) => { setRedirects(e.target.value); }} />
          </label>
          <div>
            <button type="submit" className={primary} disabled={create.isPending}>
              Register
            </button>
          </div>
        </form>
      )}
      {create.error && (
        <p role="alert" className="text-sm">
          The hub refused: {message(create.error)}
        </p>
      )}
    </section>
  );
}

// Secret shows an app's client secret, once: the hub keeps only its hash, and the page forgets it
// at Done.
function Secret({ clientId, secret, done }: { clientId: string; secret: string; done: () => void }) {
  const [copied, setCopied] = useState(false);
  const copy = () => {
    try {
      navigator.clipboard.writeText(secret).then(() => { setCopied(true); }, () => undefined);
    } catch {
      // no clipboard here: the secret can be selected
    }
  };
  return (
    <div role="status" className="grid gap-2">
      <p>
        Registered. Its client id is <span className="font-mono">{clientId}</span>. Here is its secret, shown once: the hub keeps only its hash. Put it in a file the app
        reads, mode 0600.
      </p>
      <p className="break-all rounded border border-line bg-ground p-2 font-mono text-sm">{secret}</p>
      <div className="flex flex-wrap gap-2">
        <button type="button" className={button} onClick={copy}>
          {copied ? "Copied" : "Copy the secret"}
        </button>
        <button type="button" className={button} onClick={done}>
          Done
        </button>
      </div>
    </div>
  );
}

function AppRow({ app }: { app: App }) {
  const apps = useClient(AppService);
  const qc = useQueryClient();
  const [asking, setAsking] = useState(false);
  const revoke = useMutation({
    mutationFn: () => apps.revokeApp({ clientId: app.clientId }),
    onSuccess: async () => {
      setAsking(false);
      await qc.invalidateQueries({ queryKey: ["apps"] });
    },
  });
  const revoked = app.revokedAt !== undefined;
  return (
    <li className="grid gap-2 border-b border-line py-3 md:grid-cols-[minmax(0,1fr)_minmax(0,1.4fr)_auto] md:items-start">
      <div className="min-w-0">
        <p className="font-bold">
          {app.name}
          {revoked && <span className="ml-2 rounded border border-line px-2 py-0.5 text-sm font-normal">revoked</span>}
        </p>
        <p className="break-all font-mono text-sm text-muted">{app.clientId}</p>
        <p className="text-sm text-muted">
          Registered {app.createdAt ? utc(timestampDate(app.createdAt)) : ""}
          {app.revokedAt && ` · revoked ${utc(timestampDate(app.revokedAt))}`}
        </p>
      </div>
      <div className="min-w-0 text-sm">
        <p className="break-words font-mono">{app.scopes.join(" ") || "no scopes"}</p>
        {app.redirectUris.length > 0 && <p className="break-all text-muted">{app.redirectUris.join(" ")}</p>}
      </div>
      <div>
        {!revoked &&
          (asking ? (
            <div role="group" aria-label={`Revoke ${app.name}`} className="grid gap-2 rounded border-2 border-warn p-2">
              <p>Revoke {app.name}? Its tokens stop working at once, and members’ grants to it go.</p>
              <div className="flex gap-2">
                <button type="button" className={primary} disabled={revoke.isPending} onClick={() => { revoke.mutate(); }}>
                  Yes, revoke it
                </button>
                <button type="button" className={button} onClick={() => { setAsking(false); }}>
                  Cancel
                </button>
              </div>
            </div>
          ) : (
            <button type="button" className={button} onClick={() => { setAsking(true); }}>
              Revoke
            </button>
          ))}
        {revoke.error && (
          <p role="alert" className="text-sm">
            The hub refused: {message(revoke.error)}
          </p>
        )}
      </div>
    </li>
  );
}
