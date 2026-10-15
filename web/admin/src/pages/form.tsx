// The settings form's building blocks (gravel#205, #253): a section, a labelled field with the hub's
// message under it, a checkbox, a list typed with commas, and the messages about a whole list or
// entry. Every control is labelled; a message is a left border and bold text, never colour alone.
import { type ReactNode, useState } from "react";

import { list } from "../settings";

export const field = "block w-full rounded border border-line bg-surface px-2 py-1";
export const button = "rounded border border-line bg-surface px-3 py-1 disabled:opacity-60";
// The main action: outlined in the accent, never filled with it (see People).
export const primary = "rounded border-2 border-accent bg-surface px-3 py-1 font-bold disabled:opacity-60";

// Err is what the hub said about a path, if anything.
export type Err = (path: string) => string | undefined;

export function Section({ id, title, children }: { id: string; title: string; children: ReactNode }) {
  return (
    <section aria-labelledby={id} className="grid gap-3 rounded border border-line bg-surface p-4">
      <h2 id={id} className="text-xl font-bold">
        {title}
      </h2>
      {children}
    </section>
  );
}

// Part is a titled part of a section: one Discord module's settings.
export function Part({ id, title, intro, error, children }: { id: string; title: string; intro: ReactNode; error?: string | undefined; children: ReactNode }) {
  return (
    <section aria-labelledby={id} className="grid gap-3 border-t border-line pt-4">
      <h3 id={id} className="text-lg font-bold">
        {title}
      </h3>
      <p className="text-muted">{intro}</p>
      <Message text={error} />
      {children}
    </section>
  );
}

// Message is the hub's refusal of a whole list or entry, where no one field is at fault.
export function Message({ text }: { text?: string | undefined }) {
  return text ? <p className="border-l-4 border-error pl-2 text-sm font-bold">{text}</p> : null;
}

// Field is a labelled control with the hub's message under it when it refused this field.
export function Field({ id, label, error, hint, children }: { id: string; label: string; error?: string | undefined; hint?: string | undefined; children: ReactNode }) {
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

export function Check({ id, label, checked, set, error }: { id: string; label: string; checked: boolean; set: (v: boolean) => void; error?: string | undefined }) {
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

// ListInput is a list typed with commas between items. It shows what was typed, so a trailing
// comma survives until the next item is typed, and hands the document the items.
export function ListInput({ id, value, set }: { id: string; value: readonly string[]; set: (v: string[]) => void }) {
  const [text, setText] = useState(value.join(", "));
  return (
    <input
      id={id}
      className={field}
      value={text}
      onChange={(e) => {
        setText(e.target.value);
        set(list(e.target.value));
      }}
    />
  );
}

// Entry is one item of a list in the form, with its remove button, named for what it removes.
export function Entry({ legend, error, remove, removeLabel, children }: { legend: string; error?: string | undefined; remove: () => void; removeLabel: string; children: ReactNode }) {
  return (
    <fieldset className="grid gap-3 rounded border border-line p-3">
      <legend className="px-1 font-bold">{legend}</legend>
      <Message text={error} />
      {children}
      <div>
        <button type="button" className={button} onClick={remove}>
          {removeLabel}
        </button>
      </div>
    </fieldset>
  );
}

// Add is a list's "Add a …" button, refused once the list holds as many as the hub allows.
export function Add({ label, count, max, add }: { label: string; count: number; max: number; add: () => void }) {
  return (
    <div className="flex flex-wrap items-center gap-2">
      <button type="button" className={button} disabled={count >= max} onClick={add}>
        {label}
      </button>
      {count >= max && <p className="text-sm text-muted">At most {max}.</p>}
    </div>
  );
}
