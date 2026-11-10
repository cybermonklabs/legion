import type { ColumnOrderState, ColumnSizingState, VisibilityState } from "@tanstack/react-table";
import { COLUMN_SIZES, PINNED } from "./issues-table-model";

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
export const tableStorageKey = (project: string) => `dispatch.project-table.${project}`;

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

/** Phase is shown where a Legion daemon serves the project; Slot, Roles, CI and Time per phase
 *  wait in the column picker, as Updated does. */
const EMPTY_LAYOUT: TableLayout = {
  sizing: {},
  order: [],
  visibility: { updated: false, slot: false, roles: false, ci: false, phase_hours: false },
};

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

export function readLayout(project: string): TableLayout {
  const saved = readStore(project);
  const asRecord = (value: unknown): Record<string, unknown> =>
    value !== null && typeof value === "object" && !Array.isArray(value)
      ? (value as Record<string, unknown>)
      : {};
  const sizing = Object.fromEntries(
    Object.entries(asRecord(saved.sizing)).filter(
      ([id, size]) =>
        id in COLUMN_SIZES &&
        typeof size === "number" &&
        Number.isFinite(size) &&
        size >= 36 &&
        size <= 2000
    )
  );
  const order = Array.isArray(saved.order)
    ? [
        ...new Set<string>(
          saved.order.filter((id: unknown) => typeof id === "string" && id in COLUMN_SIZES)
        ),
      ]
    : [];
  const visibility = Object.fromEntries(
    Object.entries(asRecord(saved.visibility)).filter(
      ([id, visible]) => id in COLUMN_SIZES && !PINNED.includes(id) && typeof visible === "boolean"
    )
  );
  return {
    sizing: sizing as ColumnSizingState,
    order,
    visibility: { ...EMPTY_LAYOUT.visibility, ...visibility } as VisibilityState,
  };
}

export function writeLayout(project: string, { sizing, order, visibility }: TableLayout): void {
  writeStore(project, { sizing, order, visibility });
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
