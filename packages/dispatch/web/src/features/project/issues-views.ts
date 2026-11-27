import { useCallback, useMemo, useState } from "react";
import { readStore, writeStore } from "../../components/table-storage";
import {
  commaListParam,
  copyAddress,
  repeatedParam,
  type TableAddressGrammar,
  type TableAddressView,
  textParam,
  useAddressFields,
  useAddressParam,
} from "../../lib/page-address";
import { useStoredUserPreference, useUserPreference } from "../shell/userPreference";
import { type IssueStatus, openIssueStatuses } from "./board-model";
import { sameSet } from "./issues-filter-picks";
import { type ColumnId, isBoardFedColumn } from "./issues-table-model";

/**
 * The Issues tab's views, and what the address says about them.
 *
 * A view is a name, the parameters it stands for, the columns it opens with, and whether it
 * draws the bar of its rows by status. The two built-in views are constants of it; a saved view
 * is a name and an address query, kept in the reader's browser. An address names its view with
 * `view=<id>` and carries only what differs from it: a parameter the view has (`status`, `sort`)
 * replaces the view's value of it, and every other parameter narrows the view further. The
 * project's issue table reads and writes its own spelling of the whole view (`readUrl` and
 * `writeUrl` in `issues-table-model.ts`, whose defaults are Open's), and `viewsGrammar` puts the
 * view between that spelling and the address. The table model imports nothing of this module
 * (its filter picks are `issues-filter-picks.ts`'s), so this module reads the model's columns
 * (`isBoardFedColumn`).
 */

export type BuiltInViewId = "open" | "done";

export interface BuiltInView {
  readonly id: BuiltInViewId;
  /** The tab's name: at most 14 characters (`VIEW_NAME_MAX`). */
  readonly name: string;
  /** The statuses the view lists, which an address's `status` replaces and `nofilter=1` lifts. */
  readonly statuses: readonly IssueStatus[];
  /** The view's sort, which an address's `sort` replaces (`sort=` sorts by nothing). */
  readonly sort: string;
  /** The columns the view opens with after Select, in the order they draw (`viewColumns`). */
  readonly cols: readonly ColumnId[];
  /** It draws the bar of its rows by status under the tabs. */
  readonly bar: boolean;
}

/** Open: today's default list, and what an address that names no view, or a view this build
 *  does not know, opens. Its statuses and sort are the project table's own defaults
 *  (`defaultView` in `issues-table-model.ts`), so an address that names neither lists them. */
const OPEN: BuiltInView = {
  id: "open",
  name: "Open",
  statuses: openIssueStatuses,
  sort: "queue",
  cols: [
    "priority",
    "key",
    "title",
    "queue",
    "status",
    "author",
    "asks",
    "legion_phase",
    "assignee",
    "labels",
    "claim",
    "created",
  ],
  bar: true,
};

/** Done: every done issue, the newest close first. The Closed filter narrows it to a window. */
const DONE: BuiltInView = {
  id: "done",
  name: "Done",
  statuses: ["done"],
  sort: "-closed",
  cols: [
    "priority",
    "key",
    "title",
    "closed",
    "phase_hours",
    "work_hours",
    "assignee",
    "author",
    "labels",
  ],
  bar: false,
};

/** The built-in views, in their tabs' order. Any other list is a filter of one of them, which a
 *  reader saves as a view of their own. */
export const BUILT_IN_VIEWS: readonly BuiltInView[] = [OPEN, DONE];

/** The view `id` names, or Open for no id or an id this build does not know
 *  (`view=in-flight`). */
export function viewFor(id: string | null): BuiltInView {
  return BUILT_IN_VIEWS.find((view) => view.id === id) ?? OPEN;
}

/** The address a built-in view opens at: Open's is empty, Done's is `view=done`. */
export function builtInAddress(id: BuiltInViewId): URLSearchParams {
  const address = copyAddress();
  if (id !== OPEN.id) address.set("view", id);
  return address;
}

/** The columns a view opens with: Select, which every view shows so the bulk actions stay, then
 *  the view's own. In a project with no Legion daemon Queue and every board-fed column are left
 *  out; Queue stays in the Columns menu. */
export function viewColumns(view: BuiltInView, hasLegion: boolean): readonly ColumnId[] {
  return [
    "select",
    ...view.cols.filter((id) => hasLegion || (id !== "queue" && !isBoardFedColumn(id))),
  ];
}

const COLS = commaListParam();

/** The columns `cols=` names, the whole set shown for this load, or undefined where the address
 *  names none. Select is not named: it always shows. */
export function readCols(params: URLSearchParams): readonly string[] | undefined {
  const cols = [...new Set(COLS.read(params, "cols"))].filter((id) => id !== "select");
  return cols.length === 0 ? undefined : cols;
}

/** The columns a view shows for this load: the address's `cols=`, else the view's own. Neither
 *  shows a board-fed column in a project with no Legion daemon. */
export function shownColumns(
  params: URLSearchParams,
  view: BuiltInView,
  hasLegion: boolean
): readonly string[] {
  const cols = readCols(params);
  if (cols === undefined) return viewColumns(view, hasLegion);
  return ["select", ...cols.filter((id) => hasLegion || !isBoardFedColumn(id))];
}

/** The ids older links name the Legion queue's tab by. That tab is gone: Open narrowed to the
 *  issues in the queue lists the same rows in the same order, since Open sorts by Queue. */
const QUEUE_VIEW_IDS: readonly string[] = ["queue", "waiting"];

/** The filters and the grouping of columns this build does not have, which an address drops. */
const DROPPED_FILTERS: readonly string[] = ["f.stage", "f.epic"];
const DROPPED_GROUP = "epic";

/**
 * An older address in today's spelling, or undefined where it is today's already:
 * `view=queue` and `view=waiting` open Open with `f.queue=Queued`, and `f.stage`, `f.epic` and
 * `group=epic` are dropped. The page writes this over the address it opens on, so the tab shows
 * its mark and Clear filters takes the Queue filter off.
 */
export function currentAddress(params: URLSearchParams): URLSearchParams | undefined {
  const id = params.get("view");
  const queue = id !== null && QUEUE_VIEW_IDS.includes(id);
  const groups = params.getAll("group");
  const droppedGrouping = groups.includes(DROPPED_GROUP);
  const dropped = DROPPED_FILTERS.filter((key) => params.has(key));
  if (!queue && !droppedGrouping && dropped.length === 0) return undefined;
  const next = copyAddress(params);
  if (queue) {
    next.delete("view");
    if (!next.has("f.queue")) next.set("f.queue", "Queued");
  }
  if (droppedGrouping) {
    next.delete("group");
    for (const group of groups) if (group !== DROPPED_GROUP) next.append("group", group);
  }
  for (const key of dropped) next.delete(key);
  return next;
}

/** The parameters a view has a value of, which `expandView` and `compressView` write. */
const VIEW_OWN_KEYS: readonly string[] = ["view", "status", "nofilter", "sort"];

/** The statuses an address lists, `fallback` where it names none, or null where the Status
 *  filter is lifted: by `nofilter=1`, or by a `status=` with no value, which takes the view's
 *  own value away. A named status wins over `nofilter=1`, as `readUrl` reads it. */
function spelledStatuses(
  params: URLSearchParams,
  fallback: readonly string[]
): readonly string[] | null {
  const named = params.getAll("status").filter((status) => status !== "");
  if (named.length > 0) return named;
  if (params.get("nofilter") === "1" || params.has("status")) return null;
  return fallback;
}

/**
 * The address with its view written out in the project table's spelling: `view=<id>` of the view
 * it opens (`viewFor`, after `currentAddress`), the view's statuses and sort where the address
 * names none of its own, and every other parameter as the address has it. So an explicit
 * `status` or `sort` replaces the view's value, `nofilter=1` lifts the view's statuses, and
 * every other parameter narrows the view.
 */
export function expandView(params: URLSearchParams): URLSearchParams {
  const address = currentAddress(params) ?? params;
  const view = viewFor(address.get("view"));
  const expanded = copyAddress();
  expanded.set("view", view.id);
  const statuses = spelledStatuses(address, view.statuses);
  if (statuses === null) expanded.set("nofilter", "1");
  else for (const status of statuses) expanded.append("status", status);
  expanded.set("sort", address.get("sort") ?? view.sort);
  for (const [key, value] of address) if (!VIEW_OWN_KEYS.includes(key)) expanded.append(key, value);
  return expanded;
}

/**
 * `full`, an address in the project table's spelling that names its view with `view=`, written
 * as the difference from that view: what the view says already is left out. The table's
 * spelling leaves out its defaults, Open's statuses and sort, so a `full` that names no status
 * or no sort has Open's. A Status filter lifted is `nofilter=1`, and a sort the view does not
 * have is written even when it is none (`sort=`). Open is the address with no `view`.
 */
export function compressView(full: URLSearchParams): URLSearchParams {
  const view = viewFor(full.get("view"));
  const next = builtInAddress(view.id);
  const statuses = spelledStatuses(full, OPEN.statuses);
  const own: readonly string[] = view.statuses;
  if (statuses === null) next.set("nofilter", "1");
  else if (!sameSet(statuses, own)) for (const status of statuses) next.append("status", status);
  const sort = full.get("sort") ?? OPEN.sort;
  if (sort !== view.sort) next.set("sort", sort);
  for (const [key, value] of full) if (!VIEW_OWN_KEYS.includes(key)) next.append(key, value);
  return next;
}

/**
 * The project table's grammar with the views between it and the address: `project` reads and
 * writes the whole view in the table's own spelling, and the address holds only what differs
 * from the view it names. A module-level constant of the caller's, since `useTableAddress`
 * memoizes what it reads on the grammar's identity.
 */
export function viewsGrammar<V extends TableAddressView>(
  project: TableAddressGrammar<V>
): TableAddressGrammar<V> {
  return {
    read: (params) => project.read(expandView(params)),
    write: (params, view) => compressView(project.write(expandView(params), view)),
  };
}

/** The parameters that are no part of what a view lists: they mark no view as changed. */
const DISPLAY_KEYS: readonly string[] = ["view", "page", "size", "cols", "layout"];

/** A built-in view is changed when any parameter but the page, its size, the columns and the
 *  layout differs from the view's own. */
export function isViewChanged(params: URLSearchParams): boolean {
  return [...compressView(expandView(params)).keys()].some((key) => !DISPLAY_KEYS.includes(key));
}

/** What Clear filters leaves as it is: the view, its sort and grouping, and what is no part of
 *  what a view lists. Everything else is a narrowing: a filter, the search, a flag or a chip. */
const KEPT_BY_CLEAR: readonly string[] = [...DISPLAY_KEYS, "sort", "group"];

/** The address's entries other than `skipped`, in one order, with `cols=` as a set: two
 *  addresses that say the same give the same text. */
function entriesKey(params: URLSearchParams, skipped: readonly string[]): string {
  const entries = [...params]
    .filter(([key]) => !skipped.includes(key))
    .map(([key, value]): [string, string] => [
      key,
      key === "cols" ? [...new Set(value.split(","))].sort().join(",") : value,
    ])
    .sort(([a, x], [b, y]) => (a === b ? x.localeCompare(y) : a.localeCompare(b)));
  const key = copyAddress();
  for (const [name, value] of entries) key.append(name, value);
  return key.toString();
}

/** Whether the address's narrowings differ from those of `own`, the address the view opens at
 *  (`builtInAddress`, or a saved view's query): while they do, the control row shows Clear
 *  filters. */
export function narrowingsDiffer(params: URLSearchParams, own: URLSearchParams): boolean {
  return (
    entriesKey(expandView(params), KEPT_BY_CLEAR) !== entriesKey(expandView(own), KEPT_BY_CLEAR)
  );
}

/**
 * The address with its narrowings taken back to those of `own`, the address the view opens at:
 * its filters, search, flags and chips. The sort and the grouping stay as the reader left them,
 * and so do the columns, the layout and the page size; the page goes back to the first.
 */
export function clearNarrowings(params: URLSearchParams, own: URLSearchParams): URLSearchParams {
  const next = copyAddress();
  for (const [key, value] of own)
    if (key === "view" || !KEPT_BY_CLEAR.includes(key)) next.append(key, value);
  for (const key of ["sort", "group", "cols", "layout", "size"])
    for (const value of params.getAll(key)) next.append(key, value);
  return next;
}

/** How a view draws its rows: the table, or the Board. */
export type ViewLayout = "table" | "board";

/** A stored or written layout, or undefined for anything else. */
function readViewLayout(value: unknown): ViewLayout | undefined {
  return value === "table" || value === "board" ? value : undefined;
}

/** The view the tabs have open: a built-in one, or a saved one by its name. */
export type ActiveView =
  | { readonly kind: "built-in"; readonly id: BuiltInViewId }
  | { readonly kind: "saved"; readonly name: string };

/** Where a built-in view keeps its layout in this browser. Open keeps the project's table key
 *  (`tableKey`, `tableStorageKey` in `issues-table-model.ts`), so the layout stored before the
 *  views is Open's with no write; Done keeps its own beside it, `<tableKey>:view=done`. A saved
 *  view keeps none: its columns and layout are in its query. */
export function viewLayoutKey(tableKey: string, view: BuiltInViewId): string {
  return view === OPEN.id ? tableKey : `${tableKey}:view=${view}`;
}

/** The `project.issue-view` choice kept before the views, List or Board, as Open's layout. */
export function legacyOpenLayout(stored: string | null): ViewLayout {
  return stored === "board" ? "board" : "table";
}

const legacyWrite = (layout: ViewLayout): string => (layout === "board" ? "board" : "list");

/** The address's `layout=`, which holds a view's layout for one load. A module-level constant,
 *  for the reason `useAddressParam` gives. */
const LAYOUT_PARAM = textParam();

/**
 * The layout the view draws in, and the one way to change it. The address's `layout=` holds for
 * this load: a change writes the address and nothing is kept. Else a built-in view takes the
 * layout kept under its key (`viewLayoutKey`), Open the `project.issue-view` choice where it
 * keeps none, and a change is kept under that key. A saved view's layout is its query's, in the
 * address, so a change writes the address. With no choice anywhere a view draws the table.
 */
export function useViewLayout(
  tableKey: string,
  active: ActiveView
): readonly [ViewLayout, (next: ViewLayout) => void] {
  const [addressLayout, writeAddressLayout] = useAddressParam("layout", LAYOUT_PARAM);
  const key = active.kind === "built-in" ? viewLayoutKey(tableKey, active.id) : undefined;
  const isOpen = active.kind === "built-in" && active.id === OPEN.id;
  const [legacy] = useUserPreference("project.issue-view", legacyOpenLayout, legacyWrite);
  const stored = useMemo(
    () => (key === undefined ? undefined : readViewLayout(readStore(key).layout)),
    [key]
  );
  const [kept, setKept] = useState<Readonly<Record<string, ViewLayout>>>({});
  const fromAddress = readViewLayout(addressLayout);
  const layout =
    fromAddress ??
    (key === undefined ? "table" : (kept[key] ?? stored ?? (isOpen ? legacy : "table")));
  const setLayout = useCallback(
    (next: ViewLayout) => {
      if (key === undefined || fromAddress !== undefined) {
        writeAddressLayout(next);
        return;
      }
      writeStore(key, { layout: next });
      setKept((current) => ({ ...current, [key]: next }));
    },
    [fromAddress, key, writeAddressLayout]
  );
  return [layout, setLayout] as const;
}

/** The narrowing of the Evals tab's links that leaves out the rows of a tree: repeatable, one
 *  root key each. */
const EXCLUDE_TREE = "exclude_tree";

const EXCLUDED_TREES = { [EXCLUDE_TREE]: repeatedParam() };

/** Whether a row is in one of the `excluded` trees: its key is one of their roots, or its facts'
 *  `root_key` is. */
export function inExcludedTree(
  key: string,
  rootKey: string | undefined,
  excluded: readonly string[]
): boolean {
  return excluded.includes(key) || (rootKey !== undefined && excluded.includes(rootKey));
}

/** The trees `params` leaves out (`exclude_tree`), as the address holds them. */
export function readExcludedTrees(params: URLSearchParams): readonly string[] {
  return EXCLUDED_TREES[EXCLUDE_TREE].read(params, EXCLUDE_TREE);
}

/** The trees the address leaves out (`exclude_tree`), and the remove of one, which its chip
 *  calls. */
export function useExcludedTrees(): readonly [readonly string[], (tree: string) => void] {
  const [values, setValues] = useAddressFields(EXCLUDED_TREES);
  const trees = values[EXCLUDE_TREE];
  const remove = useCallback(
    (tree: string) => setValues({ [EXCLUDE_TREE]: trees.filter((one) => one !== tree) }),
    [setValues, trees]
  );
  return [trees, remove] as const;
}

/** A view saved by the reader: its name and the address query that opens it, which holds its
 *  parameters, `cols=` and `layout=`. */
export interface SavedView {
  readonly name: string;
  readonly query: string;
}

/** What `project.saved-views` keeps: a version, and each project's saved views by its key. */
export interface StoredViews {
  readonly version: 1;
  readonly projects: Readonly<Record<string, readonly SavedView[]>>;
}

export const NO_SAVED_VIEWS: StoredViews = { version: 1, projects: {} };

const NO_VIEWS: readonly SavedView[] = [];

/** The longest name a saved view may have, in characters: the longest a view tab shows. */
export const VIEW_NAME_MAX = 14;

/** What a name for a saved view does: it cannot be used, it names a new view, or it is the name
 *  of a saved view (`replaces`), which saving under it asks to replace. */
export type ViewNameCheck =
  | { readonly kind: "problem"; readonly problem: string }
  | { readonly kind: "new"; readonly name: string }
  | { readonly kind: "replace"; readonly name: string; readonly replaces: string };

const sameName = (a: string, b: string) => a.trim().toLowerCase() === b.trim().toLowerCase();

/** Whether `name` can name a saved view among `saved`, the project's: at most 14 characters, no
 *  built-in view's name, and a name in use is a replace. Names compare without regard to case or
 *  the spaces around them. */
export function checkViewName(name: string, saved: readonly SavedView[]): ViewNameCheck {
  const trimmed = name.trim();
  if (trimmed === "") return { kind: "problem", problem: "Type a name." };
  if ([...trimmed].length > VIEW_NAME_MAX)
    return { kind: "problem", problem: `Use ${VIEW_NAME_MAX} characters or fewer.` };
  const builtIn = BUILT_IN_VIEWS.find((view) => sameName(view.name, trimmed));
  if (builtIn !== undefined)
    return {
      kind: "problem",
      problem: `${builtIn.name} is a built-in view. Type a different name.`,
    };
  const used = saved.find((view) => sameName(view.name, trimmed));
  return used === undefined
    ? { kind: "new", name: trimmed }
    : { kind: "replace", name: trimmed, replaces: used.name };
}

/** One project's saved views out of the store. */
function savedViewsOf(stored: StoredViews, project: string): readonly SavedView[] {
  return Object.hasOwn(stored.projects, project)
    ? (stored.projects[project] ?? NO_VIEWS)
    : NO_VIEWS;
}

/** One stored entry as a saved view, or undefined where it does not parse: its name a string as
 *  `checkViewName` takes it, new among `kept`, and its query a string. */
function readSavedView(entry: unknown, kept: readonly SavedView[]): SavedView | undefined {
  if (typeof entry !== "object" || entry === null) return undefined;
  if (!("name" in entry) || !("query" in entry)) return undefined;
  const { name, query } = entry;
  if (typeof name !== "string" || typeof query !== "string") return undefined;
  const check = checkViewName(name, kept);
  return check.kind === "new" && check.name === name ? { name, query } : undefined;
}

/** `project.saved-views` as stored, with every entry that does not parse dropped. A value of
 *  another version, or that is no object, is no saved view at all. */
export function readSavedViews(stored: string | null): StoredViews {
  if (stored === null) return NO_SAVED_VIEWS;
  let value: unknown;
  try {
    value = JSON.parse(stored);
  } catch {
    return NO_SAVED_VIEWS;
  }
  if (typeof value !== "object" || value === null || !("version" in value)) return NO_SAVED_VIEWS;
  if (value.version !== 1 || !("projects" in value)) return NO_SAVED_VIEWS;
  const { projects } = value;
  if (typeof projects !== "object" || projects === null || Array.isArray(projects))
    return NO_SAVED_VIEWS;
  const kept = Object.entries(projects).flatMap(
    ([project, entries]): [string, readonly SavedView[]][] => {
      if (!Array.isArray(entries)) return [];
      const views: SavedView[] = [];
      for (const entry of entries) {
        const view = readSavedView(entry, views);
        if (view !== undefined) views.push(view);
      }
      return [[project, views]];
    }
  );
  return { version: 1, projects: Object.fromEntries(kept) };
}

/** `project.saved-views` as it is written. */
export function writeSavedViews(value: StoredViews): string {
  return JSON.stringify(value);
}

/** The store with `view` saved in `project`: in the place of the saved view of the same name,
 *  or after the project's others. */
export function withSavedView(stored: StoredViews, project: string, view: SavedView): StoredViews {
  const saved = savedViewsOf(stored, project);
  const at = saved.findIndex((one) => sameName(one.name, view.name));
  const views =
    at === -1 ? [...saved, view] : saved.map((one, index) => (index === at ? view : one));
  return { version: 1, projects: { ...stored.projects, [project]: views } };
}

/** The store without the saved view `name` of `project`. */
export function withoutSavedView(stored: StoredViews, project: string, name: string): StoredViews {
  const views = savedViewsOf(stored, project).filter((one) => !sameName(one.name, name));
  return { version: 1, projects: { ...stored.projects, [project]: views } };
}

/** The query a view is saved under, from the address it is on: the address in today's spelling
 *  (`currentAddress`) less its page, with the columns and the layout it draws in. A `cols=` the
 *  address carries is kept; else `display.cols`, the columns shown, names them. */
export function savedViewQuery(
  params: URLSearchParams,
  display: { readonly cols: readonly string[]; readonly layout: ViewLayout }
): string {
  const query = currentAddress(params) ?? copyAddress(params);
  query.delete("page");
  if (!query.has("cols")) query.set("cols", display.cols.filter((id) => id !== "select").join(","));
  query.set("layout", display.layout);
  return query.toString();
}

/** An address as a saved view compares it: its view written as the difference from its
 *  built-in view, less the page and its size. */
const savedViewKey = (params: URLSearchParams) =>
  entriesKey(compressView(expandView(params)), ["page", "size"]);

/** A saved view is changed when the address differs from its query in any parameter but the
 *  page and its size: its columns and layout included. */
export function isSavedViewChanged(params: URLSearchParams, view: SavedView): boolean {
  return savedViewKey(params) !== savedViewKey(copyAddress(view.query));
}

/** The saved view whose query the address is, where one is. */
export function matchSavedView(
  saved: readonly SavedView[],
  params: URLSearchParams
): SavedView | undefined {
  const key = savedViewKey(params);
  return saved.find((view) => savedViewKey(copyAddress(view.query)) === key);
}

/** One project's saved views, kept with the signed-in login in this browser, with their name
 *  check, the save and the delete. Every caller reads what is stored now, in this tab and in
 *  every other tab of the browser (`useStoredUserPreference`), and a save or a delete changes the
 *  stored list at the moment it writes, by its own name alone: a view saved in another tab stays. */
export interface SavedViews {
  readonly views: readonly SavedView[];
  readonly check: (name: string) => ViewNameCheck;
  /** Saves `query` under `name` when `check` allows it, in the place of a saved view of that
   *  name, and answers what `check` answers. */
  readonly save: (name: string, query: string) => ViewNameCheck;
  readonly remove: (name: string) => void;
}

export function useSavedViews(project: string): SavedViews {
  const [stored, change] = useStoredUserPreference<StoredViews>(
    "project.saved-views",
    readSavedViews,
    writeSavedViews
  );
  const views = savedViewsOf(stored, project);
  const check = useCallback((name: string) => checkViewName(name, views), [views]);
  const save = useCallback(
    (name: string, query: string) => {
      const checked = checkViewName(name, views);
      if (checked.kind !== "problem")
        change((now) => withSavedView(now, project, { name: checked.name, query }));
      return checked;
    },
    [change, project, views]
  );
  const remove = useCallback(
    (name: string) => change((now) => withoutSavedView(now, project, name)),
    [change, project]
  );
  return { views, check, save, remove };
}
