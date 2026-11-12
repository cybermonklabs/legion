import type { ColumnDef, ColumnFiltersState } from "@tanstack/react-table";
import type { LayoutColumns } from "../project/issues-table-layout";

/**
 * One column of a table on the priorities pages, as its page declares it: how wide it is, what
 * a row files under in it, and what it groups by. Everything a column does that is not drawing
 * a cell lives here, as `COLUMNS` in `features/project/issues-table-model.ts` holds it for the
 * issues table, so a column cannot filter by one value and group by another.
 *
 * A column filters either by typed text (`text`) or by picked values (`values`); every column
 * of these tables does one or the other, since the page carries a filter on every column.
 */
export interface TableColumn<Row> {
  readonly id: string;
  /** The column's name: its header, the name of its filter and its entry in the column
   *  control and the group-by, which read it as text. */
  readonly header: string;
  readonly size: number;
  readonly min: number;
  /** Kept at the left edge while the table scrolls sideways. */
  readonly pinned?: boolean;
  readonly sortable?: boolean;
  /** The text a typed filter matches, where this column filters by typing. */
  readonly text?: (row: Row) => string;
  /** The values a picked filter offers, where this column filters by picking. */
  readonly values?: (row: Row) => string[];
  /** The heading this row groups under; a column without it cannot be grouped by. */
  readonly group?: (row: Row) => string;
  /** The order the filter lists its values in; by count when absent. */
  readonly order?: readonly string[];
  /** A value as the reader sees it. */
  readonly show?: (value: string) => string;
  /** The values the filter offers whatever the rows hold. */
  readonly always?: readonly string[];
}

/** The columns by id, which every lookup here goes through. */
export const columnsById = <Row>(
  columns: readonly TableColumn<Row>[]
): ReadonlyMap<string, TableColumn<Row>> => new Map(columns.map((column) => [column.id, column]));

/** Whether `row` passes every filter but `except`, which is the column whose own panel is
 *  counting what it would leave. */
export function matchesFilters<Row>(
  row: Row,
  columns: ReadonlyMap<string, TableColumn<Row>>,
  filters: ColumnFiltersState,
  except?: string
): boolean {
  return filters.every((filter) => {
    if (filter.id === except) return true;
    const column = columns.get(filter.id);
    if (column === undefined) return true;
    if (column.text !== undefined) {
      const typed = String(filter.value ?? "")
        .trim()
        .toLowerCase();
      return typed === "" || column.text(row).toLowerCase().includes(typed);
    }
    const picked = Array.isArray(filter.value) ? filter.value.map(String) : [];
    return picked.length === 0 || (column.values?.(row) ?? []).some((v) => picked.includes(v));
  });
}

/** How many rows each value of column `id` would leave, under every other filter and the
 *  search: what the open filter panel shows beside each value. */
export function facetCounts<Row>(
  rows: readonly Row[],
  columns: ReadonlyMap<string, TableColumn<Row>>,
  id: string,
  filters: ColumnFiltersState,
  search: string,
  matchesSearch: (row: Row, search: string) => boolean
): Map<string, number> {
  const counts = new Map<string, number>();
  const column = columns.get(id);
  if (column === undefined) return counts;
  for (const row of rows)
    if (matchesFilters(row, columns, filters, id) && matchesSearch(row, search))
      for (const value of new Set(column.values?.(row) ?? []))
        counts.set(value, (counts.get(value) ?? 0) + 1);
  return counts;
}

/** The order of two of column `id`'s headings: the column's own order where it declares one,
 *  else the headings read as text, with numbers in them read as numbers. */
export function compareHeadings<Row>(
  columns: ReadonlyMap<string, TableColumn<Row>>,
  id: string,
  a: string,
  b: string
): number {
  const order = columns.get(id)?.order;
  if (order === undefined) return a.localeCompare(b, undefined, { numeric: true });
  const place = (value: string) => {
    const at = order.indexOf(value);
    return at === -1 ? order.length : at;
  };
  return place(a) - place(b);
}

/** The heading `row` files under in column `id`; the empty text where the column does not
 *  group, which TanStack never asks for. */
export function headingOf<Row>(
  columns: ReadonlyMap<string, TableColumn<Row>>,
  id: string,
  row: Row
): string {
  return columns.get(id)?.group?.(row) ?? "";
}

/** The layout rules `useTableLayout` takes for these columns: each at its own width, never
 *  below its minimum, with the pinned ones kept at the left edge and the first of them that
 *  may narrow giving up width so the rest fit. */
export function tableLayout<Row>(
  columns: readonly TableColumn<Row>[],
  flexible: string
): LayoutColumns {
  const pinned = columns.filter((column) => column.pinned).map(({ id }) => id);
  const minimums = Object.fromEntries(columns.map((column) => [column.id, column.min]));
  return {
    ids: columns.map((column) => column.id),
    sizes: Object.fromEntries(columns.map((column) => [column.id, column.size])),
    minimums,
    pins: {
      wide: { columns: pinned, share: 0.6 },
      phone: {
        columns: pinned,
        share: 0.85,
        widths: {},
        flexibleMin: minimums[flexible] ?? 0,
      },
      flexible,
    },
  };
}

/**
 * The TanStack definitions of these columns: what each one is, from its declaration, and how it
 * draws, from `rendering`. Every column sorts and filters, because the page carries a filter on
 * every column; a column that declares `group` can also be grouped by.
 */
export function columnDefs<Row>(
  columns: readonly TableColumn<Row>[],
  rendering: Record<string, Partial<ColumnDef<Row>>>,
  byId: ReadonlyMap<string, TableColumn<Row>>
): ColumnDef<Row>[] {
  return columns.map((column) => ({
    ...rendering[column.id],
    id: column.id,
    header: column.header,
    size: column.size,
    minSize: column.min,
    enableResizing: true,
    enableSorting: column.sortable !== false,
    enableColumnFilter: true,
    enableGrouping: column.group !== undefined,
    ...(column.group === undefined ? {} : { getGroupingValue: column.group }),
    filterFn: (row, id, value: unknown) =>
      matchesFilters(row.original, byId, [{ id, value }] as ColumnFiltersState),
    meta: {
      ...(column.text === undefined ? {} : { text: true }),
      ...(column.order === undefined ? {} : { order: column.order }),
      ...(column.show === undefined ? {} : { show: column.show }),
      ...(column.always === undefined ? {} : { always: column.always }),
    },
  }));
}
