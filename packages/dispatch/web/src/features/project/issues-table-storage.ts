import type { ColumnOrderState, ColumnSizingState, VisibilityState } from "@tanstack/react-table";
import { readStore, writeStore } from "./issues-table-action-storage";

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
 * (dispatch://INFRA-1109, R13). The layout half stays here, and takes the column list it
 * validates against from its caller (`StoredColumns`), so no module the shell loads reaches
 * the table's columns.
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

/** What a table's stored layout is read for: the columns it has, the ones it opens hidden, and
 *  the ones it keeps pinned in a wide box, whose visibility is not the reader's. */
export interface StoredColumns {
  /** Every column the table has: a stored width, place or visibility of any other is dropped. */
  readonly ids: readonly string[];
  /** The columns the table opens with hidden, until the reader shows them. */
  readonly hiddenByDefault?: Readonly<VisibilityState>;
  /** The columns the table keeps pinned in a wide box: a pinned column is not the reader's to
   *  hide, so its stored visibility is dropped. */
  readonly pins?: { readonly wide: { readonly columns: readonly string[] } };
}

export function readLayout(
  project: string,
  { hiddenByDefault, ids, pins }: StoredColumns
): TableLayout {
  const saved = readStore(project);
  const asRecord = (value: unknown): Record<string, unknown> =>
    value !== null && typeof value === "object" && !Array.isArray(value)
      ? (value as Record<string, unknown>)
      : {};
  const sizing: ColumnSizingState = Object.fromEntries(
    Object.entries(asRecord(saved.sizing)).flatMap(([id, size]): [string, number][] =>
      ids.includes(id) &&
      typeof size === "number" &&
      Number.isFinite(size) &&
      size >= 36 &&
      size <= 2000
        ? [[id, size]]
        : []
    )
  );
  const order = Array.isArray(saved.order)
    ? [
        ...new Set(
          saved.order.filter(
            (id: unknown): id is string => typeof id === "string" && ids.includes(id)
          )
        ),
      ]
    : [];
  const visibility: VisibilityState = Object.fromEntries(
    Object.entries(asRecord(saved.visibility)).flatMap(([id, visible]): [string, boolean][] =>
      ids.includes(id) && !pins?.wide.columns.includes(id) && typeof visible === "boolean"
        ? [[id, visible]]
        : []
    )
  );
  return { sizing, order, visibility: { ...hiddenByDefault, ...visibility } };
}

export function writeLayout(project: string, { sizing, order, visibility }: TableLayout): void {
  writeStore(project, { sizing, order, visibility });
}
