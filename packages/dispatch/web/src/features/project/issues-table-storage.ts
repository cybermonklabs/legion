import type { ColumnOrderState, ColumnSizingState, VisibilityState } from "@tanstack/react-table";

import { readStore, writeStore } from "./issues-table-action-storage";
import { COLUMN_SIZES, PINNED } from "./issues-table-model";

/**
 * The one owner of what the table keeps in the browser for a project: the layout the reader
 * dragged and the record of an action a reload interrupted, under one key, read and written
 * one typed field at a time.
 *
 * The action half, and the box both halves read through, is `issues-table-action-storage.ts`,
 * re-exported below so every reader of this module is unchanged: the shell registers the
 * table's write kind at boot and that kind reads an interrupted action, which through this
 * module reached the table model and its match ranking - about 16,000 bytes of the entry chunk
 * every page parses before it draws anything, for a page no first paint draws
 * (dispatch://INFRA-1109, R13). The layout half stays here, where the column list it validates
 * against is.
 *
 * One failure rule, for every field: a browser that refuses storage, a full quota or a corrupt
 * value is a table with no saved preferences, never an exception out of a click. The URL still
 * carries the view, and the action record is only a convenience for finishing what a reload
 * cut short.
 */
export {
  ACTION_KINDS,
  ACTION_LIFETIME_MS,
  readTableAction,
  type TableAction,
  tableStorageKey,
  writeTableAction,
} from "./issues-table-action-storage";

export interface TableLayout {
  sizing: ColumnSizingState;
  order: ColumnOrderState;
  visibility: VisibilityState;
}

const EMPTY_LAYOUT: TableLayout = { sizing: {}, order: [], visibility: { updated: false } };

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
