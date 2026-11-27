import {
  type Column,
  type ColumnDef,
  type ColumnFiltersState,
  type ColumnSizingState,
  defaultColumnSizing,
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
import {
  Fragment,
  type ReactNode,
  useCallback,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { ColumnFilter, IssueTablePager } from "../features/project/issues-table-controls";
import { useRowKeymap } from "../features/project/issues-table-keys";
import type { TableLayoutModel } from "../features/project/issues-table-layout";
import { reorder } from "../features/project/issues-table-model";
import { ViewportAnchor } from "../features/shell/ViewportAnchor";
import {
  borderDefault,
  surfaceBg,
  surfaceMutedBg,
  tableResizeActive,
  tableResizeHover,
  textMutedOnCanvas,
  textMutedOnSurfaceMuted,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../theme/classes";

const EMPTY_COUNTS: ReadonlyMap<string, number> = new Map();
const NO_SELECTION: RowSelectionState = {};
const NONE_UNLOADED: readonly string[] = [];
const NO_WIDTHS: Readonly<Record<string, number>> = {};

/** The width of the slot a sortable column's header draws its sort mark in, ▲ or ▼, beside its
 *  label. Each such column's minimum counts it whether or not the table is sorted by the column
 *  now, so a sort never takes room from the label (`DataTableModel.measureHeader`); a header that
 *  wraps (`wrap`) draws the slot only while its column is sorted. */
export const SORT_MARK_WIDTH = 10;

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

export interface DataTableOptions<Row> {
  readonly rows: Row[];
  readonly columns: ColumnDef<Row>[];
  readonly getRowId: (row: Row) => string;
  /** The caller's `useTableLayout`, made before the columns because a column's width can
   *  follow the box it measures. */
  readonly layout: TableLayoutModel;
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
  /** Whether the issue tables' row keys walk the rows (`useRowKeymap`: `j`, `k`, `o`, Enter and
   *  `x`, over the rows' `data-issue-row`). The default, `true`, is the issue tables'; a table
   *  whose rows are no issues, a chart's, binds none. */
  readonly rowKeys?: boolean;
  /** Whether the box draws only the rows in its view, as tall as the window leaves it (the
   *  default, `true`, the issue tables'), or every row at its own height, the box as tall as
   *  they are: a chart's table, which holds every number its graph draws in the page. */
  readonly virtual?: boolean;
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
  /** Whether `virtualizer` draws the rows (`DataTableOptions.virtual`), else every row is drawn. */
  readonly virtual: boolean;
  /** Whether `ViewportAnchor` may hold the row in hand across this commit. */
  readonly anchorEnabled: () => boolean;
  /** Takes the width column `columnId`'s header draws at with nothing cut, which the column is
   *  never drawn narrower than, whatever width the layout or the reader gives it. `DataTable`'s
   *  header cells measure it once per layout. */
  readonly measureHeader: (columnId: string, width: number) => void;
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
 * the keys that walk them. The caller keeps the view and the layout and draws each row; the
 * core keeps which groups are open.
 */
export function useDataTable<Row>({
  columns,
  getRowId,
  groupOrder,
  layout,
  onColumnFiltersChange,
  onGlobalFilterChange,
  onGroupingChange,
  onPaginationChange,
  onSortingChange,
  rowKeys = true,
  rows,
  search,
  selection,
  state,
  viewKey,
  virtual = true,
}: DataTableOptions<Row>): DataTableModel<Row> {
  const [expanded, setExpanded] = useState<ExpandedState>(true);
  // Each column's header width, by column id, as `measureHeader` last took it.
  const [headerWidths, setHeaderWidths] = useState(NO_WIDTHS);
  const measureHeader = useCallback(
    (columnId: string, width: number) =>
      setHeaderWidths((widths) =>
        widths[columnId] === width ? widths : { ...widths, [columnId]: width }
      ),
    []
  );
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
      // `columnSizing` is handed over below, once the headers' widths are folded into it.
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
  // No column is drawn narrower than its header: each takes the larger of the width the layout
  // gives it, which holds the layout's minimum, and its header's width, and TanStack still keeps
  // it at its definition's `minSize`. Folded in once TanStack has resolved each column's id and
  // own width, then handed to it as the sizing every width, offset and total is read from.
  const leafColumns = table.getAllLeafColumns();
  const laidOut = layout.responsive.sizing;
  const columnSizing = useMemo(() => {
    const sizing: ColumnSizingState = { ...laidOut };
    for (const column of leafColumns) {
      const header = headerWidths[column.id];
      if (header !== undefined)
        sizing[column.id] = Math.max(
          laidOut[column.id] ?? column.columnDef.size ?? defaultColumnSizing.size,
          header
        );
    }
    return sizing;
  }, [headerWidths, laidOut, leafColumns]);
  table.setOptions((options) => ({ ...options, state: { ...options.state, columnSizing } }));

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
  const leafIds: string[] = [];
  for (const row of pageRows)
    if (!row.getIsGrouped()) {
      leafIds.push(row.id);
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
  // neither the rows this page holds nor a notice above the table is measured from here. What it
  // is told is which rows the page holds - its leaves, gathered in the walk above - whose cells
  // count toward the widths of the columns that fit what they hold (`LayoutColumns.fits`)
  // whether the box has drawn them or not. A box that draws every row has no virtual row.
  const pageRowIds = leafIds.join("\n");
  const { setPageRows } = layout;
  useLayoutEffect(() => {
    setPageRows(pageRowIds === "" ? [] : pageRowIds.split("\n"));
  }, [pageRowIds, setPageRows]);
  const virtualizer = useVirtualizer({
    count: virtual ? displayRows.length : 0,
    getScrollElement: () => layout.scrollRef.current,
    estimateSize: (index) => (displayRows[index]?.row.getIsGrouped() ? 48 : 64),
    getItemKey: (index) =>
      `${displayRows[index]?.continued ? "continued-" : ""}${displayRows[index]?.row.id}`,
    initialRect: { width: 1000, height: 600 },
    overscan: 12,
  });

  useRowKeymap({
    displayRows,
    enabled: rowKeys,
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
    measureHeader,
    table,
    virtual,
    virtualizer,
  };
}

/** What a drawn line is handed: where it sits, and the ref that measures its height. */
export interface LineProps {
  readonly index: number;
  readonly measure: (element: Element | null) => void;
}

/** Whether a cell of `column` draws the divider at the pinned columns' right edge: it is the
 *  last pinned column, and something the box scrolls under it follows it - another column, or
 *  the cell each row draws after its columns (`trailing`). Pinned columns that nothing follows
 *  end where the table does, which on a phone's box is the box's own right border (the
 *  priorities table's place and title), and a divider there would draw that line twice. The
 *  header row and each row's cells ask it alike, so the line runs the table's height or not at
 *  all. */
export function drawsPinnedDivider<Row>(column: Column<Row, unknown>, trailing: boolean): boolean {
  return (
    column.getIsPinned() === "left" &&
    column.getIsLastColumn("left") &&
    (trailing || !column.getIsLastColumn())
  );
}

/** What one row of a table is called, singular and plural. Both are spelled out because no
 *  rule turns one into the other: "priority" is "priorities", never "prioritys". */
export interface RowNoun {
  readonly one: string;
  readonly many: string;
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
  /** Draws one row: a `<tr>` carrying `rowAttribute`, the row's id, which the keys, the box's
   *  height and the columns that fit what they hold read (`LayoutColumns.fits`, over
   *  `data-issue-row`), with one cell under each header in the headers' order, `data-index` and
   *  `ref={measure}`. */
  readonly renderRow: (props: LineProps & { readonly row: TableRow<Row> }) => ReactNode;
  /** Draws one group's heading, with `data-index` and `ref={measure}`. */
  readonly renderGroup: (props: LineProps & { readonly entry: DisplayRow<Row> }) => ReactNode;
  /** What one row of this table is, singular and plural, which the pager under it counts in:
   *  `{ one: "issue", many: "issues" }` for the issues table, `{ one: "priority", many:
   *  "priorities" }` for the priorities table. Every caller says its own, because only the
   *  caller knows what its rows are. */
  readonly noun: RowNoun;
  /** The width of a last, unheaded column that each row draws itself. */
  readonly trailingWidth?: number;
  /** The attribute each drawn row carries its id in, which `ViewportAnchor` holds the row in hand
   *  by: `data-issue-row`, the issue tables', by default, which their row keys and their box's
   *  height read too. A table whose rows are no issues names its own, so no reader of the issue
   *  tables' rows reads its rows. */
  readonly rowAttribute?: string;
  /** Whether the pager (`IssueTablePager`: the count, the page size and the pages) stands under
   *  the box: the issue tables' default. A table that draws every row on one page has none. */
  readonly pager?: boolean;
  /** Whether the box ends with its last row rather than standing empty down to the window's
   *  foot: it takes its own rows' height, with the height the window leaves it as its most
   *  (`useTableLayout`'s `boxHeight`), so the rows scroll inside it only once they are more
   *  than the window holds. Unset, the box is always that height. */
  readonly fitRows?: boolean;
  /** The column whose header stands its filter under its words, centred, in a phone's box
   *  (`useTableLayout`'s `compact`): the issues table's title, which a phone draws narrow beside
   *  the key. Every other header stands on one line there. */
  readonly stackedHeader?: string;
  /** The column whose header offers its sort and no filter in a phone's box: the priorities
   *  table's `#`, which a phone draws beside the one cell that holds the rest of the row, too
   *  narrow for a filter's 44 px target beside its sort's without covering the next header's. A
   *  desktop's box draws its filter. */
  readonly sortOnlyOnPhone?: string;
}

/** A line drawn outside the virtualizer, whose height nothing measures. */
const NO_MEASURE = (): void => undefined;

/**
 * The table `useDataTable` describes: its scroll box, with the headers that sort, filter,
 * resize and reorder the columns, the rows the caller draws (the virtual ones, or every one), and
 * the pager under it where the caller keeps one.
 */
export function DataTable<Row>({
  empty,
  noun,
  filterCounts,
  filterUnloaded,
  fitRows = false,
  label,
  model,
  onOpenFilterChange,
  openFilter,
  pager = true,
  renderGroup,
  renderRow,
  rowAttribute = "data-issue-row",
  scrollTestId,
  sortOnlyOnPhone,
  stackedHeader,
  trailingWidth = 0,
}: DataTableProps<Row>): ReactNode {
  const { displayRows, layout, table, virtualizer } = model;
  const [dragColumn, setDragColumn] = useState<string>();
  // The order a drop rewrites, read once rather than per header cell.
  const columnOrder = table.getAllLeafColumns().map((column) => column.id);
  const virtualRows = virtualizer.getVirtualItems();
  // The space the rows above and below the drawn window would take.
  const first = virtualRows[0];
  const last = virtualRows.at(-1);
  // Every line the box draws: the virtual ones, or each of the page's.
  const lines = model.virtual
    ? virtualRows.map((row) => ({
        index: row.index,
        key: row.key,
        measure: virtualizer.measureElement,
      }))
    : displayRows.map((entry, index) => ({
        index,
        key: `${entry.continued ? "continued-" : ""}${entry.row.id}`,
        measure: NO_MEASURE,
      }));

  return (
    <>
      <div
        className={`min-w-0 overflow-auto rounded-xl border ${borderDefault}`}
        ref={layout.attachBox}
        data-testid={scrollTestId}
        style={{
          overflowAnchor: "none",
          // A box that draws every row is as tall as they are; one that draws the rows in its
          // view is the height the window leaves it, or, fitting its rows, that height at most.
          height: model.virtual && !fitRows ? layout.boxHeight : undefined,
          maxHeight: model.virtual && fitRows ? layout.boxHeight : undefined,
          scrollPaddingTop: 64,
        }}
      >
        <ViewportAnchor
          item={rowAttribute}
          className="w-max min-w-full"
          group="data-issue-group"
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
                      boxWidth={layout.boxWidth}
                      compact={layout.responsive.compact}
                      counts={header.column.id === openFilter ? filterCounts : EMPTY_COUNTS}
                      divider={drawsPinnedDivider(header.column, trailingWidth > 0)}
                      dragColumn={dragColumn}
                      header={header}
                      measure={model.measureHeader}
                      noun={noun}
                      key={header.id}
                      openFilter={openFilter}
                      order={columnOrder}
                      setDragColumn={setDragColumn}
                      setOpenFilter={onOpenFilterChange}
                      setOrder={layout.setOrder}
                      sortOnly={layout.responsive.compact && header.column.id === sortOnlyOnPhone}
                      stacked={layout.responsive.compact && header.column.id === stackedHeader}
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
              {lines.map(({ index, key, measure }) => {
                const entry = displayRows[index];
                if (entry === undefined) return null;
                const line = { index, measure };
                return (
                  <Fragment key={key}>
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

      {pager ? <IssueTablePager noun={noun} table={table} pagerRef={layout.pagerRef} /> : null}
    </>
  );
}

/** One header cell: its sort control, which doubles as the handle that drags the column
 *  elsewhere, its filter, and the edge that resizes it. It measures the width it draws its
 *  header at with nothing cut once per layout, which its column is never drawn narrower than. */
function TableHeaderCell<TData>({
  boxWidth,
  compact,
  counts,
  dragColumn,
  divider,
  header,
  measure,
  noun,
  openFilter,
  order,
  setDragColumn,
  setOpenFilter,
  setOrder,
  sortOnly,
  stacked,
  unloaded,
}: {
  /** The box's width as the layout last measured it: a new one is a new layout, in which this
   *  header is measured again. */
  boxWidth: number;
  /** A phone's box (`useTableLayout`'s `compact`), where every header stands on one line at its
   *  column's left edge but the stacked one. */
  compact: boolean;
  counts: ReadonlyMap<string, number>;
  dragColumn: string | undefined;
  /** Whether it draws the pinned columns' divider at its right (`drawsPinnedDivider`). */
  divider: boolean;
  header: Header<TData, unknown>;
  /** Takes the width this header draws at with nothing cut (`DataTableModel.measureHeader`). */
  measure: (columnId: string, width: number) => void;
  /** What one row of this table is, which this cell's filter menu counts in. */
  noun: RowNoun;
  openFilter: string | undefined;
  /** The columns in the order they are shown, which a drop rewrites. */
  order: readonly string[];
  setDragColumn: (id: string | undefined) => void;
  setOpenFilter: (id: string | undefined) => void;
  setOrder: (next: string[]) => void;
  /** Whether this header offers its sort and no filter (`DataTableProps.sortOnlyOnPhone`). */
  sortOnly: boolean;
  /** Whether this header stands its filter under its words (`DataTableProps.stackedHeader`). */
  stacked: boolean;
  /** This column's filter values whose rows are not loaded yet. */
  unloaded: readonly string[];
}): ReactNode {
  const column = header.column;
  const pinned = column.getIsPinned() === "left";
  // On a desktop a column of counts is as narrow as its numbers: its words stand at its right
  // edge, over the numbers, after the sort arrow. Its words, and those of a header that wraps
  // (`wrap`), take a second line rather than be cut, and such a header draws no sort arrow's
  // place while it is not sorted: in a column that narrow its gap is width the longest word
  // needs. A phone's box draws each of them on one line at its column's left edge, as it draws
  // every other header: the column scrolls inside the box, as wide as its header needs there.
  const { numeric = false, wrap = numeric } = (compact ? undefined : column.columnDef.meta) ?? {};
  const sorted = column.getIsSorted();
  // Every other header draws its sort mark in a slot `SORT_MARK_WIDTH` wide whether or not the
  // table is sorted by this column now, so a sort neither moves the label nor cuts it.
  const mark =
    wrap && sorted === false ? null : (
      <span className={`shrink-0 ${textMutedOnCanvas}`} style={{ width: SORT_MARK_WIDTH }}>
        {{ asc: "▲", desc: "▼" }[sorted as string] ?? ""}
      </span>
    );
  const sortable = column.getCanSort();
  const filterable = column.getCanFilter() && !sortOnly;
  const label = column.columnDef.header;
  const cellRef = useRef<HTMLTableCellElement>(null);
  const partsRef = useRef<HTMLSpanElement>(null);
  const sortRef = useRef<HTMLButtonElement>(null);
  const labelRef = useRef<HTMLSpanElement>(null);
  const filterRef = useRef<HTMLSpanElement>(null);
  // What the header draws whole changes with the layout (its breakpoint's padding, gaps and
  // filter button), its label, and whether it sorts, filters and wraps; not with its column's
  // width, which the measure does not read, nor with a sort, whose mark's slot it counts sorted
  // or not. A header that wraps takes more lines rather than be cut, so its words on one line are
  // no minimum of its column: the layout reads what it needs (`LayoutColumns.wrappedHeaders`).
  // biome-ignore lint/correctness/useExhaustiveDependencies: `boxWidth`, `filterable`, `label` and `sortable` are the triggers, not reads
  useLayoutEffect(() => {
    const cell = cellRef.current;
    const parts = partsRef.current;
    const text = labelRef.current;
    if (cell === null || parts === null || text === null) return;
    if (wrap) {
      measure(column.id, 0);
      return;
    }
    const filter = filterRef.current;
    const sort = sortRef.current;
    measure(column.id, headerWidth({ cell, filter, label: text, parts, sort, stacked }));
  }, [boxWidth, column.id, filterable, label, measure, sortable, stacked, wrap]);
  return (
    <th
      aria-sort={sorted === "asc" ? "ascending" : sorted === "desc" ? "descending" : undefined}
      className={`sticky top-0 border-b px-1 py-2 pr-2 text-left align-middle text-xs font-semibold md:px-2 ${borderDefault} ${surfaceBg} ${textSecondaryOnCanvas} ${pinned ? "z-30" : "z-20"} ${divider ? "border-r" : ""}`}
      data-column={column.id}
      onDragOver={(event) => {
        if (dragColumn !== undefined && !pinned) event.preventDefault();
      }}
      onDrop={() => {
        if (dragColumn === undefined || pinned || dragColumn === column.id) return;
        setOrder(reorder(order, dragColumn, column.id));
        setDragColumn(undefined);
      }}
      ref={cellRef}
      style={{
        width: header.getSize(),
        left: pinned ? column.getStart("left") : undefined,
      }}
    >
      <span
        className={`flex ${numeric ? "justify-end" : "justify-between"} ${stacked ? "flex-col items-center" : "items-center gap-0 md:gap-1"}`}
        ref={partsRef}
      >
        {sortable ? (
          <button
            className={`flex min-w-0 items-center gap-1 ${numeric ? "text-right" : wrap ? "text-left" : "truncate whitespace-nowrap"}`}
            draggable={!pinned}
            onClick={column.getToggleSortingHandler()}
            onDragEnd={() => setDragColumn(undefined)}
            onDragStart={() => setDragColumn(column.id)}
            ref={sortRef}
            title={
              pinned
                ? "Sort (shift-click to add a second sort)"
                : "Sort (shift-click to add a second sort); drag to move the column"
            }
            type="button"
          >
            {numeric ? mark : null}
            <span ref={labelRef}>{flexRender(label, header.getContext())}</span>
            {numeric ? null : mark}
          </button>
        ) : (
          <span className={wrap ? undefined : "whitespace-nowrap"} ref={labelRef}>
            {flexRender(label, header.getContext())}
          </span>
        )}
        {filterable ? (
          <span className="flex shrink-0" ref={filterRef}>
            <ColumnFilter
              column={column}
              counts={counts}
              noun={noun}
              onOpenChange={(open) => setOpenFilter(open ? column.id : undefined)}
              open={openFilter === column.id}
              unloaded={unloaded}
            />
          </span>
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
 * The width a header cell draws its header at with nothing cut, whatever width its column has
 * now: the cell's padding and border; its label; where the column sorts, the sort button's gap
 * and the sort mark's slot beside the label (`SORT_MARK_WIDTH`), whether or not the table is
 * sorted by it now; and its filter button, beside the label across the row's gap or, in a header
 * that stacks them, under it. The label and the filter button are read by a range over what each
 * holds, since the sort button clips its label once the column is narrower while the label's
 * text keeps its width.
 */
function headerWidth({
  cell,
  filter,
  label,
  parts,
  sort,
  stacked,
}: {
  cell: HTMLElement;
  /** The filter button's box, where the column filters. */
  filter: HTMLElement | null;
  label: HTMLElement;
  /** The row that holds the sort button, or the label, and the filter. */
  parts: HTMLElement;
  /** The sort button, where the column sorts: the label and the mark's slot. */
  sort: HTMLElement | null;
  stacked: boolean;
}): number {
  const px = (value: string) => Number.parseFloat(value) || 0;
  const drawn = (node: Node) => {
    const range = document.createRange();
    range.selectNodeContents(node);
    return range.getBoundingClientRect().width;
  };
  const named =
    drawn(label) + (sort === null ? 0 : px(getComputedStyle(sort).columnGap) + SORT_MARK_WIDTH);
  const content =
    filter === null
      ? named
      : stacked
        ? Math.max(named, drawn(filter))
        : named + px(getComputedStyle(parts).columnGap) + drawn(filter);
  const edges = getComputedStyle(cell);
  return Math.ceil(
    px(edges.paddingLeft) +
      px(edges.paddingRight) +
      px(edges.borderLeftWidth) +
      px(edges.borderRightWidth) +
      content
  );
}

export interface DataTableGroupRowProps {
  /** How many of the table's columns the heading's line spans. */
  readonly columns: number;
  readonly expanded: boolean;
  /** What the toggle shows beside its chevron: a node, not a string, so a heading can carry a
   *  count, a state's own colour or a word the caller draws for its column's values. */
  readonly heading: ReactNode;
  readonly index: number;
  /** The toggle's accessible name, which says what opens and closes and what is under it. */
  readonly label: string;
  readonly measure: (element: Element | null) => void;
  readonly onToggle: () => void;
  /** What is drawn beside the heading: the group's own actions, a strip of its rows by state, a
   *  link to what the group stands for. */
  readonly slot?: ReactNode;
  /** How wide the strip that stays at the box's left edge is drawn, so a heading is readable
   *  wherever the reader has scrolled sideways to. */
  readonly width: number;
}

/**
 * One group's heading: the line that spans the table's columns, with the toggle that opens and
 * closes the group, the caller's heading beside the chevron and the caller's slot beside that.
 * It stays at the box's left edge while the columns scroll under it.
 */
export function DataTableGroupRow({
  columns,
  expanded,
  heading,
  index,
  label,
  measure,
  onToggle,
  slot,
  width,
}: DataTableGroupRowProps): ReactNode {
  return (
    <tr className={surfaceMutedBg} data-index={index} ref={measure}>
      <td className="px-2 py-2" colSpan={columns}>
        <div
          className="sticky left-0 flex flex-wrap items-center gap-x-4 gap-y-1"
          style={{ width }}
        >
          <button
            aria-expanded={expanded}
            aria-label={label}
            className={`flex min-w-0 items-center gap-2 text-left font-semibold ${textPrimaryOnCanvas}`}
            onClick={onToggle}
            type="button"
          >
            <span className={`w-4 ${textMutedOnSurfaceMuted}`}>{expanded ? "▾" : "▸"}</span>
            {heading}
          </button>
          {slot}
        </div>
      </td>
    </tr>
  );
}
