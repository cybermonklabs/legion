import { useQuery } from "@tanstack/react-query";
import {
  type ColumnDef,
  type ColumnFiltersState,
  getCoreRowModel,
  getFilteredRowModel,
  getPaginationRowModel,
  getSortedRowModel,
  type SortingState,
  useReactTable,
} from "@tanstack/react-table";
import { useVirtualizer } from "@tanstack/react-virtual";
import { type ReactNode, useMemo, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { userStateQuery } from "../../api/queries";
import { InfoHint } from "../../components/InfoHint";
import {
  borderDefault,
  linkHoverText,
  linkText,
  surfaceBg,
  textMutedOnCanvas,
} from "../../theme/classes";
import { useAgentRegistry } from "../refs/agent-registry";
import { useIssueTableColumns } from "./issues-table-columns";
import { IssueTablePager } from "./issues-table-controls";
import { useProjectIssueItems } from "./issues-table-data";
import { useTableLayout } from "./issues-table-layout";
import { ALL_ROWS, type ColumnId, type Item, PAGE_SIZES } from "./issues-table-model";
import { IssueRow, TableHeaderCell } from "./issues-table-rows";

/** The width of the trailing cell every row keeps, where the Issues tab draws its hover action
 *  and a grid draws nothing: one table shape, so a column's width means the same on both. */
const ACTION_WIDTH = 104;
const EMPTY_FACETS: ReadonlyMap<string, number> = new Map();

/**
 * A column of a grid that the issues registry does not have: the Legion tab's own numbers, a
 * bar drawn per row. `value` is what it sorts and filters by, `cell` what it draws (the value
 * when it gives none), and `info` the hint its header offers.
 */
export interface GridColumn<Row> {
  readonly id: string;
  readonly header: ReactNode;
  readonly value: (row: Row, item: Item | undefined) => string | number | undefined;
  readonly cell?: (row: Row, item: Item | undefined) => ReactNode;
  readonly filter?: "picks" | "text" | false;
  readonly size?: number;
  readonly info?: string;
}

/** What one row of a grid is made of: the caller's row, and the issue it names as the issues
 *  table knows it. A key the project listing does not hold has no item; its own columns still
 *  draw, and every registry column is blank for it. */
interface GridRow<Row> {
  readonly key: string;
  readonly row: Row;
  readonly item: Item | undefined;
}

/** The row an unknown key stands on: an issue of that key and nothing else, so the registry's
 *  cells draw blanks rather than throwing and the row keeps its own columns. */
function stubItem(key: string): Item {
  return {
    issue: {
      assignee: null,
      blocked_by: [],
      claim: null,
      components: { ids: [], inherited_from: null, mode: "inherit", reason: null, unknown: [] },
      created_at: "",
      created_by: null,
      key,
      labels: [],
      last_seq: 0,
      open_asks: 0,
      parent: null,
      primary_artifact_id: "",
      priority: null,
      progress: { children: null, tasks: null },
      rank: key,
      repo: null,
      resolved_repo: null,
      route: null,
      route_holder: null,
      route_status: null,
      route_status_reason: null,
      status: "triage",
      title: key,
      updated_at: "",
    },
    epic: "No epic",
    author: "Unknown",
    claimedBy: "Nobody",
    claimFree: true,
    search: key,
  };
}

/** Every pick a grid's own column offers, with how many of its rows carry each: the facet counts
 *  of a column the issues registry does not know. */
function gridFacets<Row>(
  rows: readonly GridRow<Row>[],
  column: GridColumn<Row>
): Map<string, number> {
  const counts = new Map<string, number>();
  for (const row of rows) {
    const value = column.value(row.row, row.item);
    if (value === undefined) continue;
    const text = String(value);
    counts.set(text, (counts.get(text) ?? 0) + 1);
  }
  return new Map([...counts].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0])));
}

/**
 * One issues table, wherever a page needs one: the Issues tab's columns, its header filters,
 * its sort, its resizing, reordering, column picker, pagination, virtualisation and row look,
 * over rows of the caller's own kind. Each row joins to its issue's `Item` from the project
 * listing, closed issues included, so every registry column works by its id and a column of the
 * caller's own sits beside them.
 *
 * It selects nothing and acts on nothing: the Issues tab's selection bar and its bulk writes
 * stay with the Issues tab.
 */
export function IssueGrid<Row extends { key: string }>({
  project,
  label,
  rows,
  columns,
  layoutKey,
  stateKey,
  initialSort,
  pageSize = 50,
  empty,
  keyHref,
}: {
  project: string;
  /** The table's accessible name. */
  label: string;
  rows: readonly Row[];
  columns: readonly (ColumnId | GridColumn<Row>)[];
  /** Where the widths, the order and the visibility are kept. */
  layoutKey: string;
  /** The URL parameters the sort, the filters and the page travel in; in memory when absent. */
  stateKey?: string;
  initialSort?: SortingState;
  pageSize?: number | "all";
  empty?: ReactNode;
  /** Where a row's Key link goes; the issue's own page when absent. A keyboard open follows it. */
  keyHref?: (row: Row) => string;
}): ReactNode {
  const [params, setParams] = useSearchParams();
  const layout = useTableLayout(layoutKey);
  const { attachBox, boxHeight, boxWidth, pagerRef, responsive, scrollRef } = layout;
  const userState = useQuery(userStateQuery());
  const registry = useAgentRegistry(false);
  const { allItems, hasLegion } = useProjectIssueItems(project, { wantsDone: true, registry });
  const [openFilter, setOpenFilter] = useState<string>();
  const [dragColumn, setDragColumn] = useState<string>();

  const byKey = useMemo(() => new Map(allItems.map((item) => [item.issue.key, item])), [allItems]);
  const data = useMemo(
    () => rows.map((row) => ({ key: row.key, row, item: byKey.get(row.key) })),
    [rows, byKey]
  );
  /** The table runs on `Item`s, as the Issues tab does, so every registry column, cell and row
   *  is the same code; a row's own columns reach its caller's row through this. */
  const items = useMemo(() => data.map((entry) => entry.item ?? stubItem(entry.key)), [data]);
  const rowFor = useMemo(() => new Map(data.map((entry) => [entry.key, entry])), [data]);

  const registryColumns = useIssueTableColumns(userState.data, boxWidth < 640, hasLegion);
  const tableColumns = useMemo<ColumnDef<Item>[]>(() => {
    const byId = new Map(registryColumns.map((column) => [column.id, column]));
    return columns.flatMap((entry) => {
      if (typeof entry === "string") {
        const found = byId.get(entry);
        if (found === undefined) return [];
        if (entry !== "key" || keyHref === undefined) return [found];
        // The Key column goes where the caller sends it: the Legion tab's rows open an issue's
        // timeline rather than its page, and the keyboard's open follows the same link.
        return [
          {
            ...found,
            cell: ({ row }) => {
              const entryRow = rowFor.get(row.original.issue.key);
              const href = entryRow === undefined ? undefined : keyHref(entryRow.row);
              return (
                <span className="flex items-center gap-1 font-semibold whitespace-nowrap">
                  <Link className={`${linkText} ${linkHoverText}`} data-issue-link to={href ?? "#"}>
                    {row.original.issue.key}
                  </Link>
                </span>
              );
            },
          } satisfies ColumnDef<Item>,
        ];
      }
      const custom = entry;
      const readValue = (item: Item) => {
        const found = rowFor.get(item.issue.key);
        return found === undefined ? undefined : custom.value(found.row, found.item);
      };
      return [
        {
          id: custom.id,
          header: () =>
            custom.info === undefined ? (
              custom.header
            ) : (
              <span className="flex items-center gap-1">
                {custom.header}
                <InfoHint label={`About ${custom.id}`}>{custom.info}</InfoHint>
              </span>
            ),
          accessorFn: readValue,
          sortUndefined: "last",
          // The first press reads smallest first, as every registry column does.
          sortDescFirst: false,
          enableColumnFilter: custom.filter !== false && custom.filter !== undefined,
          filterFn:
            custom.filter === "text"
              ? "includesString"
              : (row, _id, picks: string[]) => {
                  const value = readValue(row.original);
                  return value !== undefined && picks.includes(String(value));
                },
          size: custom.size ?? 160,
          minSize: 116,
          meta: { control: true, ...(custom.filter === "text" ? { text: true } : {}) },
          cell: ({ row }) => {
            const found = rowFor.get(row.original.issue.key);
            if (found === undefined) return null;
            return custom.cell === undefined
              ? (custom.value(found.row, found.item) ?? null)
              : custom.cell(found.row, found.item);
          },
        } satisfies ColumnDef<Item>,
      ];
    });
  }, [columns, registryColumns, rowFor, keyHref]);

  const shownIds = useMemo(() => tableColumns.map((column) => String(column.id)), [tableColumns]);
  const prefix = stateKey === undefined ? undefined : `${stateKey}.`;
  const urlSorting = useMemo(() => {
    if (prefix === undefined) return undefined;
    const value = params.get(`${prefix}sort`);
    if (value === null) return undefined;
    return value
      .split(",")
      .filter(Boolean)
      .map((entry) => ({ desc: entry.startsWith("-"), id: entry.replace(/^-/, "") }));
  }, [params, prefix]);
  const [memorySorting, setMemorySorting] = useState<SortingState>(initialSort ?? []);
  const sorting = urlSorting ?? memorySorting;
  const setSorting = (next: SortingState | ((current: SortingState) => SortingState)) => {
    const value = typeof next === "function" ? next(sorting) : next;
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
  };
  const [filters, setFilters] = useState<ColumnFiltersState>([]);
  const [pagination, setPagination] = useState({
    pageIndex: 0,
    pageSize: pageSize === "all" ? ALL_ROWS : pageSize,
  });

  const table = useReactTable({
    data: items,
    columns: tableColumns,
    state: {
      sorting,
      columnFilters: filters,
      columnVisibility: layout.visibility,
      pagination,
      columnSizing: responsive.sizing,
      columnOrder: layout.order.filter((id) => shownIds.includes(id)),
      columnPinning: { left: responsive.pinned.filter((id) => shownIds.includes(id)) },
    },
    getRowId: (item) => item.issue.key,
    onColumnSizingChange: layout.setSizing,
    onColumnOrderChange: layout.setOrder,
    columnResizeMode: "onChange",
    onSortingChange: setSorting,
    onColumnFiltersChange: setFilters,
    onColumnVisibilityChange: layout.setVisibility,
    onPaginationChange: setPagination,
    enableRowSelection: false,
    autoResetPageIndex: false,
    getCoreRowModel: getCoreRowModel(),
    getFilteredRowModel: getFilteredRowModel(),
    getSortedRowModel: getSortedRowModel(),
    getPaginationRowModel: getPaginationRowModel(),
  });

  const pageRows = table.getRowModel().rows;
  const virtualizer = useVirtualizer({
    count: pageRows.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => 64,
    getItemKey: (index) => pageRows[index]?.id ?? index,
    initialRect: { width: 1000, height: 600 },
    overscan: 12,
  });

  const openColumn = columns.find(
    (entry) => typeof entry !== "string" && entry.id === openFilter
  ) as GridColumn<Row> | undefined;
  const facets = useMemo(
    () => (openColumn === undefined ? EMPTY_FACETS : gridFacets(data, openColumn)),
    [data, openColumn]
  );
  const columnOrder = table.getAllLeafColumns().map((column) => column.id);

  return (
    <div className="space-y-3">
      <div
        className={`min-w-0 overflow-auto rounded-xl border ${borderDefault}`}
        ref={attachBox}
        data-testid="issue-grid-scroll"
        style={{ height: boxHeight, overflowAnchor: "none", scrollPaddingTop: 64 }}
      >
        <table
          aria-label={label}
          className="w-max min-w-full table-fixed border-separate border-spacing-0 text-sm"
          style={{ width: table.getTotalSize() + ACTION_WIDTH }}
        >
          <thead>
            {table.getHeaderGroups().map((group) => (
              <tr key={group.id}>
                {group.headers.map((header) => (
                  <TableHeaderCell
                    compact={boxWidth < 640}
                    counts={header.column.id === openFilter ? facets : EMPTY_FACETS}
                    doneLoaded
                    dragColumn={dragColumn}
                    header={header}
                    key={header.id}
                    openFilter={openFilter}
                    order={columnOrder}
                    setDragColumn={setDragColumn}
                    setOpenFilter={setOpenFilter}
                    setOrder={layout.setOrder}
                  />
                ))}
                <th
                  className={`sticky top-0 z-20 border-b ${borderDefault} ${surfaceBg}`}
                  style={{ width: ACTION_WIDTH }}
                />
              </tr>
            ))}
          </thead>
          <tbody>
            {virtualizer.getVirtualItems()[0] ? (
              <tr aria-hidden style={{ height: virtualizer.getVirtualItems()[0]?.start ?? 0 }} />
            ) : null}
            {virtualizer.getVirtualItems().map((virtual) => {
              const row = pageRows[virtual.index];
              if (row === undefined) return null;
              return (
                <IssueRow
                  index={virtual.index}
                  key={row.id}
                  measure={virtualizer.measureElement}
                  row={row}
                />
              );
            })}
            {virtualizer.getVirtualItems().length > 0 ? (
              <tr
                aria-hidden
                style={{
                  height:
                    virtualizer.getTotalSize() - (virtualizer.getVirtualItems().at(-1)?.end ?? 0),
                }}
              />
            ) : null}
          </tbody>
        </table>
        {pageRows.length === 0 ? (
          <p className={`p-6 text-center text-sm ${textMutedOnCanvas}`}>
            {empty ?? "No issues match these filters."}
          </p>
        ) : null}
      </div>
      {pagination.pageSize >= ALL_ROWS && items.length <= PAGE_SIZES[0] ? null : (
        <IssueTablePager table={table} items={items} pagerRef={pagerRef} />
      )}
    </div>
  );
}
