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
import { useVirtualizer, type Virtualizer } from "@tanstack/react-virtual";
import { Fragment, type ReactNode, useLayoutEffect, useRef, useState } from "react";
import { ViewportAnchor } from "../features/shell/ViewportAnchor";
import {
  borderDefault,
  surfaceBg,
  surfaceMutedBg,
  surfaceMutedHoverBg,
  tableResizeActive,
  tableResizeFocus,
  tableResizeHover,
  textMutedOnCanvas,
  textMutedOnSurfaceMuted,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../theme/classes";
import { ChoiceMenu, type MenuChoice } from "./ChoiceMenu";
import { ColumnFilter } from "./ColumnFilter";
import { CONTROL } from "./control-names";
import { useFocusPlace } from "./focus-place";
import { Pager, type PagerOptions } from "./Pager";
import { type FocusRow, type RowKeymap, useRowKeymap } from "./table-keys";
import { reorder, type TableLayoutModel, widthBounds } from "./table-layout";

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
  /** Names the view the reader chose. Where it changes, the box goes back to its top and
   *  `ViewportAnchor` does not hold the row in hand: the reader moved, not the rows. */
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
  /** Puts the focus on a row of this page, or on what `selector` names inside it, by the path
   *  `j` and `k` take: a row the virtualizer has not drawn is scrolled to and focused once it
   *  lands, and null is handed back meanwhile. */
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
    estimateSize: (index) => (displayRows[index]?.row.getIsGrouped() ? 48 : rowHeight),
    getItemKey: (index) =>
      `${displayRows[index]?.continued ? "continued-" : ""}${displayRows[index]?.row.id}`,
    initialRect: { width: 1000, height: 600 },
    overscan: 12,
  });

  const focusRow = useRowKeymap({
    displayRows,
    keymap: names.keymap,
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

  return {
    anchorEnabled: () => previousView.current === viewKey,
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

/**
 * What draws the filter in a column's header cell. The caller's, never the cell's own choice:
 * one page's filter reads a registry of facets with a count against each value, another reads
 * the column's own values, and a third narrows by two ends of a range. A caller that gives none
 * gets the shared `ColumnFilter`, counting with `filterCounts` in the table's `noun`.
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
  /** By column id, the filter values whose rows are not loaded yet, which say `load` rather
   *  than a count. Kept apart from the column definitions: a new definition is a new component
   *  to React, so a change there would draw every cell anew. */
  readonly filterUnloaded?: ReadonlyMap<string, readonly string[]>;
  /** Draws one row: a `<tr>` carrying the caller's own row attribute, which the keys and the
   *  box's height read, `data-index`, which the table writes the row's `aria-rowindex` from, and
   *  `ref={measure}`. A `<tr>` is a row to the page's focus rescue by its element alone
   *  (`row-marker.ts`'s `ROW_SELECTOR`). */
  readonly renderRow: (props: LineProps & { readonly row: TableRow<Row> }) => ReactNode;
  /** Draws one group's heading, with `data-index` and `ref={measure}`, as `renderRow` does. */
  readonly renderGroup: (props: LineProps & { readonly entry: DisplayRow<Row> }) => ReactNode;
  /** What one row of this table is, which the shared header filter and the pager count in. */
  readonly noun: RowNoun;
  /** The pager drawn under the box, in the caller's own words. A table given none draws none:
   *  a listing that fits one page, or one paged elsewhere, has no line to show. */
  readonly pager?: PagerOptions;
  /** Draws the filter in a header cell, for every column that filters. */
  readonly renderFilter?: ColumnFilterRenderer<Row>;
  /** The column that takes what a box wider than the columns has to spare, and is never drawn
   *  narrower than its own width: the rows then reach the box's right edge rather than stop
   *  short of it. The Delivery list names its `title`, which took the rest of its rows before
   *  they were drawn here. A caller that names none, or names a column that is hidden or
   *  pinned, draws every column at its own width. */
  readonly fillColumn?: string;
  /** The column whose header stacks its sort and its filter one above the other in a box under
   *  640 px, where the two side by side leave the name of a narrow column unreadable. The List,
   *  the priorities table and the Delivery list each name their `title`; a caller that names none
   *  stacks none, and every header keeps the two side by side at every width. */
  readonly stackColumn?: string;
  /** The width of a last, unheaded column that each row draws itself (`DataTableCells`'
   *  `trailing`). */
  readonly trailingWidth?: number;
}

/** How far a key on a column's grip changes the column's width, in px: an arrow alone, and an
 *  arrow with Shift. */
export const GRIP_STEP = { plain: 10, shift: 50 } as const;

/** The mark on a column's move menu, by which its header cell finds it again after a move. */
const MOVE_MENU_ATTRIBUTE = "data-column-move";

/** The two arrows a column's move menu shows: the column goes left or right. */
function MoveIcon(): ReactNode {
  return (
    <svg
      aria-hidden="true"
      className="size-3.5"
      fill="none"
      stroke="currentColor"
      strokeLinecap="round"
      strokeLinejoin="round"
      strokeWidth={1.5}
      viewBox="0 0 16 16"
    >
      <path d="M5 4.5 1.5 8 5 11.5M11 4.5 14.5 8 11 11.5M1.5 8h13" />
    </svg>
  );
}

/**
 * One header cell: its sort control, which doubles as the handle that drags the column
 * elsewhere, its move menu, its filter, and the grip on its edge that resizes it. The grip and
 * the move menu are named for the column by its header's text, or by its id where the header
 * draws no text, as the Delivery list's colour dot draws none: "Resize color", "Move color".
 *
 * The grip is a Tab stop as well as a pointer's handle (`role="separator"`, with the width as its
 * value): ArrowLeft and ArrowRight change the width by `GRIP_STEP.plain`, with Shift by
 * `GRIP_STEP.shift`, within `widthBounds`, and Enter gives the column back its first width, as a
 * double-click does. A key never moves the width against its own direction: a column drawn
 * outside those bounds - the colour dot is drawn at 20 px, under the 36 px the stored layout
 * keeps - stays as it is on a narrowing key, and the range the grip reports takes in the drawn
 * width. Both paths write the same widths (`onColumnSizingChange`, the layout's `setSizing`). The
 * grip takes only those keys, with `preventDefault`, so the page's keymap, which skips a handled
 * key, reads every other key as it reads it on the header's other controls: `j` and `k` still
 * walk the rows.
 *
 * The move menu (`ChoiceMenu`) offers Move left and Move right, each only where a column that
 * is not pinned stands on that side, and writes the order a drop writes (`reorder`): the column
 * dropped on its left neighbour, or its right neighbour dropped on the column. A pinned
 * column has no move menu, as it has no drag. The menu hands the focus back to its trigger as it
 * closes; the move then draws the cell at its new place, which React does by taking the cell out
 * of its row and putting it back, and a cell taken out loses the focus to the page. So the cell
 * gives the trigger the focus again once the move is drawn.
 *
 * The menu's trigger takes no room in the header: it is drawn out of the header's flow, hanging
 * from the cell's bottom edge at its right end, over the cell's bottom padding and the row
 * under it. It shows, and takes a pointer's press, while the pointer is over the header cell,
 * while it holds the focus and while its menu is open; otherwise it is transparent and a press
 * goes through it to the row. It stays in the Tab order between the sort control and the
 * filter. Each table's widths budget the label, the sort mark, the filter and the padding alone
 * (`issues-table-model.ts`'s `ColumnSpec.min`, the Delivery list's widths), so a trigger in the
 * flow would take its room from the sort control or push the filter under the next cell.
 */
function TableHeaderCell<TData>({
  dragColumn,
  fills,
  header,
  left,
  minimum,
  openFilter,
  order,
  renderFilter,
  right,
  setDragColumn,
  setOpenFilter,
  setOrder,
  stacked,
}: {
  dragColumn: string | undefined;
  /** Whether this column takes what the table has over the other columns' widths: the caller's
   *  `fillColumn`. Its header names no width, and a fixed table gives such a column the rest. */
  fills: boolean;
  header: Header<TData, unknown>;
  /** The column drawn on this one's left, where Move left puts this one before it: undefined
   *  for a pinned column, and where no column, or a pinned one, stands there. */
  left: string | undefined;
  /** The narrowest the caller draws this column (`TableLayoutModel.minimums`). */
  minimum: number | undefined;
  openFilter: string | undefined;
  /** The columns in the order they are shown, which a drop or a move rewrites. */
  order: readonly string[];
  /** Draws this column's filter, which the cell never chooses for itself. */
  renderFilter: ColumnFilterRenderer<TData>;
  /** The column drawn on this one's right, which Move right puts before this one: undefined for
   *  a pinned column, and where no column stands there. */
  right: string | undefined;
  setDragColumn: (id: string | undefined) => void;
  setOpenFilter: (id: string | undefined) => void;
  setOrder: (next: string[]) => void;
  /** Whether this header stacks its sort and its filter one above the other: the caller's
   *  `stackColumn`, in a box under 640 px. */
  stacked: boolean;
}): ReactNode {
  const column = header.column;
  const pinned = column.getIsPinned() === "left";
  const heading = column.columnDef.header;
  const name = typeof heading === "string" && heading !== "" ? heading : column.id;
  const cell = useRef<HTMLTableCellElement>(null);
  // Set by a move from this cell's menu, and read once that move is drawn.
  const moved = useRef(false);
  useLayoutEffect(() => {
    if (!moved.current) return;
    moved.current = false;
    const trigger = cell.current?.querySelector<HTMLElement>(`[${MOVE_MENU_ATTRIBUTE}]`);
    if (trigger !== document.activeElement) trigger?.focus();
  });
  const moves: MenuChoice<"left" | "right">[] = [
    ...(left === undefined ? [] : [{ label: "Move left", value: "left" as const }]),
    ...(right === undefined ? [] : [{ label: "Move right", value: "right" as const }]),
  ];
  const width = Math.round(header.getSize());
  const bounds = widthBounds(column.columnDef, minimum);
  const filter = column.getCanFilter()
    ? renderFilter({
        column,
        onOpenChange: (open) => setOpenFilter(open ? column.id : undefined),
        open: openFilter === column.id,
      })
    : null;
  return (
    <th
      aria-sort={
        column.getIsSorted() === "asc"
          ? "ascending"
          : column.getIsSorted() === "desc"
            ? "descending"
            : undefined
      }
      className={`group/header sticky top-0 border-b px-1 py-2 pr-2 text-left align-middle text-xs font-semibold md:px-2 ${borderDefault} ${surfaceBg} ${textSecondaryOnCanvas} ${pinned ? "z-30" : "z-20"} ${pinned && column.getIsLastColumn("left") ? "border-r" : ""}`}
      onDragOver={(event) => {
        if (dragColumn !== undefined && !pinned) event.preventDefault();
      }}
      onDrop={() => {
        if (dragColumn === undefined || pinned || dragColumn === column.id) return;
        setOrder(reorder(order, dragColumn, column.id));
        setDragColumn(undefined);
      }}
      ref={cell}
      style={{
        width: fills ? undefined : header.getSize(),
        left: pinned ? column.getStart("left") : undefined,
      }}
    >
      <span
        className={`flex justify-between ${stacked ? "flex-col items-center" : "items-center gap-0 md:gap-1"}`}
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
        {moves.length === 0 ? null : (
          <ChoiceMenu
            choices={moves}
            face="Move"
            label={`Move ${name}`}
            marks={{ [MOVE_MENU_ATTRIBUTE]: column.id }}
            onChoose={(side) => {
              const neighbour = side === "left" ? left : right;
              if (neighbour === undefined) return;
              moved.current = true;
              setOrder(
                side === "left"
                  ? reorder(order, column.id, neighbour)
                  : reorder(order, neighbour, column.id)
              );
            }}
            // Out of the flow, over the cell's 8 px bottom padding (`py-2`) and the row under it,
            // so it covers neither the sort control nor the filter (see the comment above).
            triggerClassName={`pointer-events-none absolute top-[calc(100%-0.5rem)] right-2 inline-flex min-h-11 min-w-8 items-center justify-center rounded border p-1 opacity-0 shadow-sm group-hover/header:pointer-events-auto group-hover/header:opacity-100 focus:pointer-events-auto focus:opacity-100 aria-expanded:pointer-events-auto aria-expanded:opacity-100 md:min-h-8 md:min-w-6 ${borderDefault} ${surfaceBg} ${textMutedOnCanvas} ${surfaceMutedHoverBg}`}
          >
            <MoveIcon />
          </ChoiceMenu>
        )}
        {filter}
      </span>
      {column.getCanResize() ? (
        // biome-ignore lint/a11y/useSemanticElements: the grip is a focusable separator, the WAI-ARIA window splitter whose value is the width, laid over the header's edge; an `<hr>` is a thematic break between blocks of content.
        <span
          aria-label={`Resize ${name}`}
          aria-orientation="vertical"
          aria-valuemax={Math.max(width, bounds.max)}
          aria-valuemin={Math.min(width, bounds.min)}
          aria-valuenow={width}
          className={`absolute top-0 right-0 h-full w-1.5 cursor-col-resize touch-none select-none ${tableResizeFocus} ${column.getIsResizing() ? tableResizeActive : tableResizeHover}`}
          onDoubleClick={() => column.resetSize()}
          onKeyDown={(event) => {
            if (event.altKey || event.ctrlKey || event.metaKey) return;
            if (event.key === "Enter") {
              event.preventDefault();
              column.resetSize();
              return;
            }
            const direction =
              event.key === "ArrowRight" ? 1 : event.key === "ArrowLeft" ? -1 : undefined;
            if (direction === undefined) return;
            event.preventDefault();
            const step = direction * (event.shiftKey ? GRIP_STEP.shift : GRIP_STEP.plain);
            const next = Math.min(bounds.max, Math.max(bounds.min, width + step));
            // Held within the bounds, a step can turn round: ArrowLeft on the colour dot drawn at
            // 20 px would set the 36 px minimum. Such a key leaves the width as it is.
            if ((next - width) * direction < 0) return;
            header.getContext().table.setColumnSizing((sizing) => ({
              ...sizing,
              [column.id]: next,
            }));
          }}
          onMouseDown={header.getResizeHandler()}
          onTouchStart={header.getResizeHandler()}
          role="separator"
          tabIndex={0}
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
 * the one before it, else to the row itself, which takes the focus as the row keys' `focusRow`
 * gives it. A row that leaves altogether is no case of this: the selection bar and the page's
 * own hand-offs answer for it. Nor is a focused row the box scrolls out of its drawn window: it
 * goes with the focus, which the shell's focus rescue leaves on the page rather than pull the
 * box back to where the row was (`useFocusRescue`).
 *
 * The table says how many rows it has (`aria-rowcount`) and each drawn line where it sits among
 * them (`aria-rowindex`), so a screen reader hears the whole page's rows rather than the drawn
 * ones alone.
 */
export function DataTable<Row>({
  empty,
  fillColumn,
  filterCounts,
  filterUnloaded,
  label,
  model,
  noun,
  onOpenFilterChange,
  openFilter,
  pager,
  renderFilter,
  renderGroup,
  renderRow,
  scrollTestId,
  stackColumn,
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
        counts={column.id === openFilter ? filterCounts : EMPTY_COUNTS}
        noun={noun}
        onOpenChange={onOpenChange}
        open={open}
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
                  {group.headers.map((header, place) => {
                    // The columns drawn either side that a move may pass: none for a pinned
                    // column, and none that is pinned, as a drop takes no pinned column either.
                    const [left, right] = [group.headers[place - 1], group.headers[place + 1]].map(
                      (side) =>
                        header.column.getIsPinned() === false &&
                        side?.column.getIsPinned() === false
                          ? side.column.id
                          : undefined
                    );
                    return (
                      <TableHeaderCell
                        dragColumn={dragColumn}
                        fills={filled && header.column.id === fillColumn}
                        header={header}
                        key={header.id}
                        left={left}
                        minimum={layout.minimums[header.column.id]}
                        openFilter={openFilter}
                        order={columnOrder}
                        renderFilter={drawFilter}
                        right={right}
                        setDragColumn={setDragColumn}
                        setOpenFilter={onOpenFilterChange}
                        setOrder={layout.setOrder}
                        stacked={layout.boxWidth < 640 && header.column.id === stackColumn}
                      />
                    );
                  })}
                  {trailingWidth === 0 ? null : (
                    <th
                      className={`sticky top-0 z-20 border-b ${borderDefault} ${surfaceBg}`}
                      style={{ width: trailingWidth }}
                    />
                  )}
                </tr>
              ))}
            </thead>
            <tbody onFocus={noteFocus} ref={body}>
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
        <Pager {...pager} noun={noun} pagerRef={layout.pagerRef} table={table} />
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
            className={`overflow-hidden border-b px-2 py-2 ${borderDefault} ${pinned ? `sticky z-10 ${highlight ?? surfaceBg}` : ""} ${pinned && cell.column.getIsLastColumn("left") ? "border-r" : ""} ${cellClass?.(cell) ?? ""}`}
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
