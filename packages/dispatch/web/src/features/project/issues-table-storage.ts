import type { ColumnOrderState, ColumnSizingState, VisibilityState } from "@tanstack/react-table";
import { PINNED } from "./issues-table-model";
import type { BuiltInViewId } from "./issues-table-view-spec";

/**
 * The one owner of what the table keeps in the browser for a project: the layout the reader
 * dragged and the record of an action a reload interrupted, under one key, read and written
 * one typed field at a time.
 *
 * One failure rule, for every field: a browser that refuses storage, a full quota or a corrupt
 * value is a table with no saved preferences, never an exception out of a click. The URL still
 * carries the view, and the action record is only a convenience for finishing what a reload
 * cut short.
 */
/** Version two stores only reader changes; version-one snapshots of old defaults are ignored. */
export const tableStorageKey = (project: string) => `dispatch.project-table.v2.${project}`;

/** The storage key's project segment: All open keeps today's key and every other view is separate. */
export function layoutKeyOf(project: string, view: BuiltInViewId): string {
  return view === "open" ? project : `${project}.${view}`;
}

export interface TableLayout {
  sizing: ColumnSizingState;
  order: ColumnOrderState;
  visibility: VisibilityState;
}
/** The kinds of action a reload can interrupt. The union and the check a stored record passes
 *  are one list, so neither can gain a kind the other refuses. */
export const ACTION_KINDS = ["newEpic", "startNext", "merge"] as const;
export interface TableAction {
  id: string;
  kind: (typeof ACTION_KINDS)[number];
  project: string;
  keys: readonly string[];
  source?: string;
  target?: string;
  /** When the action was begun, as epoch milliseconds. A record older than `ACTION_LIFETIME_MS`
   *  is a tab somebody closed a day ago, not work this reader left half-done. */
  startedAt?: number;
  /** The merge reason exactly as it was sent, so a resume knows the sentence it is looking for
   *  even if the target has been renamed, or closed, since. */
  reasonBody?: string;
  writes: Record<string, string>;
}
/** How long a stored action is still the reader's to finish: a day. Past that the banner goes
 *  and a new action may begin, because the writes it named have long since landed or been
 *  discarded in Pending changes. */
export const ACTION_LIFETIME_MS = 24 * 60 * 60 * 1000;

const EMPTY_LAYOUT: TableLayout = { sizing: {}, order: [], visibility: {} };

function readStore(project: string): Record<string, unknown> {
  try {
    const stored: unknown = JSON.parse(localStorage.getItem(tableStorageKey(project)) ?? "{}");
    return stored !== null && typeof stored === "object" && !Array.isArray(stored)
      ? (stored as Record<string, unknown>)
      : {};
  } catch {
    return {};
  }
}
function writeStore(project: string, fields: Record<string, unknown>): void {
  try {
    localStorage.setItem(
      tableStorageKey(project),
      JSON.stringify({ ...readStore(project), ...fields })
    );
  } catch {
    /* The URL still works when the browser refuses local preferences. */
  }
}

/**
 * What the reader saved for this table, over what the surface starts with. `hiddenByDefault`
 * is the surface's own rule — the Issues tab hides Updated and the Legion columns, a grid on a
 * page hides nothing — and a column the reader has since shown or hidden wins over it.
 *
 * A width, an order and a visibility are kept for any column id, not only the registry's, so a
 * grid's own column keeps what the reader did to it.
 */
export function readLayout(project: string, hiddenByDefault: VisibilityState = {}): TableLayout {
  const saved = readStore(project);
  const asRecord = (value: unknown): Record<string, unknown> =>
    value !== null && typeof value === "object" && !Array.isArray(value)
      ? (value as Record<string, unknown>)
      : {};
  const sizing = Object.fromEntries(
    Object.entries(asRecord(saved.sizing)).filter(
      ([, size]) => typeof size === "number" && Number.isFinite(size) && size >= 36 && size <= 2000
    )
  );
  const order = Array.isArray(saved.order)
    ? [
        ...new Set<string>(
          saved.order.filter((id: unknown): id is string => typeof id === "string")
        ),
      ]
    : [];
  const visibility = Object.fromEntries(
    Object.entries(asRecord(saved.visibility)).filter(
      ([id, visible]) => !PINNED.includes(id) && typeof visible === "boolean"
    )
  );
  return {
    sizing: sizing as ColumnSizingState,
    order,
    visibility: {
      ...EMPTY_LAYOUT.visibility,
      ...hiddenByDefault,
      ...visibility,
    } as VisibilityState,
  };
}

export function writeLayout(
  project: string,
  { sizing, order, visibility }: TableLayout,
  hiddenByDefault: VisibilityState = {}
): void {
  const changedVisibility = Object.fromEntries(
    Object.entries(visibility).filter(([id, visible]) => hiddenByDefault[id] !== visible)
  );
  if (
    Object.keys(sizing).length === 0 &&
    order.length === 0 &&
    Object.keys(changedVisibility).length === 0
  ) {
    const stored = readStore(project);
    const { order: _order, sizing: _sizing, visibility: _visibility, ...other } = stored;
    try {
      if (Object.keys(other).length === 0) localStorage.removeItem(tableStorageKey(project));
      else localStorage.setItem(tableStorageKey(project), JSON.stringify(other));
    } catch {
      /* The URL still works when the browser refuses local preferences. */
    }
    return;
  }
  writeStore(project, { sizing, order, visibility: changedVisibility });
}

export function readPhaseMode(layoutKey: string): "share" | "hours" {
  return readStore(layoutKey).phaseMode === "hours" ? "hours" : "share";
}

export function writePhaseMode(layoutKey: string, mode: "share" | "hours"): void {
  writeStore(layoutKey, { phaseMode: mode });
}

export function readTableAction(project: string): TableAction | undefined {
  const action = readStore(project).action as TableAction | undefined;
  if (
    action?.project === project &&
    typeof action.id === "string" &&
    (ACTION_KINDS as readonly string[]).includes(action.kind) &&
    Array.isArray(action.keys) &&
    action.keys.every((key: unknown) => typeof key === "string") &&
    action.writes &&
    typeof action.writes === "object" &&
    !Array.isArray(action.writes) &&
    Object.values(action.writes).every((key) => typeof key === "string") &&
    (action.target === undefined || typeof action.target === "string") &&
    (action.startedAt === undefined ||
      (typeof action.startedAt === "number" &&
        Date.now() - action.startedAt < ACTION_LIFETIME_MS)) &&
    (action.reasonBody === undefined || typeof action.reasonBody === "string") &&
    (action.kind !== "merge" ||
      (typeof action.source === "string" && typeof action.target === "string"))
  )
    return action;
  return undefined;
}

export function writeTableAction(project: string, action: TableAction | undefined): void {
  writeStore(project, { action });
}
