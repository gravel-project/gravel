// People (gravel#203): the members and their grants, and every action on them in one place: grant
// and revoke a role (instance-wide or for a game, as the granter rules allow), make a local account
// and its password link, issue a reset link, end a member's sessions, and hand the hub to another
// member. A one-time link is shown once, in this page's memory only: it is never fetched again.
// A nominee sees the handshake here even when their roles don't let them read the members.
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, type SubmitEvent, useState } from "react";

import { IdentityService, type PasswordLink, type User } from "../gen/gravel/hub/v1/identity_pb";
import { type Organization, OrganizationService } from "../gen/gravel/hub/v1/organization_pb";
import { ServerService } from "../gen/gravel/hub/v1/servers_pb";
import { grantable, grantLabel, localUsername, manages, mayChange, readsMembers } from "../people";
import { useClient } from "../transport";

const field = "block w-full rounded border border-line bg-surface px-2 py-1";
const button = "rounded border border-line bg-surface px-3 py-1 disabled:opacity-60";
const primary = "rounded bg-accent px-3 py-1 font-bold text-on-accent disabled:opacity-60";

// pageSize is how many members one ListUsers page brings.
export const pageSize = 100;

export function People({ me, org }: { me: User; org: Organization | undefined }) {
  return (
    <>
      <h1 className="mb-4 text-2xl font-bold">People</h1>
      <Ownership me={me} org={org} />
      {manages(me) && <AddLocal />}
      {readsMembers(me) ? (
        <Members me={me} />
      ) : org?.pendingOwnerUserId === me.id ? null : (
        <p>Your roles do not include reading the members (identity:read). An owner or admin grants roles on the hub.</p>
      )}
    </>
  );
}

// useRefresh is what every action does after the hub answers: read the members, the hub and the
// viewer again, so the page shows the change (and a grant that changed the viewer's own access).
function useRefresh(): () => Promise<void> {
  const qc = useQueryClient();
  return async () => {
    await Promise.all([
      qc.invalidateQueries({ queryKey: ["people"] }),
      qc.invalidateQueries({ queryKey: ["organization"] }),
      qc.invalidateQueries({ queryKey: ["me"] }),
    ]);
  };
}

function message(err: unknown): string {
  return ConnectError.from(err).rawMessage;
}

// utc is a time as the member pages print it.
export function utc(d: Date): string {
  return d.toISOString().slice(0, 16).replace("T", " ") + " UTC";
}

function Ownership({ me, org }: { me: User; org: Organization | undefined }) {
  const client = useClient(OrganizationService);
  const refresh = useRefresh();
  const accept = useMutation({ mutationFn: () => client.acceptOwnership({}), onSuccess: refresh });
  const cancel = useMutation({ mutationFn: () => client.cancelOwnershipTransfer({}), onSuccess: refresh });
  if (!org || org.pendingOwnerUserId === "") {
    return null;
  }
  const until = org.pendingOwnerExpiresAt ? utc(timestampDate(org.pendingOwnerExpiresAt)) : "";
  const failed = accept.error ?? cancel.error;
  const busy = accept.isPending || cancel.isPending;
  return (
    <section aria-labelledby="ownership" className="mb-6 rounded border-2 border-accent bg-surface p-4">
      <h2 id="ownership" className="mb-2 text-xl font-bold">
        Handing the hub over
      </h2>
      {org.pendingOwnerUserId === me.id ? (
        <>
          <p className="mb-3">
            The owner offers you {org.name}, until {until}. If you accept, you own it and they stay an admin.
          </p>
          <div className="flex flex-wrap gap-2">
            <button type="button" className={primary} disabled={busy} onClick={() => { accept.mutate(); }}>
              Accept the hub
            </button>
            <button type="button" className={button} disabled={busy} onClick={() => { cancel.mutate(); }}>
              Decline
            </button>
          </div>
        </>
      ) : (
        <>
          <p className="mb-3">
            {org.pendingOwnerName} may take the hub over until {until}. Until they accept, you own it.
          </p>
          <button type="button" className={button} disabled={busy} onClick={() => { cancel.mutate(); }}>
            Cancel the nomination
          </button>
        </>
      )}
      {failed && (
        <p role="alert" className="mt-2">
          The hub refused: {message(failed)}
        </p>
      )}
    </section>
  );
}

function AddLocal() {
  const client = useClient(IdentityService);
  const refresh = useRefresh();
  const [username, setUsername] = useState("");
  const [email, setEmail] = useState("");
  const create = useMutation({
    mutationFn: () => client.createLocalAccount({ username: username.trim(), email: email.trim() }),
    onSuccess: async () => {
      setUsername("");
      setEmail("");
      await refresh();
    },
  });
  const submit = (e: SubmitEvent<HTMLFormElement>) => {
    e.preventDefault();
    create.mutate();
  };
  return (
    <section aria-labelledby="add-local" className="mb-6 rounded border border-line bg-surface p-4">
      <h2 id="add-local" className="mb-2 text-xl font-bold">
        Add a local account
      </h2>
      {create.data?.link ? (
        <OneTimeLink
          what={`The account ${create.data.user?.displayName ?? ""} is made. Send them this link to set their password`}
          link={create.data.link}
          done={() => { create.reset(); }}
        />
      ) : (
        <form onSubmit={submit} className="grid gap-3 sm:grid-cols-[1fr_1fr_auto] sm:items-end">
          <label>
            Username
            <input className={field} required minLength={3} maxLength={32} autoComplete="off" value={username} onChange={(e) => { setUsername(e.target.value); }} />
          </label>
          <label>
            Email, for recovery only (optional)
            <input className={field} type="email" autoComplete="off" value={email} onChange={(e) => { setEmail(e.target.value); }} />
          </label>
          <button type="submit" className={primary} disabled={create.isPending}>
            Make the account
          </button>
        </form>
      )}
      {create.error && (
        <p role="alert" className="mt-2">
          The hub refused: {message(create.error)}
        </p>
      )}
    </section>
  );
}

// OneTimeLink shows a password link the hub will never show again: the page keeps it until Done.
function OneTimeLink({ what, link, done }: { what: string; link: PasswordLink; done: () => void }) {
  const url = new URL(link.url, window.location.origin).href;
  const [copied, setCopied] = useState(false);
  const until = link.expiresAt ? utc(timestampDate(link.expiresAt)) : "";
  // A browser without the clipboard (or one that refuses it) leaves the link to be selected.
  const copy = () => {
    try {
      navigator.clipboard.writeText(url).then(() => { setCopied(true); }, () => undefined);
    } catch {
      // no clipboard here
    }
  };
  return (
    <div role="status" className="grid gap-2">
      <p>
        {what}. It is shown once and works once, until {until}.
      </p>
      <p className="break-all rounded border border-line bg-ground p-2 font-mono text-sm">{url}</p>
      <div className="flex flex-wrap gap-2">
        <button type="button" className={button} onClick={copy}>
          {copied ? "Copied" : "Copy the link"}
        </button>
        <button type="button" className={button} onClick={done}>
          Done
        </button>
      </div>
    </div>
  );
}

function Members({ me }: { me: User }) {
  const ids = useClient(IdentityService);
  const servers = useClient(ServerService);
  const list = useInfiniteQuery({
    queryKey: ["people"],
    initialPageParam: "",
    queryFn: ({ pageParam, signal }) => ids.listUsers({ pageSize, pageToken: pageParam }, { signal }),
    getNextPageParam: (last) => last.nextPageToken || undefined,
  });
  const games = useQuery({ queryKey: ["games"], queryFn: ({ signal }) => servers.listGames({}, { signal }), staleTime: Infinity, refetchInterval: false });
  const gameList = games.data?.games ?? [];
  const names = new Map(gameList.map((g) => [g.id, g.name]));
  if (list.error && ConnectError.from(list.error).code === Code.PermissionDenied) {
    return <p>Your roles do not include reading the members (identity:read). An owner or admin grants roles on the hub.</p>;
  }
  if (list.error && !list.data) {
    return <p role="alert">The hub did not answer: {message(list.error)}. Trying again shortly.</p>;
  }
  if (!list.data) {
    return (
      <p role="status" className="text-muted">
        Loading the members…
      </p>
    );
  }
  const members = list.data.pages.flatMap((p) => p.users);
  return (
    <section aria-labelledby="members">
      <h2 id="members" className="mb-2 text-xl font-bold">
        Members
      </h2>
      <ul className="border-t border-line">
        {members.map((m) => (
          <MemberRow key={m.id} me={me} member={m} games={gameList.map((g) => g.id)} names={names} />
        ))}
      </ul>
      {list.hasNextPage && (
        <button type="button" className={`${button} mt-3`} disabled={list.isFetchingNextPage} onClick={() => { void list.fetchNextPage(); }}>
          More members
        </button>
      )}
    </section>
  );
}

type Note = { ok: boolean; text: string } | undefined;

function MemberRow({ me, member, games, names }: { me: User; member: User; games: string[]; names: ReadonlyMap<string, string> }) {
  const ids = useClient(IdentityService);
  const orgs = useClient(OrganizationService);
  const refresh = useRefresh();
  const [note, setNote] = useState<Note>();
  const [link, setLink] = useState<PasswordLink>();
  const [confirm, setConfirm] = useState<"" | "logout" | "nominate">("");
  const done = (text: string) => async () => {
    setNote({ ok: true, text });
    setConfirm("");
    await refresh();
  };
  const failed = (err: unknown) => {
    setNote({ ok: false, text: "The hub refused: " + message(err) });
    setConfirm("");
  };
  const setRole = useMutation({
    mutationFn: (g: { role: string; gameId: string; granted: boolean }) => ids.setUserRole({ userId: member.id, ...g }),
    onSuccess: (_, g) =>
      done(`${member.displayName} ${g.granted ? "is now" : "is no longer"} ${grantLabel(g, names)}.`)(),
    onError: failed,
  });
  const reset = useMutation({
    mutationFn: (username: string) => ids.issuePasswordReset({ username }),
    onSuccess: (r) => {
      setNote(undefined);
      setLink(r.link);
    },
    onError: failed,
  });
  const logout = useMutation({
    mutationFn: () => ids.revokeUserSessions({ userId: member.id }),
    onSuccess: (r) => done(`${member.displayName} is logged out everywhere (${r.revoked} sessions ended).`)(),
    onError: failed,
  });
  const nominate = useMutation({
    mutationFn: () => orgs.nominateOwner({ userId: member.id }),
    onSuccess: done(`${member.displayName} may take the hub over for 7 days; until they accept, you own it.`),
    onError: failed,
  });
  const offer = grantable(me, member, games);
  const local = localUsername(member);
  const self = member.id === me.id;
  const busy = setRole.isPending || reset.isPending || logout.isPending || nominate.isPending;
  const joined = member.createdAt ? utc(timestampDate(member.createdAt)) : "";
  const seen = member.lastLoginAt ? utc(timestampDate(member.lastLoginAt)) : "never";
  return (
    <li className="grid gap-3 border-b border-line py-4 md:grid-cols-[minmax(0,1fr)_minmax(0,1.4fr)]">
      <div>
        <p className="font-bold">
          <a href={"/members/" + member.id} className="underline">
            {member.displayName}
          </a>
          {member.owner && <span className="ml-2 rounded border border-line px-2 py-0.5 text-sm font-normal">owner</span>}
          {self && <span className="ml-2 text-sm font-normal text-muted">you</span>}
        </p>
        <p className="text-sm text-muted">{member.identities.map((i) => i.provider + " · " + (i.displayName || i.subject)).join(", ")}</p>
        <p className="text-sm text-muted">
          Joined {joined} · last login {seen}
        </p>
      </div>
      <div className="grid gap-3">
        <Grants me={me} member={member} names={names} busy={busy} revoke={(role, gameId) => { setRole.mutate({ role, gameId, granted: false }); }} />
        {offer.roles.length > 0 && (
          <GrantForm member={member} offer={offer} names={names} busy={busy} grant={(role, gameId) => { setRole.mutate({ role, gameId, granted: true }); }} />
        )}
        {link && <OneTimeLink what={`A new password link for ${member.displayName}; their earlier links no longer work`} link={link} done={() => { setLink(undefined); }} />}
        <Actions>
          {manages(me) && local !== undefined && !link && (
            <button type="button" className={button} disabled={busy} onClick={() => { reset.mutate(local); }}>
              Password reset link
            </button>
          )}
          {manages(me) && !self && (
            <Confirmable
              label="Log out everywhere"
              question={`End every session ${member.displayName} has, and the tokens apps hold for them?`}
              open={confirm === "logout"}
              busy={busy}
              ask={() => { setConfirm("logout"); }}
              no={() => { setConfirm(""); }}
              yes={() => { logout.mutate(); }}
            />
          )}
          {me.owner && !self && (
            <Confirmable
              label={`Hand the hub to ${member.displayName}`}
              question={`Offer ${member.displayName} the hub for 7 days? They own it once they accept, and you stay an admin.`}
              open={confirm === "nominate"}
              busy={busy}
              ask={() => { setConfirm("nominate"); }}
              no={() => { setConfirm(""); }}
              yes={() => { nominate.mutate(); }}
            />
          )}
        </Actions>
        {note && (
          <p role={note.ok ? "status" : "alert"} className={note.ok ? "text-sm" : "text-sm font-bold"}>
            {note.text}
          </p>
        )}
      </div>
    </li>
  );
}

function Grants({ me, member, names, busy, revoke }: { me: User; member: User; names: ReadonlyMap<string, string>; busy: boolean; revoke: (role: string, gameId: string) => void }) {
  if (member.grants.length === 0) {
    return <p className="text-sm text-muted">{member.owner ? "Owns the hub: every capability." : "No role: a member."}</p>;
  }
  return (
    <ul aria-label={`${member.displayName}'s roles`} className="flex flex-wrap gap-2">
      {member.grants.map((g) => {
        const label = grantLabel(g, names);
        return (
          <li key={g.role + "/" + g.gameId} className="flex items-center gap-2 rounded border border-line px-2 py-0.5">
            <span>{label}</span>
            {mayChange(me, member, g.role, g.gameId) && (
              <button type="button" className="text-sm underline" disabled={busy} aria-label={`Revoke ${label} from ${member.displayName}`} onClick={() => { revoke(g.role, g.gameId); }}>
                Revoke
              </button>
            )}
          </li>
        );
      })}
    </ul>
  );
}

function GrantForm({ member, offer, names, busy, grant }: { member: User; offer: { roles: string[]; scopes: string[] }; names: ReadonlyMap<string, string>; busy: boolean; grant: (role: string, gameId: string) => void }) {
  const [role, setRole] = useState(offer.roles.includes("moderator") ? "moderator" : (offer.roles[0] ?? ""));
  const [scope, setScope] = useState(offer.scopes[0] ?? "");
  const submit = (e: SubmitEvent<HTMLFormElement>) => {
    e.preventDefault();
    grant(role, scope);
  };
  return (
    <form onSubmit={submit} aria-label={`Grant ${member.displayName} a role`} className="grid gap-2 sm:grid-cols-[1fr_1fr_auto] sm:items-end">
      <label>
        Role
        <select className={field} value={role} onChange={(e) => { setRole(e.target.value); }}>
          {offer.roles.map((r) => (
            <option key={r} value={r}>
              {r}
            </option>
          ))}
        </select>
      </label>
      <label>
        For
        <select className={field} value={scope} onChange={(e) => { setScope(e.target.value); }}>
          {offer.scopes.map((s) => (
            <option key={s} value={s}>
              {s === "" ? "Every game" : (names.get(s) ?? s)}
            </option>
          ))}
        </select>
      </label>
      <button type="submit" className={button} disabled={busy}>
        Grant
      </button>
    </form>
  );
}

function Actions({ children }: { children: ReactNode }) {
  return <div className="flex flex-wrap gap-2">{children}</div>;
}

// Confirmable is an action that asks first, in the page: a second click on Yes does it.
function Confirmable({ label, question, open, busy, ask, no, yes }: { label: string; question: string; open: boolean; busy: boolean; ask: () => void; no: () => void; yes: () => void }) {
  if (!open) {
    return (
      <button type="button" className={button} disabled={busy} onClick={ask}>
        {label}
      </button>
    );
  }
  return (
    <div role="group" aria-label={label} className="flex w-full flex-wrap items-center gap-2 rounded border-2 border-warn p-2">
      <p className="w-full">{question}</p>
      <button type="button" className={primary} disabled={busy} onClick={yes}>
        Yes, {label.charAt(0).toLowerCase() + label.slice(1)}
      </button>
      <button type="button" className={button} disabled={busy} onClick={no}>
        Cancel
      </button>
    </div>
  );
}
