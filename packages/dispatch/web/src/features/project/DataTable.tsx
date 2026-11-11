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
  type OnChangeFn,
  type PaginationState,
  type RowModel,
  type RowSelectionState,
  type SortingState,
  type Table,
  type Row as TanstackRow,
  useReactTable,
} from "@tanstack/react-table";
import { useVirtualizer } from "@tanstack/react-virtual";
import {
  Fragment,
  type ReactNode,
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { useSearchParams } from "react-router-dom";
import { InfoHint } from "../../components/InfoHint";
import { borderDefault, textMutedOnCanvas } from "../../theme/classes";
import { ViewportAnchor } from "../shell/ViewportAnchor";
import { ColumnsMenu, IssueTablePager } from "./issues-table-controls";
import { useRowKeymap } from "./issues-table-keys";
import { useTableLayout } from "./issues-table-layout";
import { TableHeaderCell } from "./issues-table-rows";

const EMPTY_FACETS: ReadonlyMap<string, number> = new Map();
const NO_MEASURE = () => {};
const NO_PRIMARIES: readonly string[] = [];
const defaultFacets = (): ReadonlyMap<string, number> => EMPTY_FACETS;
const defaultGroupOrder = (_column: string, a: string, b: string): number => a.localeCompare(b);
const defaultGroupValue = (_column: string, row: unknown): string => String(row);

/** A column authored from a row: what it sorts and filters by, what it draws. */
export interface GridColumn<Row> {
  readonly id: string;
  readonly header: ReactNode;
  readonly value: (row: Row) => string | number | undefined;
  readonly cell?: (row: Row) => ReactNode;
  readonly filter?: "picks" | "text" | false;
  readonly size?: number;
  readonly info?: string;
  readonly primary?: boolean;
}

/** Turns a row-authored column into the TanStack definition the table renders. */
export function gridColumn<Row>(spec: GridColumn<Row>): ColumnDef<Row> {
  return {
    accessorFn: spec.value,
    cell: ({ getValue, row }) => spec.cell?.(row.original) ?? (getValue() as ReactNode),
    enableSorting: true,
    enableColumnFilter: spec.filter !== false && spec.filter !== undefined,
    filterFn:
      spec.filter === "text"
        ? "includesString"
        : (row, _id, picks: string[]) => {
            const value = spec.value(row.original);
            return value !== undefined && picks.includes(String(value));
          },
    header: () =>
      spec.info === undefined ? (
        spec.header
      ) : (
        <span className="flex items-center gap-1">
          {spec.header}
          <InfoHint label={`About ${spec.id}`}>{spec.info}</InfoHint>
        </span>
      ),
    id: spec.id,
    meta: {
      control: true,
      label: typeof spec.header === "string" ? spec.header : spec.id,
      ...(spec.filter === "text" ? { text: true } : {}),
    },
    minSize: 116,
    size: spec.size ?? 160,
    sortDescFirst: false,
    sortUndefined: "last",
  };
}

/** Counts each row under the value a row-authored column gives it. */
export function gridFacets<Row>(
  rows: readonly Row[],
  column: GridColumn<Row>
): Map<string, number> {
  const counts = new Map<string, number>();
  for (const row of rows) {
    const value = column.value(row);
    if (value === undefined) continue;
    const text = String(value);
    counts.set(text, (counts.get(text) ?? 0) + 1);
  }
  return new Map([...counts].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0])));
}

/** The state a caller that keeps it in the URL controls. */
export interface DataTableControlled {
  readonly sorting: SortingState;
  readonly grouping: GroupingState;
  readonly filters: ColumnFiltersState;
  readonly search: string;
  readonly pagination: PaginationState;
  readonly onSortingChange: OnChangeFn<SortingState>;
  readonly onGroupingChange: OnChangeFn<GroupingState>;
  readonly onFiltersChange: OnChangeFn<ColumnFiltersState>;
  readonly onSearchChange: (search: string) => void;
  readonly onPaginationChange: OnChangeFn<PaginationState>;
}

export interface DataTableOptions<Row> {
  readonly rows: readonly Row[];
  readonly columns: readonly ColumnDef<Row>[];
  readonly getRowId: (row: Row) => string;
  readonly layoutKey: string;
  readonly longestKeyChars?: number;
  readonly hiddenByDefault?: Readonly<Record<string, boolean>>;
  readonly columnsOverride?: readonly string[];
  readonly onVisibilityChanged?: () => void;
  readonly primary?: readonly string[];
  readonly fit?: "window" | "rows";
  readonly controlled?: DataTableControlled;
  readonly initialSort?: SortingState;
  readonly pageSize?: number | "all";
  readonly stateKey?: string;
  readonly groupOrder?: (columnId: string, a: string, b: string) => number;
  readonly groupValue?: (columnId: string, row: Row) => string;
  readonly globalFilterFn?: FilterFn<Row>;
  readonly searchColumn?: string;
  readonly selection?: {
    readonly state: RowSelectionState;
    readonly onChange: OnChangeFn<RowSelectionState>;
  };
  readonly facets?: (columnId: string) => ReadonlyMap<string, number>;
  /** Lets a data source begin a dependent read while this core owns the panel state. */
  readonly onOpenFilterChange?: (id: string | undefined) => void;
}

export interface DisplayRow<Row> {
  readonly row: TanstackRow<Row>;
  readonly continued?: boolean;
  readonly shown?: number;
}

export interface DataTableModel<Row> {
  readonly table: Table<Row>;
  readonly displayRows: readonly DisplayRow<Row>[];
  readonly pageLeaves: readonly Row[];
  readonly listedLeaves: readonly Row[];
  readonly layout: ReturnType<typeof useTableLayout>;
  readonly openFilter: string | undefined;
  readonly setOpenFilter: (id: string | undefined) => void;
  readonly narrow: boolean;
  readonly fit: "window" | "rows";
  readonly facets: (columnId: string) => ReadonlyMap<string, number>;
}

/**
 * The sorted rows, with group headings put back in the order the caller gives their values.
 * TanStack groups before sorting, so a group's heading otherwise follows its first leaf.
 */
function sortedByGroup<Row>(
  groupOrder: (columnId: string, a: string, b: string) => number,
  groupValue: (columnId: string, row: Row) => string
): (instance: Table<Row>) => () => RowModel<Row> {
  return (instance) => {
    const sorted = getSortedRowModel<Row>()(instance);
    let last: { from: RowModel<Row>; grouping: string; model: RowModel<Row> } | undefined;
    return () => {
      const model = sorted();
      const grouped = instance.getState().grouping[0];
      if (grouped === undefined) return model;
      if (last?.from === model && last.grouping === grouped) return last.model;
      const heading = (row: TanstackRow<Row>) =>
        row.getIsGrouped() ? String(row.groupingValue ?? "") : groupValue(grouped, row.original);
      const ordered = {
        ...model,
        rows: [...model.rows].sort((a, b) => groupOrder(grouped, heading(a), heading(b))),
      };
      last = { from: model, grouping: grouped, model: ordered };
      return ordered;
    };
  };
}

/** Builds a generic table model, including the layout, state, grouping and current page. */
export function useDataTable<Row>(options: DataTableOptions<Row>): DataTableModel<Row> {
  const [params, setParams] = useSearchParams();
  const prefix = options.stateKey === undefined ? undefined : `${options.stateKey}.`;
  const urlSorting = useMemo(() => {
    if (prefix === undefined) return undefined;
    const value = params.get(`${prefix}sort`);
    if (value === null) return undefined;
    return value
      .split(",")
      .filter(Boolean)
      .map((entry) => ({ desc: entry.startsWith("-"), id: entry.replace(/^-/, "") }));
  }, [params, prefix]);
  const [memorySorting, setMemorySorting] = useState<SortingState>(options.initialSort ?? []);
  const [memoryGrouping, setMemoryGrouping] = useState<GroupingState>([]);
  const [memoryFilters, setMemoryFilters] = useState<ColumnFiltersState>([]);
  const [memorySearch, setMemorySearch] = useState("");
  const [memoryPagination, setMemoryPagination] = useState<PaginationState>({
    pageIndex: 0,
    pageSize: options.pageSize === "all" ? Number.MAX_SAFE_INTEGER : (options.pageSize ?? 50),
  });
  const [expanded, setExpanded] = useState<ExpandedState>(true);
  const [openFilter, setOpenFilter] = useState<string>();

  const controlled = options.controlled;
  const sorting = controlled?.sorting ?? urlSorting ?? memorySorting;
  const grouping = controlled?.grouping ?? memoryGrouping;
  const filters = controlled?.filters ?? memoryFilters;
  const search = controlled?.search ?? memorySearch;
  const pagination = controlled?.pagination ?? memoryPagination;
  const named = useMemo(
    () => options.columns.map((column) => String(column.id ?? "")),
    [options.columns]
  );
  const primary = options.primary ?? NO_PRIMARIES;
  const layout = useTableLayout(options.layoutKey, options.longestKeyChars, {
    columnsOverride: options.columnsOverride,
    hiddenByDefault: options.hiddenByDefault,
    primaryColumns: primary,
  });
  const narrow =
    layout.boxWidth < 640 && primary.length > 0 && options.columnsOverride === undefined;
  const visibilityChanged = useRef(false);
  const visibility = useMemo(() => {
    if (options.columnsOverride !== undefined && !visibilityChanged.current) {
      const shown = new Set(options.columnsOverride);
      return Object.fromEntries(named.map((id) => [id, shown.has(id)]));
    }
    if (!narrow) return layout.visibility;
    const hidden: Record<string, boolean> = {};
    for (const id of named) if (id !== "key" && !primary.includes(id)) hidden[id] = false;
    return { ...hidden, ...layout.visibility, title: false };
  }, [layout.visibility, named, narrow, options.columnsOverride, primary]);
  const groupOrder = options.groupOrder ?? defaultGroupOrder;
  const groupValue = options.groupValue ?? defaultGroupValue;
  const getSortedRowModel = useMemo(
    () => sortedByGroup(groupOrder, groupValue),
    [groupOrder, groupValue]
  );

  const table = useReactTable({
    autoResetExpanded: false,
    autoResetPageIndex: false,
    columnResizeMode: "onChange",
    columns: options.columns as ColumnDef<Row>[],
    data: options.rows as Row[],
    enableRowSelection: options.selection === undefined ? false : (row) => !row.getIsGrouped(),
    enableSorting: true,
    getColumnCanGlobalFilter: (column) => column.id === options.searchColumn,
    getCoreRowModel: getCoreRowModel(),
    getExpandedRowModel: getExpandedRowModel(),
    getFilteredRowModel: getFilteredRowModel(),
    getGroupedRowModel: getGroupedRowModel(),
    getPaginationRowModel: getPaginationRowModel(),
    getRowId: options.getRowId,
    getSortedRowModel,
    globalFilterFn: options.globalFilterFn,
    groupedColumnMode: false,
    onColumnFiltersChange: (next) => {
      if (controlled) controlled.onFiltersChange(next);
      else setMemoryFilters(functionalUpdate(next, filters));
    },
    onColumnOrderChange: layout.setOrder,
    onColumnSizingChange: layout.setSizing,
    onColumnVisibilityChange: (next) => {
      visibilityChanged.current = true;
      layout.setVisibility(next);
    },
    onExpandedChange: setExpanded,
    onGlobalFilterChange: (next) => {
      if (controlled) controlled.onSearchChange(functionalUpdate(next, search));
      else setMemorySearch(functionalUpdate(next, search));
    },
    onGroupingChange: (next) => {
      if (controlled) controlled.onGroupingChange(next);
      else setMemoryGrouping(functionalUpdate(next, grouping));
    },
    onPaginationChange: (next) => {
      if (controlled) controlled.onPaginationChange(next);
      else setMemoryPagination(functionalUpdate(next, pagination));
    },
    onRowSelectionChange: options.selection?.onChange,
    onSortingChange: (next) => {
      if (controlled) {
        controlled.onSortingChange(next);
        return;
      }
      const value = functionalUpdate(next, sorting);
      setMemorySorting(value);
      if (prefix === undefined) return;
      setParams(
        (current) => {
          const copy = new URLSearchParams(current);
          if (value.length === 0) copy.delete(`${prefix}sort`);
          else
            copy.set(
              `${prefix}sort`,
              value.map(({ desc, id }) => `${desc ? "-" : ""}${id}`).join(",")
            );
          return copy;
        },
        { replace: true }
      );
    },
    paginateExpandedRows: true,
    state: {
      columnFilters: filters,
      columnOrder: layout.order.filter((id) => named.includes(id)),
      columnPinning: { left: layout.responsive.pinned.filter((id) => named.includes(id)) },
      columnSizing: layout.responsive.sizing,
      columnVisibility: visibility,
      expanded,
      globalFilter: search,
      grouping,
      pagination,
      ...(options.selection === undefined ? {} : { rowSelection: options.selection.state }),
      sorting,
    },
  });

  useEffect(
    () => options.onOpenFilterChange?.(openFilter),
    [openFilter, options.onOpenFilterChange]
  );
  // biome-ignore lint/correctness/useExhaustiveDependencies: a visibility update commits storage before this callback runs.
  useEffect(() => {
    if (!visibilityChanged.current) return;
    visibilityChanged.current = false;
    options.onVisibilityChanged?.();
  }, [layout.visibility, options.onVisibilityChanged]);

  const pageRows = table.getRowModel().rows;
  const continued =
    pageRows[0] !== undefined && !pageRows[0].getIsGrouped()
      ? pageRows[0].getParentRow()
      : undefined;
  const onThisPage = new Map<string, number>();
  for (const row of pageRows) {
    if (row.getIsGrouped()) continue;
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
      const shown = onThisPage.get(row.id) ?? 0;
      return shown === 0 ? [] : [{ row, shown }];
    }),
  ];
  const listedLeaves = table.getFilteredRowModel().rows.map((row) => row.original);
  const pageLeaves = pageRows.filter((row) => !row.getIsGrouped()).map((row) => row.original);

  return {
    displayRows,
    facets: options.facets ?? defaultFacets,
    fit: options.fit ?? "window",
    layout,
    listedLeaves,
    narrow,
    openFilter,
    pageLeaves,
    setOpenFilter,
    table,
  };
}

export function DataTable<Row>({
  empty = "No matching issues",
  label,
  model,
  renderGroup,
  renderRow,
  scrollTestId = "data-table-scroll",
  trailingWidth = 0,
}: {
  readonly model: DataTableModel<Row>;
  readonly label: string;
  readonly empty?: ReactNode;
  readonly renderRow: (props: {
    row: TanstackRow<Row>;
    index: number;
    measure: (element: Element | null) => void;
    continues: boolean;
  }) => ReactNode;
  readonly renderGroup?: (props: {
    entry: DisplayRow<Row>;
    index: number;
    measure: (element: Element | null) => void;
  }) => ReactNode;
  readonly scrollTestId?: string;
  readonly trailingWidth?: number;
}): ReactNode {
  const { layout, table } = model;
  const [dragColumn, setDragColumn] = useState<string>();
  const controlSignature = JSON.stringify([
    table.getState().columnFilters,
    table.getState().globalFilter,
    table.getState().grouping,
    table.getState().pagination,
    table.getState().sorting,
  ]);
  const previousControls = useRef(controlSignature);
  useLayoutEffect(() => {
    if (previousControls.current !== controlSignature)
      layout.scrollRef.current?.scrollTo({ top: 0 });
    previousControls.current = controlSignature;
  }, [controlSignature, layout.scrollRef]);
  const getItemKey = useCallback(
    (index: number) => {
      const entry = model.displayRows[index];
      return `${entry?.continued ? "continued-" : ""}${entry?.row.id ?? index}`;
    },
    [model.displayRows]
  );
  const virtualizer = useVirtualizer({
    count: model.fit === "rows" || model.narrow ? 0 : model.displayRows.length,
    getItemKey,
    getScrollElement: () => layout.scrollRef.current,
    estimateSize: (index) => (model.displayRows[index]?.row.getIsGrouped() ? 48 : 64),
    initialRect: { width: 1000, height: 600 },
    overscan: 12,
  });
  useRowKeymap({
    displayRows: model.displayRows,
    rowsById: table.getCoreRowModel().rowsById,
    scrollRef: layout.scrollRef,
    scrollToIndex: (index) => virtualizer.scrollToIndex(index, { align: "auto" }),
  });
  const listed = model.fit === "rows" || model.narrow;
  const drawn = listed
    ? model.displayRows.map((_entry, index) => ({ index, key: index }))
    : virtualizer.getVirtualItems().map((item) => ({ index: item.index, key: item.key }));
  const openFacets = useMemo(
    () => (model.openFilter === undefined ? EMPTY_FACETS : model.facets(model.openFilter)),
    [model.facets, model.openFilter]
  );
  const columnOrder = table.getAllLeafColumns().map((column) => column.id);
  const titleRow = (row: TanstackRow<Row>) => {
    const title = row.getAllCells().find((cell) => cell.column.id === "title");
    if (title === undefined) return null;
    return (
      <tr data-grid-title-row={row.id}>
        <td
          className={`border-b px-2 pb-2 ${borderDefault}`}
          colSpan={row.getVisibleCells().length}
          style={{ width: table.getTotalSize() + trailingWidth }}
        >
          <span
            className={`line-clamp-3 break-words ${textMutedOnCanvas}`}
            title={String(title.getValue())}
          >
            {flexRender(title.column.columnDef.cell, title.getContext())}
          </span>
        </td>
      </tr>
    );
  };

  return (
    <>
      <div
        className={`min-w-0 rounded-xl border ${model.fit === "rows" ? "overflow-x-auto" : "overflow-auto"} ${borderDefault}`}
        data-row-count={model.listedLeaves.length}
        data-testid={scrollTestId}
        ref={layout.attachBox}
        style={{
          height: model.fit === "rows" ? undefined : layout.boxHeight,
          overflowAnchor: "none",
          scrollPaddingTop: 64,
        }}
      >
        <ViewportAnchor
          className="w-max min-w-full"
          enabled={() => previousControls.current === controlSignature}
          group="data-issue-group"
          item="data-issue-row"
          scroller={layout.scrollRef}
        >
          <table
            aria-label={label}
            className="w-max min-w-full table-fixed border-separate border-spacing-0 text-sm"
            style={{ width: table.getTotalSize() + trailingWidth }}
          >
            <thead>
              {table.getHeaderGroups().map((group) => (
                <tr key={group.id}>
                  {group.headers.map((header) => (
                    <TableHeaderCell
                      compact={layout.boxWidth < 640}
                      counts={header.column.id === model.openFilter ? openFacets : EMPTY_FACETS}
                      dragColumn={dragColumn}
                      header={header}
                      key={header.id}
                      openFilter={model.openFilter}
                      order={columnOrder}
                      setDragColumn={setDragColumn}
                      setOpenFilter={model.setOpenFilter}
                      setOrder={layout.setOrder}
                    />
                  ))}
                  {trailingWidth === 0 ? null : (
                    <th
                      className={`sticky top-0 z-20 border-b ${borderDefault}`}
                      style={{ width: trailingWidth }}
                    />
                  )}
                </tr>
              ))}
            </thead>
            <tbody>
              {listed || virtualizer.getVirtualItems()[0] === undefined ? null : (
                <tr aria-hidden style={{ height: virtualizer.getVirtualItems()[0]?.start ?? 0 }} />
              )}
              {drawn.map(({ index, key }) => {
                const entry = model.displayRows[index];
                if (entry === undefined) return null;
                if (entry.row.getIsGrouped())
                  return renderGroup === undefined ? null : (
                    <Fragment key={`${entry.continued ? "continued-" : ""}${entry.row.id}`}>
                      {renderGroup({
                        entry,
                        index,
                        measure: listed ? NO_MEASURE : virtualizer.measureElement,
                      })}
                    </Fragment>
                  );
                return (
                  <Fragment key={String(key)}>
                    {renderRow({
                      continues: model.narrow,
                      index,
                      measure: listed ? NO_MEASURE : virtualizer.measureElement,
                      row: entry.row,
                    })}
                    {model.narrow ? titleRow(entry.row) : null}
                  </Fragment>
                );
              })}
              {listed || drawn.length === 0 ? null : (
                <tr
                  aria-hidden
                  style={{
                    height:
                      virtualizer.getTotalSize() - (virtualizer.getVirtualItems().at(-1)?.end ?? 0),
                  }}
                />
              )}
            </tbody>
          </table>
        </ViewportAnchor>
        {model.listedLeaves.length === 0 ? (
          <p className={`p-6 text-center text-sm ${textMutedOnCanvas}`} data-table-empty>
            {empty}
          </p>
        ) : null}
      </div>
      <div className="flex flex-wrap items-center gap-3">
        <IssueTablePager items={table.options.data} pagerRef={layout.pagerRef} table={table} />
        <ColumnsMenu columns={table.getAllLeafColumns()} />
      </div>
    </>
  );
}
