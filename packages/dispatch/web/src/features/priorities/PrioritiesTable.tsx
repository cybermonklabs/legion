import { type ReactNode, useCallback, useMemo, useState } from "react";
import type { PriorityRow } from "../../api/types";
import { DataTable, useDataTable } from "../../components/DataTable";
import {
  borderDefault,
  focusVisibleRing,
  surfaceBg,
  surfaceMutedHoverBg,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { IssuesTable } from "../project/IssuesTable";
import { useTableLayout } from "../project/issues-table-layout";
import { usePriorityColumns } from "./priorities-columns";
import {
  buildPriorityItems,
  matchesPrioritySearch,
  PRIORITY_COLUMNS,
  PRIORITY_COLUMNS_BY_ID,
  PRIORITY_LAYOUT,
  PRIORITY_SORTING,
  type PriorityItem,
  rankMove,
} from "./priorities-model";
import { GroupHeadingRow, TableToolbar } from "./priority-table-controls";
import { compareHeadings, facetCounts, headingOf } from "./priority-table-model";
import { TableBodyRow } from "./priority-table-rows";
import { useTableView } from "./priority-table-view";
import { usePriorityWrites } from "./priority-writes";

const EMPTY_FACETS: ReadonlyMap<string, number> = new Map();
/** The layout key of the priorities table: one table, one deployment, one stored layout. */
const LAYOUT_KEY = "priorities";
/** The address prefix of this table's own view, so the two fault lists keep their own. */
const VIEW_PREFIX = "p";
/** The address prefix of the work under an opened row, which is the issues table's own view. */
const WORK_PREFIX = "w";

/**
 * The priorities themselves: one row per priority in the one order, with the counts that say
 * what is happening under each. A reader who may write a priority adds one, renames one, writes
 * its description and drags its order here; every reader opens a row, which shows the whole of
 * that priority's description and the work advancing it.
 */
export function PrioritiesTable({ rows }: { rows: readonly PriorityRow[] }): ReactNode {
  const { params, setParams, view, setFilters, setGrouping, setPagination, setSearch, setSorting } =
    useTableView(VIEW_PREFIX, PRIORITY_COLUMNS, PRIORITY_SORTING);
  const layout = useTableLayout(LAYOUT_KEY, PRIORITY_LAYOUT);
  const { attachAbove } = layout;
  const [openId, setOpenId] = useState<string>();
  const [dragging, setDragging] = useState<string>();
  const [adding, setAdding] = useState(false);
  const [newTitle, setNewTitle] = useState("");
  const [openFilter, setOpenFilter] = useState<string>();
  const { add, canWrite, patch } = usePriorityWrites();

  const items = useMemo(() => buildPriorityItems(rows), [rows]);
  const order = useMemo(() => items.map((item) => item.priority.id), [items]);
  const open = items.find((item) => item.priority.id === openId);
  // A drag writes the place the reader can see. Under another sort, a grouping or a page of
  // rows that is not the whole order, where a row lands is not where it was dropped, so the
  // handle is not drawn at all.
  const canReorder =
    view.grouping.length === 0 &&
    view.sorting.length === 1 &&
    view.sorting[0]?.id === "order" &&
    view.sorting[0]?.desc === false;

  const move = useCallback(
    (item: PriorityItem, onto: string) => {
      const rank = rankMove(order, item.priority.id, onto);
      if (rank === undefined) return;
      patch.mutate({
        id: item.priority.id,
        input: { rank },
        label: `Move the priority “${item.priority.title}” in the order`,
      });
    },
    [order, patch.mutate]
  );
  const onDragEnd = useCallback(() => setDragging(undefined), []);
  const onDescribe = useCallback(
    (item: PriorityItem, description: string) =>
      patch.mutate({
        id: item.priority.id,
        input: { description },
        label: `Description of the priority “${item.priority.title}”`,
      }),
    [patch.mutate]
  );
  const onMove = useCallback(
    (item: PriorityItem, by: -1 | 1) => {
      const onto = order[order.indexOf(item.priority.id) + by];
      if (onto !== undefined) move(item, onto);
    },
    [move, order]
  );
  const onOpen = useCallback(
    (item: PriorityItem) => {
      const next = openId === item.priority.id ? undefined : item.priority.id;
      setOpenId(next);
      // The work under a priority is read by status, as the page's rule says, unless the
      // reader has already grouped that table by something else.
      if (next !== undefined && !params.has(`${WORK_PREFIX}.group`))
        setParams(
          (current) => {
            const changed = new URLSearchParams(current);
            changed.set(`${WORK_PREFIX}.group`, "status");
            return changed;
          },
          { replace: true }
        );
    },
    [openId, params, setParams]
  );
  const onRename = useCallback(
    (item: PriorityItem, title: string) => {
      const next = title.trim();
      if (next === "" || next === item.priority.title) return;
      patch.mutate({
        id: item.priority.id,
        input: { title: next },
        label: `Rename the priority “${item.priority.title}” to “${next}”`,
      });
    },
    [patch.mutate]
  );
  // One object while nothing a row can do changes, since the column declarations are built
  // over it (`usePriorityColumns`) and the table is built over those.
  const actions = useMemo(
    () => ({
      canReorder,
      canWrite,
      dragging,
      onDragEnd,
      onDragStart: setDragging,
      onDescribe,
      onMove,
      onOpen,
      onRename,
      openId,
    }),
    [canReorder, canWrite, dragging, onDescribe, onDragEnd, onMove, onOpen, onRename, openId]
  );
  const columns = usePriorityColumns(actions);
  const model = useDataTable<PriorityItem>({
    rows: items,
    columns,
    getRowId: (item) => item.priority.id,
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
      filterFn: (row, _id, search: string) => matchesPrioritySearch(row.original, search),
    },
    groupOrder: {
      compare: (id, a, b) => compareHeadings(PRIORITY_COLUMNS_BY_ID, id, a, b),
      heading: (id, item) => headingOf(PRIORITY_COLUMNS_BY_ID, id, item),
    },
  });
  const { table } = model;
  const facets = useMemo(
    () =>
      openFilter === undefined
        ? EMPTY_FACETS
        : facetCounts(
            items,
            PRIORITY_COLUMNS_BY_ID,
            openFilter,
            view.filters,
            view.search,
            matchesPrioritySearch
          ),
    [items, openFilter, view.filters, view.search]
  );
  const visibleColumns = table.getVisibleLeafColumns().length;

  return (
    <section aria-label="Priorities" className="space-y-3">
      <div className="space-y-3" ref={attachAbove}>
        <TableToolbar
          searchLabel="Search priorities"
          searchPlaceholder="Search priorities: title, owner"
          table={table}
        >
          {canWrite ? (
            <button
              className={`rounded-lg border px-3 py-1.5 text-sm ${borderDefault} ${surfaceMutedHoverBg}`}
              onClick={() => setAdding((was) => !was)}
              type="button"
            >
              New priority
            </button>
          ) : null}
        </TableToolbar>
        {adding ? (
          <form
            aria-label="New priority"
            className="flex flex-wrap items-center gap-2"
            onSubmit={(event) => {
              event.preventDefault();
              const title = newTitle.trim();
              if (title === "") return;
              add.mutate({ title });
              setNewTitle("");
              setAdding(false);
            }}
          >
            <input
              aria-label="Title of the new priority"
              className={`min-w-64 flex-1 rounded-lg border px-3 py-1.5 text-sm ${borderDefault} ${surfaceBg} ${textPrimaryOnCanvas}`}
              onChange={(event) => setNewTitle(event.target.value)}
              placeholder="What the new priority is"
              value={newTitle}
            />
            <button
              className={`min-h-11 rounded-lg border px-3 text-sm disabled:opacity-40 ${borderDefault} ${surfaceMutedHoverBg} ${focusVisibleRing}`}
              disabled={newTitle.trim() === ""}
              type="submit"
            >
              Add priority
            </button>
            <button
              className={`min-h-11 rounded-lg border px-3 text-sm ${borderDefault} ${surfaceMutedHoverBg}`}
              onClick={() => {
                setAdding(false);
                setNewTitle("");
              }}
              type="button"
            >
              Cancel
            </button>
          </form>
        ) : null}
      </div>

      <DataTable
        empty="No priorities match these filters."
        filterCounts={facets}
        label="Priorities table"
        model={model}
        noun={{ many: "priorities", one: "priority" }}
        onOpenFilterChange={setOpenFilter}
        openFilter={openFilter}
        renderGroup={({ entry, index, measure }) => (
          <GroupHeadingRow
            columns={visibleColumns}
            entry={entry}
            index={index}
            measure={measure}
            noun={{ many: "priorities", one: "priority" }}
          />
        )}
        renderRow={({ row, index, measure }) => (
          <TableBodyRow
            dragging={dragging}
            index={index}
            measure={measure}
            onDrop={(onto) => {
              const item = items.find((candidate) => candidate.priority.id === dragging);
              setDragging(undefined);
              if (item !== undefined) move(item, onto);
            }}
            label={`${row.original.order} ${row.original.priority.title}`}
            row={row}
          />
        )}
        scrollTestId="priorities-table-scroll"
      />

      {open === undefined ? null : (
        <section aria-label={`Work advancing ${open.priority.title}`} className="space-y-2">
          <h3 className={`text-sm font-semibold ${textSecondaryOnCanvas}`}>
            Work advancing {open.order} {open.priority.title}
          </h3>
          <IssuesTable listing={{ advances: open.priority.id }} stateKey={WORK_PREFIX} />
        </section>
      )}
    </section>
  );
}
