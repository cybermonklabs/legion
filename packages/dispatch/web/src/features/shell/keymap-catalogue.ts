/**
 * Every action of the keymap, whether or not the page that draws it is open: its scope, its id,
 * its label and its default keys. A component registers an action by naming it here
 * (`useKeymap(scope, [{ action: "inbox.snooze", run }])`, `keymap.ts`), so the `?` help, the
 * command bar and Settings read one label for it, and its keys are the person's where they
 * changed them (`keymap-keys.ts`) and these defaults everywhere else.
 *
 * Each scope writes its actions in a table of its own, keyed by id (`keymap-catalogue-inbox.ts`
 * and the others beside it), and this file builds from those tables the one map of every action,
 * named by its scope and its id. The keys are the defaults of the spec's table "The keys"
 * (dispatch://INFRA-2160/spec, R13). An action holds its keys in its table whether or not any
 * component registers it now, so the refusal check (`keymap-refusal.ts`) reads every default key
 * at once, and no person's key takes a key that a page not open would take.
 */

import { AGENTS_ACTIONS } from "./keymap-catalogue-agents";
import { ARCHITECTURE_ACTIONS } from "./keymap-catalogue-architecture";
import { BOARD_ACTIONS } from "./keymap-catalogue-board";
import { DELIVERY_ACTIONS } from "./keymap-catalogue-delivery";
import { DOCUMENT_ACTIONS } from "./keymap-catalogue-document";
import { GLOBAL_ACTIONS } from "./keymap-catalogue-global";
import { INBOX_ACTIONS } from "./keymap-catalogue-inbox";
import { ISSUE_ACTIONS } from "./keymap-catalogue-issue";
import { PROJECT_ACTIONS } from "./keymap-catalogue-project";

/** A scope an action is listed under; `dialog` is no scope of its own here, since a dialog
 *  registers the actions of the page it shows (the Inbox drawer registers `inbox.*`). */
export type CatalogueScope =
  | "global"
  | "inbox"
  | "project"
  | "architecture"
  | "board"
  | "delivery"
  | "issue"
  | "document"
  | "agents";

/**
 * The part of a page that draws an action, which is what the refusal check reads: two actions of
 * one surface, or of two surfaces drawn at the same time, cannot share a key (`keymap-refusal.ts`).
 * The `project` scope stays pushed under the Board and the Architecture page, but only one tab
 * panel of a project mounts at a time, so the List's own keys (`project-list`) and the Board's
 * never meet; both meet the project page's own (`project-page`).
 *
 * An issue's address draws one of two pages, never both (`issue-page/IssuePage.tsx`): today's
 * page or the new issue page a reader turns on. Each action only today's page draws is of
 * `issue`, and each only the new page draws is of `issue-outline`: its outline's keys, which
 * share today's page's `issue` scope, `j` and `k` among them, and Details' key, which the new
 * page's frame draws where every other page draws the margin, so Toggle margin is drawn on every
 * page but the new issue page (`margin`). The actions both pages draw, the header's keys and
 * rows and the tab chords, are of `issue-both`, drawn with each of the two, so a key on either
 * page meets every key that page draws. An action of today's page alone meets exactly the keys it
 * met before the new page had keys of its own, and an action of both pages meets the new page's
 * keys too. Two actions share a key on purpose, by focus: on the new page `a` approves on a
 * focused approval chip and answers on a focused row of the outline (`keymap-refusal.ts`'s
 * `EXCLUSIVE_BY_FOCUS`).
 */
export type KeySurface =
  | "agents"
  | "architecture"
  | "board"
  | "delivery"
  | "document"
  | "global"
  | "inbox"
  | "issue"
  | "issue-both"
  | "issue-outline"
  | "margin"
  | "project-list"
  | "project-page";

export interface CatalogueEntry {
  readonly scope: CatalogueScope;
  /** Its key in its scope's table, so unique within the scope; the registry describes the
   *  binding under it. */
  readonly id: string;
  readonly label: string;
  /** The default keys, in tinykeys syntax (`keymap.ts`), which a person can change. */
  readonly keys: readonly string[];
  /** The keys a person cannot change: the digits, the arrows, Enter, Escape, Tab and Space, and
   *  every key a run reads through `event.key`. They are always bound, after the person's. */
  readonly fixedKeys?: readonly string[];
  /** Whether the command bar offers the action as a row. Always written: a default read from the
   *  number of keys would drop an action from the bar once a person gives it a second key. */
  readonly palette: boolean;
  /** Fires while a control that takes typed text has the focus (`KeyBinding.inEditable`). */
  readonly inEditable?: boolean;
  /** The action writes a record, so the registry runs it once per press: a held key's repeats
   *  (`KeyboardEvent.repeat`) do nothing (`actionBinding`, `keymap.ts`). A repeat would write
   *  again: the same review twice, or, once the write hands the reader on to the next item (R8),
   *  a write on that item, which nobody chose. A palette row runs it as a press does. */
  readonly writes?: true;
  readonly surface: KeySurface;
}

/** An action as its scope's table writes it: the table gives its scope, and its key there its
 *  id. */
export type ScopeEntry = Omit<CatalogueEntry, "scope" | "id">;

/** One scope's actions, by id (`keymap-catalogue-<scope>.ts`). */
export type ScopeTable = Readonly<Record<string, ScopeEntry>>;

/** Each scope's table, in the order `KEYMAP_ACTIONS` lists their actions. */
const SCOPE_TABLES = {
  global: GLOBAL_ACTIONS,
  inbox: INBOX_ACTIONS,
  issue: ISSUE_ACTIONS,
  document: DOCUMENT_ACTIONS,
  project: PROJECT_ACTIONS,
  board: BOARD_ACTIONS,
  architecture: ARCHITECTURE_ACTIONS,
  agents: AGENTS_ACTIONS,
  delivery: DELIVERY_ACTIONS,
} as const satisfies Record<CatalogueScope, ScopeTable>;

type ScopeTables = typeof SCOPE_TABLES;

/** An action's name: its scope and its id, `inbox.snooze`. */
export type KeymapActionId = {
  [S in CatalogueScope]: `${S}.${keyof ScopeTables[S] & string}`;
}[CatalogueScope];

/** The entry of the action named `A`: its scope and its id, then its table's entry. */
type EntryOf<A extends KeymapActionId> = A extends `${infer S extends CatalogueScope}.${infer I}`
  ? I extends keyof ScopeTables[S]
    ? Flat<{ readonly scope: S; readonly id: I } & ScopeTables[S][I]>
    : never
  : never;

/** `T`'s properties as one object type rather than an intersection: the type an entry written
 *  out whole with `as const` has. */
type Flat<T> = { readonly [K in keyof T]: T[K] };

/** Every action by its name, `<scope>.<id>`, in the tables' order and each table's own: its scope
 *  and its id, then its table's entry. Each name is built from the scope and the id its entry
 *  gets, which the type says of every name. */
export const KEYMAP_CATALOGUE = Object.fromEntries(
  Object.entries(SCOPE_TABLES).flatMap(([scope, table]) =>
    Object.entries(table).map(([id, entry]) => [`${scope}.${id}`, { scope, id, ...entry }] as const)
  )
) as { readonly [A in KeymapActionId]: EntryOf<A> };

/** Every action, in the catalogue's order. */
export const KEYMAP_ACTIONS = Object.keys(KEYMAP_CATALOGUE) as readonly KeymapActionId[];

/** The keys a person changed, by action, and only those: `[]` is an action they took every key
 *  from, and an action that is not named takes its defaults. */
export type PersonKeys = Readonly<Partial<Record<KeymapActionId, readonly string[]>>>;

/** A person who changed nothing, and everyone while nobody is signed in. */
export const NO_PERSON_KEYS: PersonKeys = Object.freeze({});

/** The entry of `action`, read as the one shape every entry has. */
export function catalogueEntry(action: KeymapActionId): CatalogueEntry {
  return KEYMAP_CATALOGUE[action];
}

/** Whether every key of `entry` keeps its own action: it has no default key a person can change,
 *  and fixed keys its run reads through `event.key`, so it takes no key of a person's. */
export function keepsOwnKeys(entry: CatalogueEntry): boolean {
  return entry.keys.length === 0 && (entry.fixedKeys?.length ?? 0) > 0;
}

/** Whether `name` is an action of the catalogue: a stored map may hold one a later build
 *  renamed or removed. */
export function isKeymapAction(name: string): name is KeymapActionId {
  return Object.hasOwn(KEYMAP_CATALOGUE, name);
}

/** The keys that run `action` for a person: theirs where they changed them, else the defaults,
 *  then the keys nobody changes. */
export function keysOfAction(action: KeymapActionId, personKeys: PersonKeys): readonly string[] {
  const entry = catalogueEntry(action);
  const keys = personKeys[action] ?? entry.keys;
  const fixed = entry.fixedKeys ?? [];
  return fixed.length === 0 ? keys : [...keys, ...fixed];
}
