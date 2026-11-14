import type { ColumnOrderState, ColumnSizingState, VisibilityState } from "@tanstack/react-table";

/**
 * The one owner of what a table keeps in the browser: the layout the reader dragged, under the
 * whole key its caller names, read and written one typed field at a time. A caller that keeps
 * more than the layout under that key — the project List keeps the record of an action a reload
 * interrupted — writes its own fields through `readStore` and `writeStore`, so one key holds one
 * object however many owners write into it.
 *
 * One failure rule, for every field: a browser that refuses storage, a full quota or a corrupt
 * value is a table with no saved preferences, never an exception out of a click. The URL still
 * carries the view, and anything else stored here is only a convenience.
 */

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

/** Everything stored under `key`, or nothing where the browser refuses or the value is not an
 *  object this can read a field out of. */
export function readStore(key: string): Record<string, unknown> {
  try {
    const stored: unknown = JSON.parse(localStorage.getItem(key) ?? "{}");
    return stored !== null && typeof stored === "object" && !Array.isArray(stored)
      ? (stored as Record<string, unknown>)
      : {};
  } catch {
    return {};
  }
}

/** Writes `fields` into what is stored under `key`, leaving every other field as it was. */
export function writeStore(key: string, fields: Record<string, unknown>): void {
  try {
    localStorage.setItem(key, JSON.stringify({ ...readStore(key), ...fields }));
  } catch {
    /* The URL still works when the browser refuses local preferences. */
  }
}

export function readLayout(
  key: string,
  { hiddenByDefault, ids, pins }: StoredColumns
): TableLayout {
  const saved = readStore(key);
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

export function writeLayout(key: string, { sizing, order, visibility }: TableLayout): void {
  writeStore(key, { sizing, order, visibility });
}
