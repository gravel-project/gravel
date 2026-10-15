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
