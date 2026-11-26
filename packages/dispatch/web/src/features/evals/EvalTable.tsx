import {
  type ColumnDef,
  type ColumnFiltersState,
  flexRender,
  type GroupingState,
  type PaginationState,
  type SortingState,
} from "@tanstack/react-table";
import { type KeyboardEvent, type ReactNode, useMemo, useState } from "react";

import { DataTable, type RowNoun, useDataTable } from "../../components/DataTable";
import { focusVisibleRing, surfaceMutedBg, surfaceMutedHoverBg } from "../../theme/classes";
import { type LayoutColumns, useTableLayout } from "../project/issues-table-layout";
import { ALL_ROWS } from "../project/issues-table-model";

const NO_COUNTS: ReadonlyMap<string, number> = new Map();
const NO_SORTING: SortingState = [];

/** The attribute each row of a table of the Evals tab carries its id in: never the issue tables'
 *  `data-issue-row`, since the tab lists no issues (spec R8). */
export const EVAL_ROW = "data-row";

/** How a table's cells are set: a chart's numbers, right-aligned after the row's label, or a
 *  list's words, each left-aligned on one line. */
const CELLS = {
  numbers: "px-2 text-right tabular-nums first:text-left",
  text: "truncate px-2 text-left",
} as const;

/** The keys that move between a table's rows, each to the row it reaches from `index` of
 *  `count`. */
const ROW_MOVES: Readonly<Record<string, (index: number, count: number) => number>> = {
  ArrowDown: (index, count) => Math.min(index + 1, count - 1),
  ArrowUp: (index) => Math.max(index - 1, 0),
  End: (_index, count) => count - 1,
  Home: () => 0,
};

/**
 * A table of the Evals tab, a chart's or a list's: the one table core (`components/DataTable.tsx`,
 * plan D5) for rows that are no issues. It draws every row, in a box as tall as its rows, on one
 * page with no pager, and binds none of the issue tables' row keys; each row carries `data-row`.
 * The reader sorts, filters and lays out its columns as in any table of the core.
 *
 * A table whose rows open something (`select`) is one keyboard stop: arrow up and down move
 * between its rows, Home and End jump to the ends, and a click or Enter on a row opens it.
 */
export function EvalTable<Row>({
  cells = "numbers",
  columns,
  empty,
  getRowId,
  initialSorting = NO_SORTING,
  label,
  layoutKey,
  noun,
  rows,
  scrollTestId,
  select,
}: {
  /** How its cells are set: numbers (the default) or words. */
  cells?: keyof typeof CELLS;
  columns: ColumnDef<Row>[];
  /** What the table says with no row. */
  empty: string;
  getRowId: (row: Row) => string;
  /** The order the rows are drawn in until the reader sorts them; none, the order given. */
  initialSorting?: SortingState;
  /** The table's accessible name. */
  label: string;
  /** Where the reader's widths, order and visibility of its columns are kept. */
  layoutKey: string;
  noun: RowNoun;
  rows: Row[];
  scrollTestId: string;
  /** What a row opens, and the row open now, which it draws as selected. */
  select?: { readonly onSelect: (row: Row) => void; readonly selectedId: string | undefined };
}): ReactNode {
  const layoutColumns = useMemo(
    (): LayoutColumns => ({
      ids: columns.flatMap((column) => (column.id === undefined ? [] : [column.id])),
    }),
    [columns]
  );
  const [sorting, setSorting] = useState<SortingState>(initialSorting);
  const [grouping, setGrouping] = useState<GroupingState>([]);
  const [columnFilters, setColumnFilters] = useState<ColumnFiltersState>([]);
  const [globalFilter, setGlobalFilter] = useState("");
  const [pagination, setPagination] = useState<PaginationState>({
    pageIndex: 0,
    pageSize: ALL_ROWS,
  });
  const [openFilter, setOpenFilter] = useState<string>();
  // The row the keyboard is on: the one row in the tab order of a table whose rows open.
  const [focusId, setFocusId] = useState<string>();
  const layout = useTableLayout(layoutKey, layoutColumns);
  const state = { columnFilters, globalFilter, grouping, pagination, sorting };
  const table = useDataTable<Row>({
    columns,
    getRowId,
    layout,
    onColumnFiltersChange: setColumnFilters,
    onGlobalFilterChange: setGlobalFilter,
    onGroupingChange: setGrouping,
    onPaginationChange: setPagination,
    onSortingChange: setSorting,
    rowKeys: false,
    rows,
    state,
    viewKey: JSON.stringify(sorting),
    virtual: false,
  });
  const drawn = table.displayRows;
  const tabStop = drawn.some((entry) => entry.row.id === focusId) ? focusId : drawn[0]?.row.id;
  // Moves the keyboard from the row `event` is on to the row its key reaches, every row being
  // drawn.
  const moveFocus = (event: KeyboardEvent<HTMLTableRowElement>) => {
    const move = ROW_MOVES[event.key];
    const body = event.currentTarget.parentElement;
    if (move === undefined || body === null) return false;
    const lines = [...body.children].filter(
      (line): line is HTMLElement => line instanceof HTMLElement && line.hasAttribute(EVAL_ROW)
    );
    const next = lines[move(lines.indexOf(event.currentTarget), lines.length)];
    if (next === undefined) return false;
    next.focus();
    setFocusId(next.getAttribute(EVAL_ROW) ?? undefined);
    return true;
  };
  return (
    <DataTable
      empty={empty}
      filterCounts={NO_COUNTS}
      label={label}
      model={table}
      noun={noun}
      onOpenFilterChange={setOpenFilter}
      openFilter={openFilter}
      pager={false}
      renderGroup={() => null}
      renderRow={({ index, measure, row }) => (
        <tr
          aria-selected={select === undefined ? undefined : row.id === select.selectedId}
          className={
            select === undefined
              ? "h-7"
              : `h-7 cursor-pointer outline-none focus-visible:ring-2 focus-visible:ring-inset ${focusVisibleRing} ${row.id === select.selectedId ? surfaceMutedBg : surfaceMutedHoverBg}`
          }
          data-index={index}
          data-row={row.id}
          key={row.id}
          onClick={
            select === undefined
              ? undefined
              : () => {
                  setFocusId(row.id);
                  select.onSelect(row.original);
                }
          }
          onKeyDown={
            select === undefined
              ? undefined
              : (event) => {
                  if (moveFocus(event)) {
                    event.preventDefault();
                    return;
                  }
                  // Only the row itself: Enter on a link or a button in it is that control's.
                  if (event.key === "Enter" && event.target === event.currentTarget) {
                    select.onSelect(row.original);
                  }
                }
          }
          ref={measure}
          tabIndex={select === undefined ? undefined : row.id === tabStop ? 0 : -1}
        >
          {row.getVisibleCells().map((cell) => (
            <td className={CELLS[cells]} key={cell.id}>
              {flexRender(cell.column.columnDef.cell, cell.getContext())}
            </td>
          ))}
        </tr>
      )}
      rowAttribute={EVAL_ROW}
      scrollTestId={scrollTestId}
    />
  );
}
