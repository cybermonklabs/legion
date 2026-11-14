import { openIssueStatuses } from "./board-model";
import { IN_FLIGHT_STAGES, SLOT_USES, STATUS_IN_FLIGHT_STAGES } from "./issues-stages";
import type { ColumnId } from "./issues-table-model";

/**
 * The Issues tab's views. A view is one type: a name, the URL parameters it stands for, the
 * columns it opens with, and what its bar counts by. The seven built-in views are constants of
 * it, and a saved view is a name and a URL query. An address names its view with `view=<id>`;
 * every other parameter of the address replaces the view's value of that same parameter, or
 * narrows the view further, so a link carries only what differs from its view.
 */

export type BuiltInViewId =
  | "open"
  | "triage"
  | "waiting"
  | "in-flight"
  | "slots"
  | "finished"
  | "mine";

/** What a view's bar counts its rows by. */
export type BarBy = "stage" | "waiting_reason" | "slot_use";

export interface BuiltInView {
  readonly id: BuiltInViewId;
  /** The tab's name: at most 14 characters. */
  readonly name: string;
  /** The view's own parameters, which an address's parameters replace one key at a time. */
  readonly params: (hasLegion: boolean) => readonly (readonly [string, string])[];
  /** The columns the view opens with, after the pinned ones, in the order they draw. */
  readonly cols: readonly ColumnId[];
  readonly bar?: BarBy;
  /** The bar writes the Not started count as a number at its left end, and the work stages
   *  fill the rest of its width. */
  readonly notStartedNumber?: boolean;
  /** The view shows only for a project with a Legion daemon. */
  readonly legionOnly?: boolean;
  /** What the (i) hint beside the tabs says the view lists. */
  readonly hint: string;
}

const OPEN_STATUSES = openIssueStatuses.map((status) => ["status", status] as const);

/** All open: today's default list, and what an address that names no view, or a view the
 *  project does not have, opens. */
const ALL_OPEN: BuiltInView = {
  id: "open",
  name: "All open",
  params: () => [...OPEN_STATUSES, ["sort", "queue"]],
  // Created and Open asks are in the Columns menu, not the defaults (the Needs you flag covers
  // the asks): the default columns fit 1440 px beside the sidebar.
  cols: ["priority", "queue", "key", "title", "stage", "epic", "assignee", "claim"],
  bar: "stage",
  notStartedNumber: true,
  hint: "Every open issue",
};

export const BUILT_IN_VIEWS: readonly BuiltInView[] = [
  ALL_OPEN,
  {
    id: "triage",
    name: "Triage",
    params: () => [
      ["status", "triage"],
      ["sort", "-created"],
    ],
    cols: ["priority", "key", "title", "author", "labels", "created"],
    hint: "New issues that no person has sorted yet",
  },
  {
    id: "waiting",
    name: "Waiting line",
    params: () => [
      ["f.queue", "Queued"],
      ["sort", "queue"],
    ],
    cols: ["queue", "priority", "key", "title", "waiting_reason", "waited", "epic"],
    bar: "waiting_reason",
    legionOnly: true,
    hint: "The issues that Legion holds before it starts them, in the order it starts them",
  },
  {
    id: "in-flight",
    name: "In flight",
    params: (hasLegion) =>
      hasLegion
        ? [
            ...IN_FLIGHT_STAGES.map((stage) => ["f.stage", stage] as const),
            ["sort", "stage,-time_in_phase"],
          ]
        : [
            ...STATUS_IN_FLIGHT_STAGES.map((stage) => ["f.stage", stage] as const),
            ["sort", "stage,-updated"],
          ],
    // Assignee is in the Columns menu, not the defaults: the default columns fit 1440 px
    // beside the sidebar.
    cols: [
      "priority",
      "key",
      "title",
      "stage",
      "slot",
      "roles",
      "ci",
      "time_in_phase",
      "phase_hours",
    ],
    bar: "stage",
    hint: "The issues in work now, from Design to Held",
  },
  {
    id: "slots",
    name: "Slots",
    params: () => [...SLOT_USES.map((use) => ["f.slot_use", use] as const), ["sort", "slot"]],
    cols: ["slot", "key", "title", "stage", "slot_use", "ci", "phase_hours"],
    bar: "slot_use",
    legionOnly: true,
    hint: "The issues that hold a slot, and what each does with it",
  },
  {
    id: "finished",
    name: "Finished",
    params: (hasLegion) => [
      ["status", "done"],
      ["f.closed", "28d"],
      ...(hasLegion ? [["f.legion", "worked"] as const] : []),
      ["sort", "-closed"],
    ],
    cols: ["key", "title", "closed", "work_hours", "review_rounds", "phase_hours", "assignee"],
    hint: "The issues closed in the last 28 days that Legion worked on",
  },
  {
    id: "mine",
    name: "Created by me",
    params: () => [...OPEN_STATUSES, ["f.author", "@me"], ["sort", "-created"]],
    cols: ["priority", "key", "title", "stage", "created"],
    bar: "stage",
    notStartedNumber: true,
    hint: "The open issues that you or your agents created",
  },
];

/** The views a project has: Waiting line and Slots only where a Legion daemon runs. */
export function viewsFor(hasLegion: boolean): readonly BuiltInView[] {
  return BUILT_IN_VIEWS.filter((view) => hasLegion || view.legionOnly !== true);
}

/** The view `id` names, or All open for an id the project does not have. */
export function viewFor(id: string | null, hasLegion: boolean): BuiltInView {
  return viewsFor(hasLegion).find((view) => view.id === id) ?? ALL_OPEN;
}

/** The parameters that are no part of what a view lists: they mark no view as changed. */
const DISPLAY_KEYS = ["view", "page", "size", "cols", "layout"];

/**
 * The address with its view written out: the view's own parameters, each replaced by the
 * address's value of that key where it has one. A key the address gives as one empty value
 * (`f.stage=`) takes the view's value away, and `nofilter=1` lifts the view's Status filter.
 */
export function expandView(params: URLSearchParams, hasLegion: boolean): URLSearchParams {
  const view = viewFor(params.get("view"), hasLegion);
  const expanded = new URLSearchParams(view.params(hasLegion).map(([key, value]) => [key, value]));
  if (params.get("nofilter") === "1") expanded.delete("status");
  for (const key of new Set(params.keys())) {
    if (key === "view" || key === "nofilter") continue;
    const values = params.getAll(key);
    expanded.delete(key);
    if (values.length === 1 && values[0] === "") continue;
    for (const value of values) expanded.append(key, value);
  }
  return expanded;
}

/** Two spellings of one key's values that say the same: the sort in its order, every other key
 *  as a set. */
function sameValues(key: string, a: readonly string[], b: readonly string[]): boolean {
  if (key === "sort") return a.join(",") === b.join(",");
  return a.length === b.length && [...a].sort().join("\n") === [...b].sort().join("\n");
}

/** `full`, which spells the table's whole view and names its view with `view=`, written as the
 *  difference from that view: what the view says already is left out. */
export function compressView(full: URLSearchParams, hasLegion: boolean): URLSearchParams {
  const view = viewFor(full.get("view"), hasLegion);
  const own = new URLSearchParams(view.params(hasLegion).map(([key, value]) => [key, value]));
  const next = new URLSearchParams();
  if (view.id !== ALL_OPEN.id) next.set("view", view.id);
  for (const key of new Set([...full.keys(), ...own.keys()])) {
    if (key === "view") continue;
    const values = full.getAll(key);
    if (sameValues(key, values, own.getAll(key))) continue;
    if (values.length === 0) {
      if (key === "status") next.set("nofilter", "1");
      else next.set(key, "");
      continue;
    }
    for (const value of values) next.append(key, value);
  }
  return next;
}

/** A built-in view is changed when any parameter but the page, its size, the columns and the
 *  layout differs from the view's own; the address carries only what differs. */
export function isViewChanged(params: URLSearchParams): boolean {
  return [...params.keys()].some((key) => !DISPLAY_KEYS.includes(key));
}

/** The address of the view as it opens, keeping the columns and the layout the address holds. */
export function resetView(params: URLSearchParams, hasLegion: boolean): URLSearchParams {
  const view = viewFor(params.get("view"), hasLegion);
  const next = new URLSearchParams();
  if (view.id !== ALL_OPEN.id) next.set("view", view.id);
  for (const key of ["cols", "layout"])
    for (const value of params.getAll(key)) next.append(key, value);
  return next;
}

/** A Closed window: from (included) and to (excluded), in milliseconds, either end open. */
export interface ClosedWindow {
  readonly from: number | null;
  readonly to: number | null;
}

const DAY_MS = 86_400_000;

/** One end of a `<from>..<to>` window: a UTC date (`2026-11-03`), or an RFC 3339 time written in
 *  UTC with `Z`. */
function readEnd(text: string): number | null | undefined {
  if (text === "") return null;
  const at = /^\d{4}-\d{2}-\d{2}$/.test(text)
    ? Date.parse(`${text}T00:00:00Z`)
    : /Z$/.test(text)
      ? Date.parse(text)
      : Number.NaN;
  return Number.isNaN(at) ? undefined : at;
}

/** The window `f.closed` names: `<N>d` is the last N times 24 hours up to now, `<from>..<to>` a
 *  range, and `all` (or a value this cannot read) no window at all. */
export function readWindow(value: string, nowMs: number): ClosedWindow | undefined {
  const days = /^(\d+)d$/.exec(value);
  if (days !== null) return { from: nowMs - Number(days[1]) * DAY_MS, to: null };
  const range = /^(.*)\.\.(.*)$/.exec(value);
  if (range === null) return undefined;
  const from = readEnd(range[1] ?? "");
  const to = readEnd(range[2] ?? "");
  return from === undefined || to === undefined ? undefined : { from, to };
}

/** Whether a row closed at `closedAt` is in the window `value` names. With a window, a row that
 *  never closed is in none. */
export function inWindow(closedAt: string | null, value: string, nowMs: number): boolean {
  const window = readWindow(value, nowMs);
  if (window === undefined) return true;
  if (closedAt === null) return false;
  const at = Date.parse(closedAt);
  return (window.from === null || at >= window.from) && (window.to === null || at < window.to);
}

/** The read a window makes of the listing: `closed_since` and, for a window with an end,
 *  `closed_before`. A rolling window starts on the hour before, so the read's key moves once an
 *  hour rather than on every render, and the rows it reads are a superset of the window's. */
export function windowRead(
  value: string,
  nowMs: number
): { readonly closed_since?: string; readonly closed_before?: string } | undefined {
  const window = readWindow(value, nowMs);
  if (window === undefined) return undefined;
  const hour = 3_600_000;
  return {
    ...(window.from === null
      ? {}
      : { closed_since: new Date(Math.floor(window.from / hour) * hour).toISOString() }),
    ...(window.to === null ? {} : { closed_before: new Date(window.to).toISOString() }),
  };
}

/** A view saved by the reader: its name and the URL query that opens it. */
export interface SavedView {
  readonly name: string;
  readonly query: string;
}

/** What `project.saved-views` keeps: a version, and each project's saved views. */
export interface StoredViews {
  readonly version: 1;
  readonly projects: Readonly<Record<string, readonly SavedView[]>>;
}

export const NO_SAVED_VIEWS: StoredViews = { version: 1, projects: {} };

/** The longest name a saved view may have, which is the longest a view tab shows. */
export const VIEW_NAME_MAX = 14;

/** `project.saved-views` as stored, with every entry that does not parse dropped. */
export function readSavedViews(stored: string | null): StoredViews {
  if (stored === null) return NO_SAVED_VIEWS;
  let value: unknown;
  try {
    value = JSON.parse(stored);
  } catch {
    return NO_SAVED_VIEWS;
  }
  if (typeof value !== "object" || value === null || !("version" in value) || value.version !== 1)
    return NO_SAVED_VIEWS;
  if (!("projects" in value) || typeof value.projects !== "object" || value.projects === null)
    return NO_SAVED_VIEWS;
  const kept: Record<string, SavedView[]> = {};
  for (const [project, views] of Object.entries(value.projects)) {
    if (!Array.isArray(views)) continue;
    kept[project] = views.flatMap((view: unknown): SavedView[] =>
      typeof view === "object" &&
      view !== null &&
      "name" in view &&
      "query" in view &&
      typeof view.name === "string" &&
      view.name.trim() !== "" &&
      view.name.length <= VIEW_NAME_MAX &&
      typeof view.query === "string"
        ? [{ name: view.name, query: view.query }]
        : []
    );
  }
  return { version: 1, projects: kept };
}

/** Why `name` cannot name a saved view, or undefined where it can. A name already saved is not
 *  refused here: saving under it asks to replace it. */
export function viewNameProblem(name: string): string | undefined {
  const trimmed = name.trim();
  if (trimmed === "") return "Type a name";
  if (trimmed.length > VIEW_NAME_MAX) return `Use ${VIEW_NAME_MAX} characters or fewer`;
  if (BUILT_IN_VIEWS.some((view) => view.name.toLowerCase() === trimmed.toLowerCase()))
    return "A built-in view has this name";
  return undefined;
}

/** The address of a saved view's query, page and size left out: two addresses that list the
 *  same rows in the same columns and layout are one saved view. */
export function viewQueryKey(params: URLSearchParams): string {
  const kept = [...params.entries()].filter(([key]) => key !== "page" && key !== "size");
  kept.sort(([a, x], [b, y]) => (a === b ? x.localeCompare(y) : a.localeCompare(b)));
  return new URLSearchParams(kept).toString();
}
