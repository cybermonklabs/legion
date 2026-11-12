import {
  type ColumnDef,
  type ColumnFiltersState,
  type ExpandedState,
  type FilterFn,
  flexRender,
  functionalUpdate,
  type GroupingState,
  getCoreRowModel,
  getExpandedRowModel,
  getFilteredRowModel,
  getGroupedRowModel,
  getPaginationRowModel,
  getSortedRowModel,
  type Header,
  type OnChangeFn,
  type PaginationState,
  type RowModel,
  type RowSelectionState,
  type SortingState,
  type Table,
  type Row as TableRow,
  useReactTable,
} from "@tanstack/react-table";
import { useVirtualizer, type Virtualizer } from "@tanstack/react-virtual";
import { Fragment, type ReactNode, useLayoutEffect, useRef, useState } from "react";
import { ViewportAnchor } from "../features/shell/ViewportAnchor";
import {
  borderDefault,
  surfaceBg,
  tableResizeActive,
  tableResizeHover,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
} from "../theme/classes";
import { ColumnFilter, type FilterCountNoun } from "./ColumnFilter";
import { CONTROL } from "./control-names";
import { Pager, type PagerOptions } from "./Pager";
import { type RowKeymap, useRowKeymap } from "./table-keys";
import { reorder, type TableLayoutModel } from "./table-layout";

/** TanStack's own renderer for a header or a cell, re-exported: the core is the one place the
 *  table library is imported as a value, and a page that draws its own rows needs this to turn
 *  a column's definition into what the row shows. */
export { flexRender };

const EMPTY_COUNTS: ReadonlyMap<string, number> = new Map();
const NO_SELECTION: RowSelectionState = {};
const NONE_UNLOADED: readonly string[] = [];

/** How a grouped table orders its group headings. */
export interface GroupOrder<Row> {
  /** The order of two headings of column `columnId`. */
  readonly compare: (columnId: string, a: string, b: string) => number;
  /** The heading `row` files under in column `columnId`. */
  readonly heading: (columnId: string, row: Row) => string;
}

/** What the reader is viewing: the part of TanStack's state the caller keeps, in the URL or
 *  wherever it likes, and hands back through the matching `on…Change`. */
export interface DataTableState {
  readonly sorting: SortingState;
  readonly grouping: GroupingState;
  readonly columnFilters: ColumnFiltersState;
  readonly globalFilter: string;
  readonly pagination: PaginationState;
}

/**
 * The caller's own words for what its table holds, which the core reads but never invents: the
 * attributes its rows, groups and links carry, how its keys are named in the palette, and what
 * a count of its rows is called. One object, and it is given to `useDataTable` rather than to
 * `DataTable`, because the keys are registered where the rows are modelled while the attributes
 * are written where they are drawn, and the two must say the same words.
 */
export interface DataTableNames {
  /** The attribute each drawn row carries, holding the row's id: `data-issue-row`. */
  readonly rowAttribute: string;
  /** The attribute a drawn row carries naming the group it sits in. */
  readonly groupAttribute: string;
  /** The attribute on the link inside a row that opening the row follows. */
  readonly linkAttribute: string;
  readonly keymap: RowKeymap;
  /** What a header filter calls a count of rows: `3 issues`. */
  readonly filterCountNoun: FilterCountNoun;
}

export interface DataTableOptions<Row> {
  readonly rows: Row[];
  readonly columns: ColumnDef<Row>[];
  readonly getRowId: (row: Row) => string;
  /** The caller's `useTableLayout`, made before the columns because a column's width can
   *  follow the box it measures. */
  readonly layout: TableLayoutModel;
  readonly names: DataTableNames;
  readonly state: DataTableState;
  readonly onSortingChange: OnChangeFn<SortingState>;
  readonly onGroupingChange: OnChangeFn<GroupingState>;
  readonly onColumnFiltersChange: OnChangeFn<ColumnFiltersState>;
  readonly onGlobalFilterChange: (search: string) => void;
  readonly onPaginationChange: OnChangeFn<PaginationState>;
  /** Names the view the reader chose. Where it changes, the box goes back to its top and
   *  `ViewportAnchor` does not hold the row in hand: the reader moved, not the rows. */
  readonly viewKey: string;
  /** The selected rows, by id. A table without it selects nothing; one with it selects leaf
   *  rows, never a group's heading. */
  readonly selection?: {
    readonly state: RowSelectionState;
    readonly onChange: OnChangeFn<RowSelectionState>;
  };
  /** What the search box matches: `filterFn` run on `column` alone. Without it the table has
   *  no search. */
  readonly search?: { readonly column: string; readonly filterFn: FilterFn<Row> };
  /** Where the group headings go. Read once, when the table is made, as TanStack reads its row
   *  models once; without it the headings keep TanStack's own order. */
  readonly groupOrder?: GroupOrder<Row>;
}

/** One line the table draws: a row, or a group's heading. */
export interface DisplayRow<Row> {
  readonly row: TableRow<Row>;
  /** The heading of a group that began on an earlier page, repeated at this page's top. */
  readonly continued?: boolean;
  /** How many of the group's rows this page shows. */
  readonly shown?: number;
}

export interface DataTableModel<Row> {
  readonly table: Table<Row>;
  readonly layout: TableLayoutModel;
  readonly displayRows: readonly DisplayRow<Row>[];
  readonly virtualizer: Virtualizer<HTMLDivElement, Element>;
  /** Whether `ViewportAnchor` may hold the row in hand across this commit. */
  readonly anchorEnabled: () => boolean;
  readonly names: DataTableNames;
}

/**
 * The sorted rows, with the group headings put back in their column's order.
 *
 * TanStack groups first and sorts afterwards, and that sort carries the group rows with it: by
 * the sum of a numeric column, or by the first leaf's value. A group's place is its column's,
 * so the headings are re-ordered once the sort has run, leaving the rows inside each group in
 * the reader's sort. One function serves a table for its whole life — TanStack asks for this
 * model once and keeps it — so the grouping is read from the table's state at every call
 * rather than captured, and the answer is held until the sorted rows or the grouping change,
 * because the expanding and paging steps after it rebuild whenever this returns a new object.
 */
function sortedByGroup<Row>({ compare, heading }: GroupOrder<Row>) {
  return (instance: Table<Row>) => {
    const sorted = getSortedRowModel<Row>()(instance);
    let last: { from: RowModel<Row>; grouping: string; model: RowModel<Row> } | undefined;
    return () => {
      const model = sorted();
      const grouped = instance.getState().grouping[0];
      if (grouped === undefined) return model;
      if (last?.from === model && last.grouping === grouped) return last.model;
      const headingOf = (row: TableRow<Row>) =>
        row.getIsGrouped() ? String(row.groupingValue ?? "") : heading(grouped, row.original);
      const ordered = {
        ...model,
        rows: [...model.rows].sort((a, b) => compare(grouped, headingOf(a), headingOf(b))),
      };
      last = { from: model, grouping: grouped, model: ordered };
      return ordered;
    };
  };
}

/**
 * The one TanStack table of the web app, for rows of any type: its sorting, filtering,
 * grouping, paging and selection, the lines a page draws, the virtual rows that draw them and
 * the keys that walk them. The caller keeps the view and the layout, draws each row and gives
 * the words everything here is named by (`names`); the core keeps which groups are open.
 */
export function useDataTable<Row>({
  columns,
  getRowId,
  groupOrder,
  layout,
  names,
  onColumnFiltersChange,
  onGlobalFilterChange,
  onGroupingChange,
  onPaginationChange,
  onSortingChange,
  rows,
  search,
  selection,
  state,
  viewKey,
}: DataTableOptions<Row>): DataTableModel<Row> {
  const [expanded, setExpanded] = useState<ExpandedState>(true);
  const [sortedRowModel] = useState(() =>
    groupOrder === undefined ? getSortedRowModel<Row>() : sortedByGroup(groupOrder)
  );
  const table = useReactTable({
    data: rows,
    columns,
    state: {
      ...state,
      columnVisibility: layout.visibility,
      expanded,
      rowSelection: selection?.state ?? NO_SELECTION,
      columnSizing: layout.responsive.sizing,
      columnOrder: layout.order,
      columnPinning: { left: layout.responsive.pinned },
    },
    getRowId,
    onColumnSizingChange: layout.setSizing,
    onColumnOrderChange: layout.setOrder,
    columnResizeMode: "onChange",
    onSortingChange,
    onGroupingChange,
    onColumnFiltersChange,
    onColumnVisibilityChange: layout.setVisibility,
    onPaginationChange,
    // TanStack types this handler `OnChangeFn<any>` and may hand it an updater function, so the
    // caller's string setter gets the resolved search, never an updater it cannot read.
    onGlobalFilterChange: (updater) =>
      onGlobalFilterChange(functionalUpdate<string>(updater, state.globalFilter)),
    onExpandedChange: setExpanded,
    onRowSelectionChange: selection?.onChange,
    globalFilterFn: search?.filterFn,
    getColumnCanGlobalFilter: (column) => column.id === search?.column,
    enableRowSelection: selection === undefined ? false : (row) => !row.getIsGrouped(),
    autoResetPageIndex: false,
    // A page holds leaf rows, so a big group spans pages instead of making one page huge.
    paginateExpandedRows: true,
    autoResetExpanded: false,
    groupedColumnMode: false,
    getCoreRowModel: getCoreRowModel(),
    getFilteredRowModel: getFilteredRowModel(),
    getSortedRowModel: sortedRowModel,
    getGroupedRowModel: getGroupedRowModel(),
    getExpandedRowModel: getExpandedRowModel(),
    getPaginationRowModel: getPaginationRowModel(),
  });

  const pageRows = table.getRowModel().rows;
  const continued =
    pageRows[0] !== undefined && !pageRows[0].getIsGrouped()
      ? pageRows[0].getParentRow()
      : undefined;
  // A page is a flat slice of leaves, so its last heading can be one whose rows all start on
  // the next page. A heading with nothing under it here is left for the page that holds its
  // rows; the ones that stay say how many of their rows this page shows. A collapsed group
  // keeps its heading whatever the page holds — it is the only way back to its rows.
  const onThisPage = new Map<string, number>();
  for (const row of pageRows)
    if (!row.getIsGrouped()) {
      const group = row.getParentRow()?.id;
      if (group !== undefined) onThisPage.set(group, (onThisPage.get(group) ?? 0) + 1);
    }
  const displayRows: DisplayRow<Row>[] = [
    ...(continued === undefined
      ? []
      : [{ row: continued, continued: true, shown: onThisPage.get(continued.id) }]),
    ...pageRows.flatMap((row) => {
      if (!row.getIsGrouped()) return [{ row }];
      if (!row.getIsExpanded()) return [{ row }];
      const here = onThisPage.get(row.id) ?? 0;
      return here === 0 ? [] : [{ row, shown: here }];
    }),
  ];
  // The box measures itself: `useTableLayout` observes its size and the section around it, so
  // neither the rows this page holds nor a notice above the table is measured from here.
  const virtualizer = useVirtualizer({
    count: displayRows.length,
    getScrollElement: () => layout.scrollRef.current,
    estimateSize: (index) => (displayRows[index]?.row.getIsGrouped() ? 48 : 64),
    getItemKey: (index) =>
      `${displayRows[index]?.continued ? "continued-" : ""}${displayRows[index]?.row.id}`,
    initialRect: { width: 1000, height: 600 },
    overscan: 12,
  });

  useRowKeymap({
    displayRows,
    keymap: names.keymap,
    linkAttribute: names.linkAttribute,
    rowAttribute: names.rowAttribute,
    rowsById: table.getCoreRowModel().rowsById,
    scrollRef: layout.scrollRef,
    scrollToIndex: (index) => virtualizer.scrollToIndex(index, { align: "auto" }),
  });
  const previousView = useRef(viewKey);
  useLayoutEffect(() => {
    if (previousView.current !== viewKey) layout.scrollRef.current?.scrollTo({ top: 0 });
    previousView.current = viewKey;
  }, [viewKey, layout.scrollRef]);

  return {
    anchorEnabled: () => previousView.current === viewKey,
    displayRows,
    layout,
    names,
    table,
    virtualizer,
  };
}

/** What a drawn line is handed: where it sits, and the ref that measures its height. */
export interface LineProps {
  readonly index: number;
  readonly measure: (element: Element | null) => void;
}

export interface DataTableProps<Row> {
  readonly model: DataTableModel<Row>;
  /** The table's accessible name. */
  readonly label: string;
  /** The `data-testid` of the box the rows scroll in. */
  readonly scrollTestId: string;
  /** What the box says when no row is listed. */
  readonly empty: ReactNode;
  /** The column whose header filter is open, which the caller keeps: what it reads can depend
   *  on it. */
  readonly openFilter: string | undefined;
  readonly onOpenFilterChange: (id: string | undefined) => void;
  /** How many rows each value of the open filter's column would list. */
  readonly filterCounts: ReadonlyMap<string, number>;
  /** By column id, the filter values whose rows are not loaded yet, which say `load` rather
   *  than a count. Kept apart from the column definitions: a new definition is a new component
   *  to React, so a change there would draw every cell anew. */
  readonly filterUnloaded?: ReadonlyMap<string, readonly string[]>;
  /** Draws one row: a `<tr>` carrying the caller's own row attribute, which the keys and the
   *  box's height read, `data-index` and `ref={measure}`. */
  readonly renderRow: (props: LineProps & { readonly row: TableRow<Row> }) => ReactNode;
  /** Draws one group's heading, with `data-index` and `ref={measure}`. */
  readonly renderGroup: (props: LineProps & { readonly entry: DisplayRow<Row> }) => ReactNode;
  /** The pager drawn under the box, in the caller's own words. A table given none draws none:
   *  a listing that fits one page, or one paged elsewhere, has no line to show. */
  readonly pager?: PagerOptions;
  /** The width of a last, unheaded column that each row draws itself. */
  readonly trailingWidth?: number;
}

/** One header cell: its sort control, which doubles as the handle that drags the column
 *  elsewhere, its filter, and the edge that resizes it. */
function TableHeaderCell<TData>({
  compact,
  counts,
  countNoun,
  dragColumn,
  header,
  openFilter,
  order,
  setDragColumn,
  setOpenFilter,
  setOrder,
  unloaded,
}: {
  /** A phone, where the title's header stacks its controls. */
  compact: boolean;
  counts: ReadonlyMap<string, number>;
  /** What the filter calls a count of rows. */
  countNoun: FilterCountNoun;
  dragColumn: string | undefined;
  header: Header<TData, unknown>;
  openFilter: string | undefined;
  /** The columns in the order they are shown, which a drop rewrites. */
  order: readonly string[];
  setDragColumn: (id: string | undefined) => void;
  setOpenFilter: (id: string | undefined) => void;
  setOrder: (next: string[]) => void;
  /** This column's filter values whose rows are not loaded yet. */
  unloaded: readonly string[];
}): ReactNode {
  const column = header.column;
  const pinned = column.getIsPinned() === "left";
  return (
    <th
      aria-sort={
        column.getIsSorted() === "asc"
          ? "ascending"
          : column.getIsSorted() === "desc"
            ? "descending"
            : undefined
      }
      className={`sticky top-0 border-b px-1 py-2 pr-2 text-left align-middle text-xs font-semibold md:px-2 ${borderDefault} ${surfaceBg} ${textSecondaryOnCanvas} ${pinned ? "z-30" : "z-20"} ${pinned && column.getIsLastColumn("left") ? "border-r" : ""}`}
      onDragOver={(event) => {
        if (dragColumn !== undefined && !pinned) event.preventDefault();
      }}
      onDrop={() => {
        if (dragColumn === undefined || pinned || dragColumn === column.id) return;
        setOrder(reorder(order, dragColumn, column.id));
        setDragColumn(undefined);
      }}
      style={{
        width: header.getSize(),
        left: pinned ? column.getStart("left") : undefined,
      }}
    >
      <span
        className={`flex justify-between ${compact && column.id === "title" ? "flex-col items-center" : "items-center gap-0 md:gap-1"}`}
      >
        {column.getCanSort() ? (
          <button
            className="flex min-w-0 items-center gap-1 truncate whitespace-nowrap"
            draggable={!pinned}
            onClick={column.getToggleSortingHandler()}
            onDragEnd={() => setDragColumn(undefined)}
            onDragStart={() => setDragColumn(column.id)}
            title={
              pinned
                ? "Sort (shift-click to add a second sort)"
                : "Sort (shift-click to add a second sort); drag to move the column"
            }
            type="button"
          >
            {flexRender(column.columnDef.header, header.getContext())}
            <span className={textMutedOnCanvas}>
              {{ asc: "▲", desc: "▼" }[column.getIsSorted() as string] ?? ""}
            </span>
          </button>
        ) : (
          flexRender(column.columnDef.header, header.getContext())
        )}
        {column.getCanFilter() ? (
          <ColumnFilter
            column={column}
            countNoun={countNoun}
            counts={counts}
            onOpenChange={(open) => setOpenFilter(open ? column.id : undefined)}
            open={openFilter === column.id}
            unloaded={unloaded}
          />
        ) : null}
      </span>
      {column.getCanResize() ? (
        <span
          aria-hidden
          className={`absolute top-0 right-0 h-full w-1.5 cursor-col-resize touch-none select-none ${column.getIsResizing() ? tableResizeActive : tableResizeHover}`}
          onDoubleClick={() => column.resetSize()}
          onMouseDown={header.getResizeHandler()}
          onTouchStart={header.getResizeHandler()}
        />
      ) : null}
    </th>
  );
}

/**
 * The table `useDataTable` describes: its scroll box, with the headers that sort, filter,
 * resize and reorder the columns, the virtual rows the caller draws, and, where the caller
 * asks for one, the pager under it.
 */
export function DataTable<Row>({
  empty,
  filterCounts,
  filterUnloaded,
  label,
  model,
  onOpenFilterChange,
  openFilter,
  pager,
  renderGroup,
  renderRow,
  scrollTestId,
  trailingWidth = 0,
}: DataTableProps<Row>): ReactNode {
  const { displayRows, layout, names, table, virtualizer } = model;
  const [dragColumn, setDragColumn] = useState<string>();
  // The order a drop rewrites, read once rather than per header cell.
  const columnOrder = table.getAllLeafColumns().map((column) => column.id);
  const virtualRows = virtualizer.getVirtualItems();
  // The space the rows above and below the drawn window would take.
  const first = virtualRows[0];
  const last = virtualRows.at(-1);

  return (
    <>
      <div
        className={`min-w-0 overflow-auto rounded-xl border ${borderDefault}`}
        ref={layout.attachBox}
        data-control={CONTROL.dataTable}
        data-testid={scrollTestId}
        style={{
          overflowAnchor: "none",
          height: layout.boxHeight,
          scrollPaddingTop: 64,
        }}
      >
        <ViewportAnchor
          item={names.rowAttribute}
          className="w-max min-w-full"
          group={names.groupAttribute}
          scroller={layout.scrollRef}
          enabled={model.anchorEnabled}
        >
          <table
            aria-label={label}
            className="table-fixed border-separate border-spacing-0 text-sm"
            style={{ width: table.getTotalSize() + trailingWidth }}
          >
            <thead>
              {table.getHeaderGroups().map((group) => (
                <tr key={group.id}>
                  {group.headers.map((header) => (
                    <TableHeaderCell
                      compact={layout.boxWidth < 640}
                      counts={header.column.id === openFilter ? filterCounts : EMPTY_COUNTS}
                      countNoun={names.filterCountNoun}
                      dragColumn={dragColumn}
                      header={header}
                      key={header.id}
                      openFilter={openFilter}
                      order={columnOrder}
                      setDragColumn={setDragColumn}
                      setOpenFilter={onOpenFilterChange}
                      setOrder={layout.setOrder}
                      unloaded={filterUnloaded?.get(header.column.id) ?? NONE_UNLOADED}
                    />
                  ))}
                  {trailingWidth === 0 ? null : (
                    <th
                      className={`sticky top-0 z-20 border-b ${borderDefault} ${surfaceBg}`}
                      style={{ width: trailingWidth }}
                    />
                  )}
                </tr>
              ))}
            </thead>
            <tbody>
              {first ? <tr aria-hidden style={{ height: first.start }} /> : null}
              {virtualRows.map((virtual) => {
                const entry = displayRows[virtual.index];
                if (entry === undefined) return null;
                const line = { index: virtual.index, measure: virtualizer.measureElement };
                return (
                  <Fragment key={virtual.key}>
                    {entry.row.getIsGrouped()
                      ? renderGroup({ ...line, entry })
                      : renderRow({ ...line, row: entry.row })}
                  </Fragment>
                );
              })}
              {last ? (
                <tr aria-hidden style={{ height: virtualizer.getTotalSize() - last.end }} />
              ) : null}
            </tbody>
          </table>
        </ViewportAnchor>
        {table.getFilteredRowModel().rows.length === 0 ? (
          <p className={`p-6 text-center text-sm ${textMutedOnCanvas}`}>{empty}</p>
        ) : null}
      </div>

      {pager === undefined ? null : <Pager {...pager} pagerRef={layout.pagerRef} table={table} />}
    </>
  );
}
