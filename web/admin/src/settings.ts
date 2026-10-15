// The settings editor's helpers (gravel#205): the hub's refusal of a document as messages by field,
// and what each notifier kind needs.
import { Code, ConnectError } from "@connectrpc/connect";

import { FieldViolationSchema } from "./gen/gravel/hub/v1/organization_pb";

// Problems is a refused document: each field's message by its path ("notifiers[0].url"), and the
// messages no field on the page shows (the document's own, or a field edited only as YAML).
export interface Problems {
  byPath: Map<string, string>;
  other: string[];
}

export const noProblems: Problems = { byPath: new Map(), other: [] };

// problems reads an error's FieldViolation details; shown are the paths the page has a field for.
// An error with none is one message, the hub's words.
export function problems(err: unknown, shown: (path: string) => boolean): Problems {
  const e = ConnectError.from(err);
  const byPath = new Map<string, string>();
  const other: string[] = [];
  const details = e.code === Code.InvalidArgument ? e.findDetails(FieldViolationSchema) : [];
  for (const v of details) {
    if (v.path !== "" && shown(v.path)) {
      byPath.set(v.path, byPath.has(v.path) ? `${byPath.get(v.path) ?? ""}; ${v.message}` : v.message);
    } else {
      other.push(v.path !== "" ? `${v.path}: ${v.message}` : v.message);
    }
  }
  if (details.length === 0) {
    other.push(e.rawMessage);
  }
  return { byPath, other };
}

// conflict reports whether the hub refused a write because the settings changed since they were read.
export function conflict(err: unknown): boolean {
  return ConnectError.from(err).code === Code.Aborted;
}

// notifierKinds is what each kind of notifier is, what it needs in the document, and where its
// secret lives (hub.yaml; never in the settings).
export const notifierKinds = {
  ntfy: { label: "ntfy (a phone push)", secret: "an access token, optional: hub.yaml notify.secrets.<name>.file" },
  webhook: { label: "Webhook", secret: "the signing key, required: hub.yaml notify.secrets.<name>.file" },
  email: { label: "Email", secret: "none here; the mail server is hub.yaml notify.smtp" },
} as const;

export type NotifierKind = keyof typeof notifierKinds;

// list is a comma-separated field as the document's list, and back.
export function list(text: string): string[] {
  return text
    .split(",")
    .map((x) => x.trim())
    .filter((x) => x !== "");
}

// i is a list index in a path; hours a rule's weekly windows (discord/seedhours names their fields).
const i = String.raw`\[\d+\]`;
const hours = String.raw`hours(${i}(\.(days|from|to))?)?`;

// placed are the paths the form shows a message at: each field it has, each list (its length, a
// duplicate) and each entry (a problem of the whole entry, such as an overlap). Any other path is
// listed at the top of the form, so no message is lost.
const placed = new RegExp(
  "^(" +
    [
      String.raw`notifiers${i}\.(name|kind|url|flavour|topic|to(${i})?|events\.min_severity|events\.kinds(${i})?)`,
      String.raw`discord\.(guild_id|mod_log|roles\.linked|roles\.providers\.(discord|steam))`,
      String.raw`discord\.recognition(${i}(\.(role|rule|count))?)?`,
      String.raw`discord\.server_cards(${i}(\.(server|channel|note))?)?`,
      String.raw`discord\.seeding(${i}(\.(server|channel|role|below|cooldown|note|${hours}))?)?`,
      String.raw`discord\.recaps(${i}(\.(server|channel|title|floor|note|${hours}))?)?`,
      String.raw`discord\.presence\.(channel|note|games(${i}(\.(game|label|activities(${i})?|application_ids(${i})?))?)?)`,
      String.raw`discord\.streams\.(channel|role|note|streamers(${i}(\.(platform|login|label))?)?)`,
      String.raw`discord\.news\.feeds(${i}(\.(app_id|channel|label|color|feeds(${i})?))?)?`,
      String.raw`discord\.match_results\.(channel|servers(${i})?)`,
      String.raw`discord\.events\.(channel|min_severity|kinds(${i})?)`,
      String.raw`stats\.public`,
      String.raw`local_accounts\.(registration|min_password_length)`,
    ].join("|") +
    ")$",
);

// isPlaced reports whether the form has a place for a message about path.
export function isPlaced(path: string): boolean {
  return placed.test(path);
}

// within is every message about path and its items ("notifiers[0].to" and "notifiers[0].to[1]"),
// for a field that edits a whole list.
export function within(p: Problems, path: string): string | undefined {
  const items = new RegExp("^" + path.replace(/[.[\]]/g, "\\$&") + String.raw`\[\d+\]$`);
  const found = [...p.byPath].filter(([k]) => k === path || items.test(k)).map(([, v]) => v);
  return found.length > 0 ? found.join("; ") : undefined;
}
