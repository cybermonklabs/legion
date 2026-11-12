import type { ColumnDef } from "@tanstack/react-table";
import { type ReactNode, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import type { PriorityIssue } from "../../api/types";
import { DataTable, useDataTable } from "../../components/DataTable";
import { StatusPill } from "../../components/Pill";
import {
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { statusText } from "../project/board-model";
import { useTableLayout } from "../project/issues-table-layout";
import { AgentName } from "../refs/AgentName";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import {
  FAULT_SORTING,
  matchesFaultSearch,
  NO_PRIORITY_COLUMNS,
  NO_PRIORITY_COLUMNS_BY_ID,
  NO_PRIORITY_LAYOUT,
  NOT_RECORDED,
  PARKED_COLUMNS_BY_ID,
  PARKED_LAYOUT,
  parkedColumns,
} from "./fault-model";
import { priorityPath } from "./priority-routes";
import { GroupHeadingRow, TableToolbar } from "./priority-table-controls";
import { columnDefs, compareHeadings, facetCounts, headingOf } from "./priority-table-model";
import { TableBodyRow } from "./priority-table-rows";
import { useTableView } from "./priority-table-view";

const EMPTY_FACETS: ReadonlyMap<string, number> = new Map();

/** Which fault a list draws: the two differ by their columns, their layout and the words they
 *  stand under, and by nothing else. */
export type FaultKind = "no-priority" | "parked";

const SHAPE = {
  "no-priority": {
    columns: () => NO_PRIORITY_COLUMNS,
    byId: NO_PRIORITY_COLUMNS_BY_ID,
    layout: NO_PRIORITY_LAYOUT,
    layoutKey: "priorities-no-priority",
  },
  parked: {
    columns: parkedColumns,
    byId: PARKED_COLUMNS_BY_ID,
    layout: PARKED_LAYOUT,
    layoutKey: "priorities-parked",
  },
} as const;

function renderings(
  titleOf: (id: string) => string
): Record<string, Partial<ColumnDef<PriorityIssue>>> {
  return {
    key: {
      accessorFn: (issue) => issue.key,
      cell: ({ row }) => (
        <Link
          className={`font-semibold whitespace-nowrap ${linkText} ${linkHoverText}`}
          data-issue-link
          to={buildIssuePath({ key: row.original.key, kind: "issue" })}
        >
          {row.original.key}
        </Link>
      ),
    },
    title: { accessorFn: (issue) => issue.title },
    project: { accessorFn: (issue) => issue.project },
    status: {
      accessorFn: (issue) => issue.status,
      cell: ({ row }) => <StatusPill>{statusText(row.original.status)}</StatusPill>,
    },
    holder: {
      accessorFn: (issue) => issue.claim?.actor.id ?? issue.assignee ?? "",
      cell: ({ row }) =>
        row.original.claim === null ? (
          <span className={textSecondaryOnCanvas}>{row.original.assignee ?? "Nobody"}</span>
        ) : (
          <AgentName actor={row.original.claim.actor} />
        ),
    },
    changed: {
      accessorFn: (issue) => issue.changed_at,
      cell: ({ row }) => (
        <Timestamp at={row.original.changed_at} className={textSecondaryOnCanvas} />
      ),
    },
    priorities: {
      accessorFn: (issue) => (issue.priorities ?? []).map(titleOf).join(" "),
      cell: ({ row }) => (
        <span className="flex flex-wrap gap-x-2">
          {(row.original.priorities ?? []).map((id) => (
            <Link className={`${linkText} ${linkHoverText}`} key={id} to={priorityPath(id)}>
              {titleOf(id)}
            </Link>
          ))}
        </span>
      ),
    },
    moved: {
      accessorFn: (issue) => issue.moved_at ?? "",
      cell: ({ row }) =>
        row.original.moved_at === null ? (
          <span className={textMutedOnCanvas}>{NOT_RECORDED}</span>
        ) : (
          <span className="flex flex-wrap items-center gap-1">
            <Timestamp at={row.original.moved_at} className={textSecondaryOnCanvas} />
            {row.original.moved_by === null ? null : <AgentName actor={row.original.moved_by} />}
          </span>
        ),
    },
  };
}

/**
 * One fault list: the issues the server answers for it, drawn by the one table component with
 * the columns that fault names. The rows are the read's own — a fault is true of an issue as it
 * stands, and the server decides it — so this table narrows nothing itself; its search, filters,
 * sort, grouping, columns and pager are the reader's, under its own prefix in the address.
 */
export function FaultList({
  issues,
  kind,
  label,
  prefix,
  titles,
}: {
  issues: readonly PriorityIssue[];
  kind: FaultKind;
  /** What the table is called, for a reader who cannot see where it sits. */
  label: string;
  /** This table's prefix in the address. */
  prefix: string;
  /** Each priority's title by its id, so the Advances column names the priority a person
   *  wrote rather than the slug its id is. A priority this read does not hold is drawn by
   *  its id, which is the only name there is for it. */
  titles: ReadonlyMap<string, string>;
}): ReactNode {
  const shape = SHAPE[kind];
  const titleOf = useMemo(() => (id: string) => titles.get(id) ?? id, [titles]);
  const tableColumns = useMemo(() => shape.columns(titleOf), [shape, titleOf]);
  const { params, view, setFilters, setGrouping, setPagination, setSearch, setSorting } =
    useTableView(prefix, tableColumns, FAULT_SORTING);
  const layout = useTableLayout(shape.layoutKey, shape.layout);
  const [openFilter, setOpenFilter] = useState<string>();
  const rows = useMemo(() => [...issues], [issues]);
  const columns = useMemo(
    () => columnDefs<PriorityIssue>(tableColumns, renderings(titleOf), shape.byId),
    [shape, tableColumns, titleOf]
  );
  const model = useDataTable<PriorityIssue>({
    rows,
    columns,
    getRowId: (issue) => issue.key,
    layout,
    state: {
      sorting: view.sorting,
      grouping: view.grouping,
      columnFilters: view.filters,
      globalFilter: view.search,
      pagination: { pageIndex: view.page, pageSize: view.pageSize },
    },
    onSortingChange: setSorting,
    onGroupingChange: setGrouping,
    onColumnFiltersChange: setFilters,
    onGlobalFilterChange: setSearch,
    onPaginationChange: setPagination,
    viewKey: params.toString(),
    search: {
      column: "title",
      filterFn: (row, _id, search: string) => matchesFaultSearch(row.original, search),
    },
    groupOrder: {
      compare: (id, a, b) => compareHeadings(shape.byId, id, a, b),
      heading: (id, issue) => headingOf(shape.byId, id, issue),
    },
  });
  const { table } = model;
  const facets = useMemo(
    () =>
      openFilter === undefined
        ? EMPTY_FACETS
        : facetCounts(rows, shape.byId, openFilter, view.filters, view.search, matchesFaultSearch),
    [rows, shape, openFilter, view.filters, view.search]
  );
  return (
    <div className="space-y-3">
      <div className="space-y-3" ref={layout.attachAbove}>
        <TableToolbar
          searchLabel={`Search ${label}`}
          searchPlaceholder="Search issues: key, title, project"
          table={table}
        />
      </div>
      <DataTable
        empty="No issues match these filters."
        filterCounts={facets}
        label={label}
        model={model}
        onOpenFilterChange={setOpenFilter}
        openFilter={openFilter}
        renderGroup={({ entry, index, measure }) => (
          <GroupHeadingRow
            columns={table.getVisibleLeafColumns().length}
            entry={entry}
            index={index}
            measure={measure}
            noun="issue"
            show={(value) => (entry.row.groupingColumnId === "status" ? statusText(value) : value)}
          />
        )}
        renderRow={({ row, index, measure }) => (
          <TableBodyRow
            index={index}
            label={`${row.original.key} ${row.original.title}`}
            measure={measure}
            row={row}
          />
        )}
        scrollTestId={`${prefix}-table-scroll`}
      />
    </div>
  );
}
