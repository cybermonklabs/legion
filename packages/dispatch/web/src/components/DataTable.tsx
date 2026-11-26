import {
  type Cell,
  type Column,
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
import { measureElement, useVirtualizer, type Virtualizer } from "@tanstack/react-virtual";
import { Fragment, type ReactNode, useLayoutEffect, useRef, useState } from "react";
import { ViewportAnchor } from "../features/shell/ViewportAnchor";
import {
  borderDefault,
  surfaceBg,
  surfaceMutedBg,
  tableResizeActive,
  tableResizeHover,
  textMutedOnCanvas,
  textMutedOnSurface,
  textMutedOnSurfaceMuted,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../theme/classes";
import { ColumnFilter } from "./ColumnFilter";
import { CONTROL, type ControlSize } from "./control-names";
import { ARM_ON_PRESS, landNearest, useFocusPlace } from "./focus-place";
import { Pager, type PagerOptions } from "./Pager";
import { Tooltip, useTooltip } from "./Tooltip";
import { type FocusRow, type RowKeymap, useRowKeymap } from "./table-keys";
import { reorder, type TableLayoutModel } from "./table-layout";

/** TanStack's own renderer for a header or a cell, and its reading of a state change that is a
 *  value or a function of the state, re-exported: the core is the one place the table library is
 *  imported as a value, and a page that draws its own rows needs the renderer to turn a column's
 *  definition into what the row shows. */
export { flexRender, functionalUpdate };

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
 * attributes its rows, groups and links carry, and how its keys are named in the palette. One
 * object, and it is given to `useDataTable` rather than to `DataTable`, because the keys are
 * registered where the rows are modelled while the attributes are written where they are drawn,
 * and the two must say the same words. What a row is called is `DataTable`'s own `noun`.
 */
export interface DataTableNames {
  /** The attribute each drawn row carries, holding the row's id: `data-issue-row`. */
  readonly rowAttribute: string;
  /** The attribute a drawn row carries naming the group it sits in. */
  readonly groupAttribute: string;
  /** The attribute on the link inside a row that opening the row follows. */
  readonly linkAttribute: string;
  readonly keymap: RowKeymap;
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
  /** Names the view the reader chose, and whether its first draw is whole (the Issues tab's
   *  key changes once more when the Legion board first answers). Where it changes, the box goes
   *  back to its top and `ViewportAnchor` holds nothing for the commit: the reader moved, or the
   *  view drew for the first time, not the rows. */
  readonly viewKey: string;
  /** How tall the caller draws each of its rows, where it draws every row at one height: what
   *  the virtualizer counts a row it has not measured yet at, so the box scrolls the page's whole
   *  height from the first draw and no row moves once it is measured. A table given none counts
   *  an unmeasured row at 64 px until it is drawn. A group's heading is counted at 48 px either
   *  way. */
  readonly rowHeight?: number;
  /** The selected rows, by id. A table without it selects nothing; one with it selects leaf
   *  rows, never a group's heading. */
  readonly selection?: {
    readonly state: RowSelectionState;
    readonly onChange: OnChangeFn<RowSelectionState>;
    /** Told the one row the reader marked by itself, as they mark it: `x` on the row, or the
     *  row's own checkbox, which the caller's column calls it from. A select-all names none. */
    readonly onMark?: (key: string) => void;
  };
  /** What the search box matches: `filterFn` run on `column` alone. Without it the table has
   *  no search. */
  readonly search?: { readonly column: string; readonly filterFn: FilterFn<Row> };
  /** Where the group headings go. Read once, when the table is made, as TanStack reads its row
   *  models once; without it the headings keep TanStack's own order. */
  readonly groupOrder?: GroupOrder<Row>;
  /** The rows are one page a server answered, of `rowCount` rows in all: the table counts its
   *  pages by that count and slices nothing, and a step of the pager asks the caller for the
   *  next page (`onPaginationChange`). Without it the table pages the rows it holds. */
  readonly serverPage?: { readonly rowCount: number };
  /** Whether the table's row keys (`names.keymap`) are registered: false while the page draws
   *  another layout of the same rows in the table's place, such as the Issues tab's Board,
   *  whose own keys answer then and whose shortcut help lists no key of a table it hides. True
   *  where unset. */
  readonly keys?: boolean;
}

/** One line the table draws: a row, or a group's heading. */
export interface DisplayRow<Row> {
  readonly row: TableRow<Row>;
  /** The heading of a group whose rows began on an earlier page, repeated at this page's top. A
   *  heading drawn there for a group whose first row opens this page is not continued. */
  readonly continued?: boolean;
  /** How many of the group's rows this page shows. */
  readonly shown?: number;
}

export interface DataTableModel<Row> {
  readonly table: Table<Row>;
  readonly layout: TableLayoutModel;
  readonly displayRows: readonly DisplayRow<Row>[];
  readonly virtualizer: Virtualizer<HTMLDivElement, Element>;
  /** Whether `ViewportAnchor` may hold the reader's place across this commit: every commit of
   *  the view the reader is in but one that only scrolls. */
  readonly anchorEnabled: () => boolean;
  /** Focuses a row or a control inside it, scrolling an undrawn row in first and focusing it once
   *  it lands. A `"nearest"` hand-off places the control whole, measured once drawn, rather than
   *  letting the row's approximate virtual offset decide where it ends up. */
  readonly focusRow: FocusRow;
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
  keys = true,
  layout,
  names,
  onColumnFiltersChange,
  onGlobalFilterChange,
  onGroupingChange,
  onPaginationChange,
  onSortingChange,
  rowHeight = 64,
  rows,
  search,
  selection,
  serverPage,
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
    manualPagination: serverPage !== undefined,
    rowCount: serverPage?.rowCount,
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
  // A page that starts with a leaf draws its group's heading first. The heading reads continued
  // only when the page before held a leaf of that group: one that was the page before's last
  // row, with none of its rows there, is dropped there (below) and opens its group here.
  const first = pageRows[0];
  const lead = first !== undefined && !first.getIsGrouped() ? first.getParentRow() : undefined;
  const { pageIndex, pageSize } = table.getState().pagination;
  // A server's page holds every row it is given, so no page before it is held here.
  const previous =
    lead === undefined || serverPage !== undefined
      ? undefined
      : table.getPrePaginationRowModel().rows[pageIndex * pageSize - 1];
  const continued =
    previous !== undefined && !previous.getIsGrouped() && previous.getParentRow()?.id === lead?.id;
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
    ...(lead === undefined ? [] : [{ row: lead, continued, shown: onThisPage.get(lead.id) }]),
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
  // whether the box has drawn them or not.
  const pageRowIds = leafIds.join("\n");
  const { setPageRows } = layout;
  useLayoutEffect(() => {
    setPageRows(pageRowIds === "" ? [] : pageRowIds.split("\n"));
  }, [pageRowIds, setPageRows]);
  //
  // A drawn line that changes height is the anchor's (`ViewportAnchor` below): a row another
  // reader changed is redrawn in a commit, which the anchor answers by moving the box. So the
  // virtualizer moves it only for a line it measures for the first time - one drawn as the
  // reader scrolls, whose height it had guessed - and never for a line it had measured; the two
  // together moved the box twice for one change. The virtualizer measures a line from its node
  // just before it asks, so the node it has met before is the line it measured before; a line
  // drawn again after it left the box is a new node, measured from its first draw again. Its own
  // record of sizes cannot say this: it records a size only where it differs from the guess.
  const measured = useRef(new WeakSet<Element>());
  const measuredBefore = useRef(false);
  const virtualizer = useVirtualizer({
    count: displayRows.length,
    getScrollElement: () => layout.scrollRef.current,
    estimateSize: (index) => (displayRows[index]?.row.getIsGrouped() ? 48 : rowHeight),
    getItemKey: (index) =>
      `${displayRows[index]?.continued ? "continued-" : ""}${displayRows[index]?.row.id}`,
    initialRect: { width: 1000, height: 600 },
    overscan: 12,
    measureElement: (element, entry, instance) => {
      measuredBefore.current = measured.current.has(element);
      measured.current.add(element);
      return measureElement(element, entry, instance);
    },
  });
  // A field of the instance, not an option: the virtualizer reads it on each measurement.
  virtualizer.shouldAdjustScrollPositionOnItemSizeChange = (item, _delta, instance) =>
    !measuredBefore.current && item.start < (instance.scrollOffset ?? 0);

  const focusRow = useRowKeymap({
    displayRows,
    keymap: names.keymap,
    keys,
    landNearest: (element, from) => {
      const box = layout.scrollRef.current;
      if (box === null) return;
      // The index scroll only draws the row. Once it is drawn, it must not retarget the box
      // after the checkbox's measured position has taken over.
      if (from !== undefined) virtualizer.cancelScroll();
      landNearest(
        element,
        box.querySelector("thead th"),
        from === undefined ? undefined : { box, top: from }
      );
    },
    linkAttribute: names.linkAttribute,
    onMark: selection?.onMark,
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
  // What the last commit drew: its rows and the virtualizer's scroll as it stood. A commit that
  // only scrolls - the same rows, the virtualizer's offset or its scrolling flag moved - is the
  // reader's own move, which the anchor leaves alone, and so a scroll alone reads no layout.
  // Every other commit is anchored: rows another reader changed, and anything above the rows
  // that changed height while they stayed (a notice, a failed read's alert). The same rows are
  // the same ids holding the same data, in the same order.
  const scroll = { offset: virtualizer.scrollOffset, scrolling: virtualizer.isScrolling };
  const drawn = useRef({ rows: pageRows, scroll });
  useLayoutEffect(() => {
    drawn.current = { rows: pageRows, scroll };
  });
  const onlyScrolled = () => {
    const last = drawn.current;
    const scrolled =
      last.scroll.offset !== scroll.offset || last.scroll.scrolling !== scroll.scrolling;
    return (
      scrolled &&
      last.rows.length === pageRows.length &&
      pageRows.every(
        (row, index) =>
          row.id === last.rows[index]?.id && row.original === last.rows[index]?.original
      )
    );
  };

  return {
    anchorEnabled: () => previousView.current === viewKey && !onlyScrolled(),
    displayRows,
    focusRow,
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

/**
 * What draws the filter in a column's header cell. The caller's, never the cell's own choice:
 * one page's filter reads a registry of facets with a count against each value, another reads
 * the column's own values, and a third narrows by two ends of a range. A caller that gives none
 * gets the shared `ColumnFilter`, counting with `filterCounts` in the table's `noun`, at the
 * table's `size`.
 */
export type ColumnFilterRenderer<Row> = (props: {
  readonly column: Column<Row, unknown>;
  readonly open: boolean;
  readonly onOpenChange: (open: boolean) => void;
}) => ReactNode;

/** One word a table counts in, singular and plural. Both are spelled out because no rule turns
 *  one into the other: "priority" is "priorities", never "prioritys". */
export interface Noun {
  readonly one: string;
  readonly many: string;
}

/** What one row of a table is called, which its header filters and its pager count in, with
 *  the two other words the pager counts in. Every caller says its own, because only the caller
 *  knows what its rows are: the project List counts issues, in groups, over rows, and the
 *  priorities table priorities. */
export interface RowNoun extends Noun {
  /** What a grouping of the rows makes: `in 4 groups`. */
  readonly group: Noun;
  /** What the lines a page holds are called: `Rows 1–25 of 40`, `All 40 rows`, `Rows per
   *  page`. */
  readonly line: Noun;
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
  /** Which values of the open filter's column `filterCounts` has counted for the rows the
   *  reader's narrowing lists: every value (`true`, and a table given none), no value (`false`),
   *  or each value the caller's function answers true for, given the column's id and the value.
   *  A value is not counted while `filterCounts` is the previous answer's, kept on screen while
   *  the read for a new narrowing is out, or while a read that the value's count counts has not
   *  answered. The shared filter draws no count beside a value that is not counted, and its
   *  count beside every other (`ColumnFilter`'s `counted`). */
  readonly filterCounted?: boolean | ((column: string, value: string) => boolean);
  /** By column id, the filter values whose rows are not loaded yet, which say `load` rather
   *  than a count. Kept apart from the column definitions: a new definition is a new component
   *  to React, so a change there would draw every cell anew. */
  readonly filterUnloaded?: ReadonlyMap<string, readonly string[]>;
  /** Draws one row: a `<tr>` carrying the caller's own row attribute, the row's id, which the
   *  keys, the box's height and the columns that fit what they hold read (`LayoutColumns.fits`),
   *  with one cell under each header in the headers' order, `data-index`, which the table writes
   *  the row's `aria-rowindex` from, and `ref={measure}`. A `<tr>` is a row to the page's focus
   *  hand-offs by its element alone (`row-marker.ts`'s `ROW_SELECTOR`). */
  readonly renderRow: (props: LineProps & { readonly row: TableRow<Row> }) => ReactNode;
  /** Draws one group's heading, with `data-index` and `ref={measure}`, as `renderRow` does. */
  readonly renderGroup: (props: LineProps & { readonly entry: DisplayRow<Row> }) => ReactNode;
  /** What one row of this table is, which the shared header filter and the pager count in. */
  readonly noun: RowNoun;
  /** The pager drawn under the box, in the caller's own words. A table given none draws none:
   *  a listing that fits one page, or one paged elsewhere, has no line to show. */
  readonly pager?: PagerOptions;
  /** How much room the controls the table draws take (`ControlSize`): the pager's steps and its
   *  list of page sizes, and each header's shared filter. The page passes the size the rest of
   *  its controls take. The header cells, the rows and their cells are drawn the same at every
   *  size. */
  readonly size?: ControlSize;
  /** Draws the filter in a header cell, for every column that filters. */
  readonly renderFilter?: ColumnFilterRenderer<Row>;
  /** The column that takes what a box wider than the columns has to spare, and is never drawn
   *  narrower than its own width: the rows then reach the box's right edge rather than stop
   *  short of it. The Delivery list names its `title`, which took the rest of its rows before
   *  they were drawn here. A caller that names none, or names a column that is hidden or
   *  pinned, draws every column at its own width. */
  readonly fillColumn?: string;
  /** The columns whose headers stack their sort over their filter, and over a control of their
   *  own (`meta.headerControl`), in a phone's box (`useTableLayout`'s `compact`), where side by
   *  side they leave the name of a narrow column unreadable or push the control out of the
   *  window. The List and the Delivery list each name their `title`, and the Issues tab's List
   *  its Time per phase too; a caller that names none stacks none, and every header keeps them
   *  side by side at every width. */
  readonly stackColumns?: readonly string[];
  /** The column whose header offers its sort and no filter in a phone's box: the priorities
   *  table's `#`, which a phone draws beside the one cell that holds the rest of the row, too
   *  narrow for a filter's 44 px target beside its sort's without covering the next header's. A
   *  desktop's box draws its filter. */
  readonly sortOnlyOnPhone?: string;
  /** Whether the box ends with its last row rather than standing empty down to the window's
   *  foot: it takes its own rows' height, with the height the window leaves it as its most
   *  (`useTableLayout`'s `boxHeight`), so the rows scroll inside it only once they are more
   *  than the window holds. Unset, the box is always that height. */
  readonly fitRows?: boolean;
  /** The width of a last, unheaded column that each row draws itself (`DataTableCells`'
   *  `trailing`). */
  readonly trailingWidth?: number;
  /** Whether a press of the reader's is taking row `id` out of the view (`ViewportAnchor`'s
   *  `leaving`). The rows after such a row move up into its place, and the box does not scroll to
   *  hold the row in hand where it stood. A row no press names, taken away by a live update, is
   *  made up for, so the rows in view stay where they were. A table given none names no row. */
  readonly leaving?: (id: string) => boolean;
  /** Hold a reader at the top too (`ViewportAnchor`'s `holdAtTop`): a row another reader adds
   *  above the first row on screen lands out of view above, under the header. Off unless the
   *  table asks: the Issues table and the priorities table take it, and a list whose reader at
   *  the top waits for arrivals (the Delivery page's list of merged pull requests) leaves it off
   *  and sees them land. */
  readonly holdAtTop?: boolean;
}

/** What every sortable header's tooltip says of its sort button, `main`'s native title of it. */
const SORT_HINT = "Sort (shift-click to add a second sort)";

/** One header cell: its sort control, which doubles as the handle that drags the column
 *  elsewhere, a control of the column's own (`meta.headerControl`), its filter, and the edge
 *  that resizes it. */
function TableHeaderCell<TData>({
  compact,
  divider,
  dragColumn,
  fills,
  fixed,
  header,
  openFilter,
  order,
  renderFilter,
  setDragColumn,
  setOpenFilter,
  setOrder,
  sortOnly,
  stacked,
}: {
  /** A phone's box (`useTableLayout`'s `compact`), where every header stands on one line at its
   *  column's left edge but the stacked one. */
  compact: boolean;
  /** Whether it draws the pinned columns' divider at its right (`drawsPinnedDivider`). */
  divider: boolean;
  dragColumn: string | undefined;
  /** Whether this column takes what the table has over the other columns' widths: the caller's
   *  `fillColumn`. Its header names no width, and a fixed table gives such a column the rest. */
  fills: boolean;
  /** Whether the caller's order keeps this column in its place (`layout.fixedColumns`). */
  fixed: boolean;
  header: Header<TData, unknown>;
  openFilter: string | undefined;
  /** The columns in the order they are shown, which a drop rewrites. */
  order: readonly string[];
  /** Draws this column's filter, which the cell never chooses for itself. */
  renderFilter: ColumnFilterRenderer<TData>;
  setDragColumn: (id: string | undefined) => void;
  setOpenFilter: (id: string | undefined) => void;
  setOrder: (next: string[]) => void;
  /** Whether this header offers its sort and no filter (`DataTableProps.sortOnlyOnPhone`). */
  sortOnly: boolean;
  /** Whether this header stacks its sort over its filter and its own control: one of the
   *  caller's `stackColumns`, in a phone's box. */
  stacked: boolean;
}): ReactNode {
  const column = header.column;
  const pinned = column.getIsPinned() === "left";
  // A pinned column is not dragged and takes no drop. One the caller's order keeps in its place
  // is not dragged either, since no drag would move it, but a column may be dropped onto it.
  const movable = !pinned && !fixed;
  // Every sortable header says how it sorts in the shared tooltip, on hover and on keyboard
  // focus, and a column that says how its value is taken (`meta.describe`, a measure's method)
  // says that first, with the sort hint as a second, muted line. The button carries no native
  // title, so two tooltips never show at once. A drag of any column closes it, and no hover
  // opens it until the drop.
  const describe = column.columnDef.meta?.describe;
  const sortHint = movable ? `${SORT_HINT}; drag to move the column` : SORT_HINT;
  const sortRef = useRef<HTMLButtonElement>(null);
  const tooltip = useTooltip({ paused: dragColumn !== undefined });
  const control = column.columnDef.meta?.headerControl;
  // On a desktop a column of counts is as narrow as its numbers: its words stand at its right
  // edge, over the numbers, after the sort arrow. Its words, and those of a header that wraps
  // (`wrap`), take a second line rather than be cut, and such a header draws no sort arrow's
  // place while it is not sorted: in a column that narrow its gap is width the longest word
  // needs. A phone's box draws each of them on one line at its column's left edge, as it draws
  // every other header: the column scrolls inside the box, as wide as its header needs there.
  const { numeric = false, wrap = numeric } = (compact ? undefined : column.columnDef.meta) ?? {};
  const sorted = column.getIsSorted();
  // The mark of every column the table is sorted by, no wider than `SORT_MARK_WIDTH`, which the
  // layout adds to that column's minimum while the sort holds (`useTableLayout`).
  const mark =
    wrap && sorted === false ? null : (
      <span className={textMutedOnCanvas}>{{ asc: "▲", desc: "▼" }[sorted as string] ?? ""}</span>
    );
  const filter =
    column.getCanFilter() && !sortOnly
      ? renderFilter({
          column,
          onOpenChange: (open) => setOpenFilter(open ? column.id : undefined),
          open: openFilter === column.id,
        })
      : null;
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
      style={{
        width: fills ? undefined : header.getSize(),
        left: pinned ? column.getStart("left") : undefined,
      }}
    >
      <span
        className={`flex ${numeric ? "justify-end" : "justify-between"} ${stacked ? "flex-col items-center" : "items-center gap-0 md:gap-1"}`}
      >
        {column.getCanSort() ? (
          <>
            <button
              {...tooltip.props}
              className={`flex min-w-0 items-center gap-1 ${numeric ? "text-right" : wrap ? "text-left" : "truncate whitespace-nowrap"}`}
              draggable={movable}
              onClick={column.getToggleSortingHandler()}
              onDragEnd={() => setDragColumn(undefined)}
              onDragStart={() => setDragColumn(column.id)}
              ref={sortRef}
              type="button"
            >
              {numeric ? mark : null}
              {flexRender(column.columnDef.header, header.getContext())}
              {numeric ? null : mark}
            </button>
            <Tooltip anchorRef={sortRef} trigger={tooltip}>
              {describe === undefined ? null : <p>{describe}</p>}
              <p className={describe === undefined ? undefined : `mt-1 ${textMutedOnSurface}`}>
                {sortHint}
              </p>
            </Tooltip>
          </>
        ) : (
          flexRender(column.columnDef.header, header.getContext())
        )}
        {control === undefined ? (
          filter
        ) : (
          <span className="flex shrink-0 items-center gap-1">
            {control}
            {filter}
          </span>
        )}
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
 *
 * A focused control a commit takes out of a drawn row that stays - the List's Start next once
 * its issue starts next - hands the focus to the control now at its place in that row, else to
 * the one before it, passing over every control that takes typed text, else to the row itself,
 * which takes the focus as the row keys' `focusRow` gives it, each only while the reader can see
 * it, else to the main content's own stop with no scroll (`useFocusPlace`). A row that leaves
 * altogether under the reader's own press walks the focus back from where the row stood in the
 * main content (`walkBackInRegion`).
 * A focused row the box scrolls out of its drawn window is no case of a hand-off: it goes with
 * the focus, which is left on the page rather than pulling the box back to where the row was.
 *
 * The table says how many rows it has (`aria-rowcount`) and each drawn line where it sits among
 * them (`aria-rowindex`), so a screen reader hears the whole page's rows rather than the drawn
 * ones alone.
 */
export function DataTable<Row>({
  empty,
  fillColumn,
  filterCounted,
  filterCounts,
  filterUnloaded,
  fitRows = false,
  holdAtTop = false,
  label,
  leaving,
  model,
  noun,
  onOpenFilterChange,
  openFilter,
  pager,
  renderFilter,
  renderGroup,
  renderRow,
  scrollTestId,
  size = "comfortable",
  sortOnlyOnPhone,
  stackColumns,
  trailingWidth = 0,
}: DataTableProps<Row>): ReactNode {
  const { displayRows, layout, names, table, virtualizer } = model;
  const noteFocus = useFocusPlace(
    (element) => element.closest(`[${names.rowAttribute}]`),
    (row) => (row.isConnected && row instanceof HTMLElement ? row : null)
  );
  const [dragColumn, setDragColumn] = useState<string>();
  // The order a drop rewrites, read once rather than per header cell.
  const columnOrder = table.getAllLeafColumns().map((column) => column.id);
  // The caller's `fillColumn`, where it is drawn and scrolls with the rest. A pinned column
  // keeps its own width, because the pinned columns after it are held at the sum of theirs.
  const filled = table
    .getVisibleLeafColumns()
    .some((column) => column.id === fillColumn && !column.getIsPinned());
  const virtualRows = virtualizer.getVirtualItems();
  // The space the rows above and below the drawn window would take.
  const first = virtualRows[0];
  const last = virtualRows.at(-1);
  // The rows `aria-rowcount` and `aria-rowindex` count, every line of the page whether drawn
  // or not: the header's, one per TanStack header group (`getHeaderGroups`), then `displayRows`,
  // the page's lines - TanStack's row model for this page (`getRowModel`, after grouping,
  // expanding and paging, an open group's heading among its rows), with the heading of a group
  // the page before began repeated at its top and a heading whose rows all fall on the next page
  // left off.
  const headerGroups = table.getHeaderGroups();
  const body = useRef<HTMLTableSectionElement>(null);
  // Each drawn line's place among those rows, 1-based with the header's first. The caller draws
  // every line's `<tr>`, so the place is written onto it here after each commit, from the
  // `data-index` every line carries: its index in `displayRows`.
  useLayoutEffect(() => {
    for (const line of body.current?.children ?? []) {
      const index = line.getAttribute("data-index");
      if (index === null) continue;
      const place = String(headerGroups.length + Number(index) + 1);
      if (line.getAttribute("aria-rowindex") !== place) line.setAttribute("aria-rowindex", place);
    }
  });
  // The shared filter, for a caller that asked for no other: it counts with what the table was
  // handed, in what the caller calls its rows.
  const drawFilter: ColumnFilterRenderer<Row> =
    renderFilter ??
    (({ column, onOpenChange, open }) => (
      <ColumnFilter
        column={column}
        counted={
          typeof filterCounted === "function"
            ? (value) => filterCounted(column.id, value)
            : filterCounted
        }
        counts={column.id === openFilter ? filterCounts : EMPTY_COUNTS}
        noun={noun}
        onOpenChange={onOpenChange}
        open={open}
        size={size}
        unloaded={filterUnloaded?.get(column.id) ?? NONE_UNLOADED}
      />
    ));

  return (
    <>
      <div
        className={`min-w-0 overflow-auto rounded-xl border ${borderDefault}`}
        ref={layout.attachBox}
        data-control={CONTROL.dataTable}
        data-testid={scrollTestId}
        style={{
          overflowAnchor: "none",
          height: fitRows ? undefined : layout.boxHeight,
          maxHeight: fitRows ? layout.boxHeight : undefined,
          scrollPaddingTop: 64,
        }}
      >
        {/* The reader's place; at the top too where the caller asks (`holdAtTop`). */}
        <ViewportAnchor
          item={names.rowAttribute}
          className="w-max min-w-full"
          group={names.groupAttribute}
          holdAtTop={holdAtTop}
          leaving={leaving}
          scroller={layout.scrollRef}
          enabled={model.anchorEnabled}
        >
          <table
            aria-label={label}
            aria-rowcount={headerGroups.length + displayRows.length}
            className="table-fixed border-separate border-spacing-0 text-sm"
            // A table with a column to fill is at least as wide as the box, and the column takes
            // what that is over the others' widths (`TableHeaderCell`'s `fills`).
            style={{
              minWidth: filled ? "100%" : undefined,
              width: table.getTotalSize() + trailingWidth,
            }}
          >
            <thead>
              {headerGroups.map((group, index) => (
                <tr aria-rowindex={index + 1} key={group.id}>
                  {group.headers.map((header) => (
                    <TableHeaderCell
                      compact={layout.responsive.compact}
                      divider={drawsPinnedDivider(header.column, trailingWidth > 0)}
                      dragColumn={dragColumn}
                      fills={filled && header.column.id === fillColumn}
                      fixed={layout.fixedColumns?.includes(header.column.id) ?? false}
                      header={header}
                      key={header.id}
                      openFilter={openFilter}
                      order={columnOrder}
                      renderFilter={drawFilter}
                      setDragColumn={setDragColumn}
                      setOpenFilter={onOpenFilterChange}
                      setOrder={layout.setOrder}
                      sortOnly={layout.responsive.compact && header.column.id === sortOnlyOnPhone}
                      stacked={
                        layout.responsive.compact &&
                        (stackColumns?.includes(header.column.id) ?? false)
                      }
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
            <tbody {...ARM_ON_PRESS} onFocus={noteFocus} ref={body}>
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

      {pager === undefined ? null : (
        <Pager {...pager} noun={noun} pagerRef={layout.pagerRef} size={size} table={table} />
      )}
    </>
  );
}

export interface DataTableCellsProps<Row> {
  readonly row: TableRow<Row>;
  /** Anything one cell takes beyond the shared ones, by the caller's own rule: the project
   *  List indents the first cell of a row inside a group. */
  readonly cellClass?: (cell: Cell<Row, unknown>) => string;
  /** What a row the caller is pointing at is painted with — an action's answer, a row just
   *  moved. A pinned cell takes it in place of the surface it covers the scrolled cells with,
   *  so a painted row is painted to its left edge rather than from the first scrolling column.
   *  A row given none paints nothing and its pinned cells keep the surface. */
  readonly highlight?: string;
  /** What the trailing action cell holds. Every row of a table with a trailing head cell
   *  (`DataTable`'s `trailingWidth`) gives this — `null` where that row has no action — so the
   *  rows carry as many cells as the head; a row given nothing at all draws no cell. */
  readonly trailing?: ReactNode;
}

/**
 * The cells of one row: each visible column at its own width, the pinned ones held at the box's
 * left edge in the order they are pinned, and the caller's trailing action cell after them.
 * The caller draws the `<tr>` itself — its accessible name, its own data attributes,
 * `data-index` and `ref={measure}` — and gives the cells what only it knows.
 */
export function DataTableCells<Row>({
  cellClass,
  highlight,
  row,
  trailing,
}: DataTableCellsProps<Row>): ReactNode {
  return (
    <>
      {row.getVisibleCells().map((cell) => {
        const pinned = cell.column.getIsPinned() === "left";
        return (
          <td
            className={`overflow-hidden border-b px-2 py-2 ${borderDefault} ${pinned ? `sticky z-10 ${highlight ?? surfaceBg}` : ""} ${drawsPinnedDivider(cell.column, trailing !== undefined) ? "border-r" : ""} ${cellClass?.(cell) ?? ""}`}
            key={cell.id}
            style={{
              width: cell.column.getSize(),
              left: pinned ? cell.column.getStart("left") : undefined,
            }}
          >
            {flexRender(cell.column.columnDef.cell, cell.getContext())}
          </td>
        );
      })}
      {trailing === undefined ? null : (
        <td className={`border-b px-2 py-2 text-right ${borderDefault}`}>{trailing}</td>
      )}
    </>
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
