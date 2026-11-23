import { useQuery } from "@tanstack/react-query";
import { type ColumnDef, flexRender } from "@tanstack/react-table";
import { type ReactNode, useEffect, useMemo, useState } from "react";
import { userStateQuery } from "../../api/queries";
import { DataTable, useDataTable } from "../../components/DataTable";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { StatusGlyph } from "../../components/StatusGlyph";
import { borderDefault, dangerText, textSecondaryOnCanvas } from "../../theme/classes";
import { useAgentRegistry } from "../refs/agent-registry";
import type { IssueListQuery } from "../refs/routes";
import { issueStatuses, statusText } from "./board-model";
import { useIssueTableColumns } from "./issues-table-columns";
import { type QueryListing, useIssueTableItems } from "./issues-table-data";
import { type LayoutColumns, useTableLayout } from "./issues-table-layout";
import {
  COLUMN_MIN,
  COLUMNS,
  COMPACT_COLUMN_MIN,
  type ColumnId,
  facetCounts,
  type Item,
  issueQueryStart,
  priorityName,
} from "./issues-table-model";
import { IssueRow } from "./issues-table-rows";
import { useTableView } from "./issues-table-view";

/** A list's five columns, in the order it draws them (dispatch://INFRA-4935/spec#b-r-form). */
const LIST_COLUMNS = ["status", "key", "title", "priority", "asks"] as const satisfies ColumnId[];

/** The Priority column's values: `0` to `3`, and the empty value for no priority. A list offers
 *  each of them whatever its rows hold, as Status offers every status. */
const PRIORITY_VALUES = ["0", "1", "2", "3", ""];

/** The layout every list keeps its dragged widths under (`dispatch.project-table.issue-query`):
 *  the five columns, Status, Key and Title pinned in a wide box, and on a phone Status and Title,
 *  the Title cell then carrying the key above the title. */
const LIST_LAYOUT_KEY = "issue-query";
const LIST_LAYOUT: LayoutColumns = {
  ids: LIST_COLUMNS,
  hiddenByDefault: {},
  sizes: Object.fromEntries(
    COLUMNS.filter(({ id }) => (LIST_COLUMNS as readonly string[]).includes(id)).map(
      ({ id, size }) => [id, size]
    )
  ),
  minimums: COLUMN_MIN,
  pins: {
    wide: { columns: ["status", "key", "title"], share: 0.6 },
    phone: {
      columns: ["status", "title"],
      share: 0.85,
      widths: {},
      flexibleMin: COMPACT_COLUMN_MIN.title ?? 120,
    },
    flexible: "title",
  },
};

const EMPTY_FACETS: ReadonlyMap<string, number> = new Map();

/** The picks of the filter of column `id`. */
function picksOf(filters: readonly { id: string; value: unknown }[], id: string): string[] {
  const value = filters.find((filter) => filter.id === id)?.value;
  return Array.isArray(value) ? value.map(String) : [];
}

/**
 * A list of issues a document holds (`dispatch://issues?<query>`): the issue table's rows in five
 * columns - the status mark, the key, the title, the priority and the open asks - each with its
 * sort and its filter, and the pager. Above the rows stand only the query's projects and labels,
 * one line each, which only an edit of the reference changes; its statuses and priorities are the
 * picks of the Status and Priority filters, where the reader sees and changes them, and each
 * change reads the issues again. It has no bar of counts, no search, no Group by, no Columns menu,
 * no selection and no row actions (dispatch://INFRA-4935/spec#b-r-form,
 * dispatch://INFRA-4935/spec#b-r-visible-filters). Its view is in the page address under
 * `stateKey` (`useTableView`), starting from the query (`issueQueryStart`).
 */
export function IssueQueryTable({
  query,
  stateKey,
}: {
  query: IssueListQuery;
  stateKey: string;
}): ReactNode {
  const start = useMemo(() => issueQueryStart(query), [query]);
  const { params, view, setFilters, setGrouping, setPagination, setSearch, setSorting } =
    useTableView(stateKey, start);
  const { sorting, filters } = view;
  const pagination = { pageIndex: view.page, pageSize: view.pageSize };
  const layout = useTableLayout(LIST_LAYOUT_KEY, LIST_LAYOUT);
  const compact = layout.boxWidth < 640;
  const userState = useQuery(userStateQuery());
  const registry = useAgentRegistry(false);
  const [openFilter, setOpenFilter] = useState<string>();

  // The read is the query's projects and labels and the filters' picks: a value the server
  // cannot be asked for (a priority outside 0 to 3) is left to the table's own filter, which
  // matches no row by it. Each pick is held as its JSON, so the memo below changes only when a
  // pick does, whatever an address wrote into it.
  const statusKey = JSON.stringify(picksOf(filters, "status").sort());
  const priorityKey = JSON.stringify(
    picksOf(filters, "priority")
      .filter((value) => PRIORITY_VALUES.includes(value))
      .map((value) => (value === "" ? "none" : `P${value}`))
      .sort()
  );
  const listing: QueryListing = useMemo(
    () => ({
      query: {
        ...query,
        priorities: JSON.parse(priorityKey) as string[],
        statuses: JSON.parse(statusKey) as string[],
      },
    }),
    [query, statusKey, priorityKey]
  );
  const { allItems, issues, listingError } = useIssueTableItems(listing, {
    registry,
    wantsDone: false,
  });

  const registered = useIssueTableColumns(userState.data, compact, LIST_LAYOUT);
  const columns = useMemo(() => {
    const byId = new Map(registered.map((column) => [column.id, column]));
    const keyCell = byId.get("key")?.cell;
    const titleCell = byId.get("title")?.cell;
    return LIST_COLUMNS.filter((id) => !(compact && id === "key")).flatMap(
      (id): ColumnDef<Item>[] => {
        const column = byId.get(id);
        if (column === undefined) return [];
        const listed = { ...column, enableGrouping: false };
        if (id === "status")
          return [
            {
              ...listed,
              cell: ({ row }) => (
                <span title={statusText(row.original.issue.status)}>
                  <StatusGlyph status={row.original.issue.status} />
                </span>
              ),
            },
          ];
        if (id === "priority")
          return [
            {
              ...listed,
              cell: ({ row }) => (
                <span className="whitespace-nowrap">
                  {priorityName(String(row.original.issue.priority ?? ""))}
                </span>
              ),
              meta: { ...listed.meta, always: PRIORITY_VALUES },
            },
          ];
        // On a phone the Key column is gone and the key stands above the title.
        if (id === "title" && compact)
          return [
            {
              ...listed,
              cell: (context) => (
                <div className="min-w-0 space-y-1">
                  {flexRender(keyCell, context)}
                  {flexRender(titleCell, context)}
                </div>
              ),
            },
          ];
        return [listed];
      }
    );
  }, [registered, compact]);

  const model = useDataTable<Item>({
    rows: allItems,
    columns,
    getRowId: (item) => item.issue.key,
    layout,
    state: { sorting, grouping: [], columnFilters: filters, globalFilter: "", pagination },
    onSortingChange: setSorting,
    onGroupingChange: setGrouping,
    onColumnFiltersChange: setFilters,
    onGlobalFilterChange: setSearch,
    onPaginationChange: setPagination,
    viewKey: params.toString(),
  });
  const { table } = model;

  const pageCount = table.getPageCount();
  useEffect(() => {
    if (issues.data && pagination.pageIndex >= Math.max(pageCount, 1))
      table.setPageIndex(Math.max(pageCount - 1, 0));
  }, [issues.data, pagination.pageIndex, pageCount, table]);
  const openFacets = useMemo(
    () => (openFilter ? facetCounts(allItems, openFilter, filters, "") : EMPTY_FACETS),
    [allItems, openFilter, filters]
  );
  // The statuses and priorities the read left out stand in their filters with no count.
  const unloaded = useMemo(() => {
    const { priorities, statuses } = listing.query;
    return new Map([
      ["status", statuses.length === 0 ? [] : issueStatuses.filter((s) => !statuses.includes(s))],
      [
        "priority",
        priorities.length === 0
          ? []
          : PRIORITY_VALUES.filter(
              (value) => !priorities.includes(value === "" ? "none" : `P${value}`)
            ),
      ],
    ]);
  }, [listing]);

  const fields = [
    ["Project", query.projects],
    ["Label", query.labels],
  ] as const;
  const name = `Issues: ${
    fields
      .filter(([, values]) => values.length > 0)
      .map(([field, values]) => `${field} ${values.join(", ")}`)
      .join("; ") || "every project"
  }`;

  if (issues.isPending || userState.isPending) return <LoadingSkeleton label="Loading issues" />;
  if ((issues.isError && issues.data === undefined) || userState.isError)
    return (
      <p className={dangerText} role="alert">
        Could not load issues.{" "}
        <button
          type="button"
          onClick={() => {
            void issues.refetch();
            void userState.refetch();
          }}
        >
          Retry loading issues
        </button>
      </p>
    );

  return (
    <section aria-label={name} className="my-3 space-y-2">
      <div className="flex flex-wrap items-center gap-2" ref={layout.attachAbove}>
        {fields.map(([field, values]) =>
          values.length === 0 ? null : (
            <span
              className={`inline-flex min-h-8 items-center rounded-full border px-3 text-xs font-medium ${borderDefault} ${textSecondaryOnCanvas}`}
              key={field}
            >
              {field}: {values.join(", ")}
            </span>
          )
        )}
      </div>
      {listingError ? (
        <p className={dangerText} role="alert">
          Could not load issues.{" "}
          <button type="button" onClick={() => void listingError.refetch()}>
            Retry
          </button>
        </p>
      ) : null}
      <DataTable
        empty="No issues match these filters."
        filterCounts={openFacets}
        filterUnloaded={unloaded}
        fitRows
        label={`${name} table`}
        model={model}
        noun={{ many: "issues", one: "issue" }}
        onOpenFilterChange={setOpenFilter}
        openFilter={openFilter}
        renderGroup={() => null}
        renderRow={({ row, index, measure }) => (
          <IssueRow index={index} measure={measure} row={row} />
        )}
        scrollTestId={`${stateKey}-scroll`}
      />
    </section>
  );
}
