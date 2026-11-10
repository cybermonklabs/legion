import { useQuery } from "@tanstack/react-query";
import {
  type ExpandedState,
  type FilterFn,
  getCoreRowModel,
  getExpandedRowModel,
  getFilteredRowModel,
  getGroupedRowModel,
  getPaginationRowModel,
  getSortedRowModel,
  type Row,
  type RowModel,
  type RowSelectionState,
  type Table,
  useReactTable,
} from "@tanstack/react-table";
import { useVirtualizer } from "@tanstack/react-virtual";
import { type ReactNode, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { userStateQuery } from "../../api/queries";
import { InfoHint } from "../../components/InfoHint";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import {
  borderDefault,
  dangerText,
  focusVisibleRing,
  surfaceBg,
  surfaceMutedBg,
  surfaceMutedHoverBg,
  textMutedOnCanvas,
  textMutedOnSurfaceMuted,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { useAgentRegistry } from "../refs/agent-registry";
import { setSelectedIssues } from "../search/bar-scope";
import { ViewportAnchor } from "../shell/ViewportAnchor";
import { LandedWithoutAnswer } from "../writes/LandedWithoutAnswer";
import { ConfirmDialog } from "./ConfirmDialog";
import { EpicNameDialog } from "./EpicNameDialog";
import { useIssueFilters } from "./issue-filters";
import { useIssueTableActions } from "./issues-table-actions";
import { useIssueTableColumns } from "./issues-table-columns";
import {
  IssueTablePager,
  IssueTableSelection,
  IssueTableStatus,
  IssueTableToolbar,
} from "./issues-table-controls";
import { useProjectIssueItems } from "./issues-table-data";
import { useRowKeymap } from "./issues-table-keys";
import { useTableLayout } from "./issues-table-layout";
import {
  clearPicked,
  compareGroups,
  facetCounts,
  groupValue,
  ISSUES_TAB_HIDDEN,
  type Item,
  isEpic,
  matchesFilters,
  matchesSearch,
  readSelection,
  UNCLAIMED_FACET,
} from "./issues-table-model";
import { GroupRow, IssueRow, TableHeaderCell } from "./issues-table-rows";
import { useTableView } from "./issues-table-view";
import { phaseHoursTotal } from "./PhaseHoursBar";
import { issueIsUnread } from "./UnreadDot";

const EMPTY_FACETS: ReadonlyMap<string, number> = new Map();
/** Held outside the component so the stored layout is read once per project, not per render. */
const ISSUES_TAB_LAYOUT = { hiddenByDefault: ISSUES_TAB_HIDDEN };

const fuzzy: FilterFn<Item> = (row, _id, query: string) => matchesSearch(row.original, query);

const ACTION_WIDTH = 104;

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
const sortedByGroup = (instance: Table<Item>) => {
  const sorted = getSortedRowModel<Item>()(instance);
  let last: { from: RowModel<Item>; grouping: string; model: RowModel<Item> } | undefined;
  return () => {
    const model = sorted();
    const grouped = instance.getState().grouping[0];
    if (grouped === undefined) return model;
    if (last?.from === model && last.grouping === grouped) return last.model;
    const heading = (row: Row<Item>) =>
      row.getIsGrouped() ? String(row.groupingValue ?? "") : groupValue(grouped, row.original);
    const ordered = {
      ...model,
      rows: [...model.rows].sort((a, b) => compareGroups(grouped, heading(a), heading(b))),
    };
    last = { from: model, grouping: grouped, model: ordered };
    return ordered;
  };
};

export function IssuesTable({ project }: { project: string }): ReactNode {
  const { params, setParams, view, setFilters, setGrouping, setPagination, setSearch, setSorting } =
    useTableView();
  const { sorting, grouping, filters, search } = view;
  const pagination = { pageIndex: view.page, pageSize: view.pageSize };
  const flags = useIssueFilters();
  const userState = useQuery(userStateQuery());
  const [expanded, setExpanded] = useState<ExpandedState>(true);
  const [selection, setSelection] = useState<RowSelectionState>({});
  const [openFilter, setOpenFilter] = useState<string>();
  const [dragColumn, setDragColumn] = useState<string>();

  const statusPicked = (filters.find((filter) => filter.id === "status")?.value ?? []) as string[];
  const wantsDone = statusPicked.length === 0 || statusPicked.includes("done");
  const claimPicked = (filters.find((filter) => filter.id === "claim")?.value ?? []) as string[];
  // Which claims have lapsed is the agent registry's answer, read only while the Claimed by
  // filter asks for it: because it is picked, or because its panel is open and counting.
  const wantsFree = claimPicked.includes(UNCLAIMED_FACET);
  const registry = useAgentRegistry(wantsFree || openFilter === "claim");
  const {
    all,
    allItems,
    ancestry,
    doneLoaded,
    epics,
    everyIssue,
    hasLegion,
    issues,
    legion,
    listingError,
    missingParents,
  } = useProjectIssueItems(project, { wantsDone, registry });
  // The box and the pinned widths follow the window and the longest key the listing holds, so
  // the Key column writes a key in full at every width.
  const layout = useTableLayout(
    project,
    useMemo(() => all.reduce((most, issue) => Math.max(most, issue.key.length), 0), [all]),
    ISSUES_TAB_LAYOUT
  );
  const { attachAbove, attachBox, boxHeight, boxWidth, pagerRef, responsive, scrollRef } = layout;
  // The issues behind a number the reader clicked on the Legion tab, if they arrived by one.
  const picked = useMemo(() => readSelection(params), [params]);
  const items = useMemo(() => {
    const shown = allItems.filter(
      ({ issue }) =>
        (picked === null || picked.keys.has(issue.key)) &&
        (!flags.needsYou || issue.open_asks > 0) &&
        (!flags.unread || issueIsUnread(issue, userState.data?.[issue.key]?.last_read_seq ?? 0))
    );
    if (grouping[0] !== "epic") return shown;
    // An epic is an ordinary row, except where it is already a group's heading. A heading is
    // made by the children the table lists — the same filters and search it applies to every
    // row — so an epic none of the listed rows belongs to keeps its own row rather than leaving
    // the view with nothing to show it, select it or act on it.
    const headings = new Set(
      shown
        .filter(
          (item) =>
            !isEpic(item.issue) && matchesFilters(item, filters) && matchesSearch(item, search)
        )
        .map(({ epicKey }) => epicKey)
    );
    return shown.filter(({ issue }) => !isEpic(issue) || !headings.has(issue.key));
  }, [allItems, filters, flags.needsYou, flags.unread, grouping, picked, search, userState.data]);

  // One scale for the Time per phase bars: the largest total among the rows the table lists.
  const phaseHoursMax = useMemo(
    () =>
      items.reduce(
        (most, item) => Math.max(most, item.phaseHours ? phaseHoursTotal(item.phaseHours) : 0),
        0
      ) || undefined,
    [items]
  );
  const columns = useIssueTableColumns(userState.data, boxWidth < 640, hasLegion, phaseHoursMax);

  const table = useReactTable({
    data: items,
    columns,
    state: {
      sorting,
      grouping,
      columnFilters: filters,
      columnVisibility: layout.visibility,
      pagination,
      globalFilter: search,
      expanded,
      rowSelection: selection,
      columnSizing: responsive.sizing,
      columnOrder: layout.order,
      columnPinning: { left: responsive.pinned },
    },
    getRowId: (item) => item.issue.key,
    onColumnSizingChange: layout.setSizing,
    onColumnOrderChange: layout.setOrder,
    columnResizeMode: "onChange",
    onSortingChange: setSorting,
    onGroupingChange: setGrouping,
    onColumnFiltersChange: setFilters,
    onColumnVisibilityChange: layout.setVisibility,
    onPaginationChange: setPagination,
    onGlobalFilterChange: setSearch,
    onExpandedChange: setExpanded,
    onRowSelectionChange: setSelection,
    globalFilterFn: fuzzy,
    getColumnCanGlobalFilter: (column) => column.id === "title",
    enableRowSelection: (row) => !row.getIsGrouped(),
    autoResetPageIndex: false,
    // A page holds leaf rows, so a big group spans pages instead of making one page huge.
    paginateExpandedRows: true,
    autoResetExpanded: false,
    groupedColumnMode: false,
    getCoreRowModel: getCoreRowModel(),
    getFilteredRowModel: getFilteredRowModel(),
    getSortedRowModel: sortedByGroup,
    getGroupedRowModel: getGroupedRowModel(),
    getExpandedRowModel: getExpandedRowModel(),
    getPaginationRowModel: getPaginationRowModel(),
  });

  // The keys the table lists under the filters and the search it is showing, in row order. A key
  // it no longer lists drops out of the selection - another filter, another project - so a row
  // that is listed again comes back unselected; this is the rule `hooks/useMarkedKeys.ts` holds
  // the Inbox's marks to, written against the table's own `rowSelection` rather than a second
  // selection beside it. Joined into a string, so the pruning runs when the keys change rather
  // than on every render that rebuilds the rows, and `undefined` while the listing is still out,
  // which prunes nothing.
  const listedKeys =
    issues.data === undefined
      ? undefined
      : table
          .getFilteredRowModel()
          .rows.map((row) => row.id)
          .join(" ");
  useEffect(() => {
    if (listedKeys === undefined) return;
    const listed = new Set(listedKeys === "" ? [] : listedKeys.split(" "));
    setSelection((current) =>
      Object.keys(current).every((key) => listed.has(key))
        ? current
        : Object.fromEntries(Object.entries(current).filter(([key]) => listed.has(key)))
    );
  }, [listedKeys]);

  // What the bar says it will act on when it opens over this table: the issues the reader has
  // selected, in the order the table lists them. It belongs to the table while it is mounted,
  // so leaving the page - the Board, another project, anywhere - clears it again.
  const selectedSignature = table
    .getFilteredRowModel()
    .rows.filter((row) => row.getIsSelected())
    .map((row) => row.id)
    .join(" ");
  useEffect(() => {
    setSelectedIssues(selectedSignature === "" ? [] : selectedSignature.split(" "));
    return () => setSelectedIssues([]);
  }, [selectedSignature]);

  const {
    busy,
    notice,
    moved,
    duplicateEpic,
    setDuplicateEpic,
    unansweredEpic,
    dismissUnansweredEpic,
    moveTo,
    startNext,
    newEpic,
    renameEpic,
    mergeEpic,
    naming,
    cancelNaming,
    confirming,
    cancelConfirming,
    confirmed,
    interrupted,
    interruptedDescription,
    finish,
    dismiss,
  } = useIssueTableActions({
    project,
    issues: all,
    items: allItems,
    clearSelection: () => setSelection({}),
    hasLegion,
  });

  // How many parents the walk could not read: every one of them when the walk itself failed,
  // else the ones it named. The rest of the rows keep the epic they were read with.
  const unreadableAncestors =
    missingParents.length === 0
      ? 0
      : ancestry.isError
        ? missingParents.length
        : (ancestry.data?.unreadable.length ?? 0);
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
  const displayRows: { row: Row<Item>; continued?: boolean; shown?: number }[] = [
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
    getScrollElement: () => scrollRef.current,
    estimateSize: (index) => (displayRows[index]?.row.getIsGrouped() ? 48 : 64),
    getItemKey: (index) =>
      `${displayRows[index]?.continued ? "continued-" : ""}${displayRows[index]?.row.id}`,
    initialRect: { width: 1000, height: 600 },
    overscan: 12,
  });

  useRowKeymap({
    displayRows,
    rowsById: table.getCoreRowModel().rowsById,
    scrollRef,
    scrollToIndex: (index) => virtualizer.scrollToIndex(index, { align: "auto" }),
  });
  const controlSignature = params.toString();
  const previousControls = useRef(controlSignature);
  useLayoutEffect(() => {
    if (previousControls.current !== controlSignature) scrollRef.current?.scrollTo({ top: 0 });
    previousControls.current = controlSignature;
  }, [controlSignature, scrollRef]);
  const pageCount = table.getPageCount();
  useEffect(() => {
    if (
      (!wantsDone || everyIssue.data !== undefined) &&
      issues.data &&
      pagination.pageIndex >= Math.max(pageCount, 1)
    )
      table.setPageIndex(Math.max(pageCount - 1, 0));
  }, [wantsDone, everyIssue.data, issues.data, pagination.pageIndex, pageCount, table]);
  const openFacets = useMemo(
    () => (openFilter ? facetCounts(items, openFilter, filters, search) : EMPTY_FACETS),
    [items, openFilter, filters, search]
  );

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
            if (missingParents.length) void ancestry.refetch();
          }}
        >
          Retry loading issues
        </button>
      </p>
    );

  const leafCount = table.getFilteredRowModel().rows.length;
  const visibleColumns = table.getVisibleLeafColumns().length + 1;
  // The order a drop rewrites, read once rather than per header cell.
  const columnOrder = table.getAllLeafColumns().map((column) => column.id);

  return (
    <section aria-label="Project issues" className="space-y-3 sm:pb-24">
      <div className="space-y-3" ref={attachAbove}>
        {listingError ? (
          <p className={dangerText} role="alert">
            Could not load {wantsDone ? "closed " : ""}issues.{" "}
            <button
              type="button"
              aria-label={wantsDone ? "Retry loading closed issues" : "Retry loading issues"}
              onClick={() => void listingError.refetch()}
            >
              Retry
            </button>
          </p>
        ) : null}
        {unreadableAncestors > 0 ? (
          <p className={dangerText} role="alert">
            {unreadableAncestors} {unreadableAncestors === 1 ? "parent" : "parents"} did not load.{" "}
            <InfoHint label="About the parents that did not load">
              A row whose parent did not load reads Unknown epic. Every other row is as it was.
            </InfoHint>{" "}
            <button type="button" onClick={() => void ancestry.refetch()}>
              Retry loading ancestors
            </button>
          </p>
        ) : null}
        {wantsDone && everyIssue.isPending ? (
          <p aria-live="polite" className={textMutedOnCanvas}>
            Loading closed issues…
          </p>
        ) : null}
        {hasLegion && legion.isError ? (
          <p className={dangerText} role="alert">
            Legion data did not refresh.{" "}
            <button type="button" onClick={() => void legion.refetch()}>
              Retry Legion data
            </button>
          </p>
        ) : null}
        {hasLegion && legion.isPending ? (
          <p className={textMutedOnCanvas}>Loading Legion data…</p>
        ) : null}
        {/* Issues by phase or status: a click shows only that one, a second click every one. */}
        <IssueTableStatus
          table={table}
          items={allItems}
          hasLegion={hasLegion}
          board={legion.data}
        />
        {picked === null ? null : (
          <p className={`flex flex-wrap items-center gap-2 text-sm ${textSecondaryOnCanvas}`}>
            <span
              className={`inline-flex min-h-9 items-center gap-2 rounded-full border px-3 ${borderDefault} ${surfaceMutedBg}`}
            >
              Showing: <b className={textPrimaryOnCanvas}>{picked.label}</b>
              <span className={textMutedOnSurfaceMuted}>
                · {items.length} of {picked.keys.size} {picked.keys.size === 1 ? "issue" : "issues"}
              </span>
              <button
                aria-label={`Stop showing only ${picked.label}`}
                className={`-mr-1 rounded-full px-1.5 ${surfaceMutedHoverBg}`}
                onClick={() => setParams((current) => clearPicked(current), { replace: false })}
                type="button"
              >
                ×
              </button>
            </span>
          </p>
        )}

        <IssueTableToolbar
          table={table}
          unclaimedUnchecked={
            wantsFree && registry.isError
              ? registry.refilling
                ? "refilling"
                : "unreachable"
              : undefined
          }
        />

        <p
          aria-live="polite"
          role={notice ? (notice.kind === "error" ? "alert" : "status") : undefined}
          className={`min-h-5 text-sm ${notice?.kind === "error" ? dangerText : textSecondaryOnCanvas}`}
        >
          {notice?.text}
        </p>
        {interrupted ? (
          <section aria-label="Interrupted table action" className={textSecondaryOnCanvas}>
            <p>
              {interruptedDescription}{" "}
              <InfoHint label="About the unfinished action">
                Finish reads the project again and sends only the steps that are left. Dismiss
                forgets them. Writes already sent stand either way.
              </InfoHint>
            </p>
            <button
              type="button"
              onClick={finish}
              className={`min-h-11 rounded-lg border px-3 text-sm ${borderDefault} ${focusVisibleRing}`}
            >
              Finish
            </button>{" "}
            <button
              type="button"
              onClick={dismiss}
              className={`min-h-11 rounded-lg border px-3 text-sm ${borderDefault} ${focusVisibleRing}`}
            >
              Dismiss
            </button>
          </section>
        ) : null}
        {unansweredEpic ? (
          <LandedWithoutAnswer what={`The epic “${unansweredEpic}”`}>
            Nothing was moved into it.{" "}
            <InfoHint label="About the epic with no answer">
              Dispatch sent the epic and got no answer, so it may exist. Find it in the project
              before you move issues into it.
            </InfoHint>{" "}
            <button type="button" onClick={dismissUnansweredEpic}>
              Dismiss
            </button>
          </LandedWithoutAnswer>
        ) : null}
        {duplicateEpic ? (
          <div role="alert" className={textSecondaryOnCanvas}>
            <p>{duplicateEpic.message}</p>
            <button
              type="button"
              disabled={busy}
              onClick={() => void newEpic(duplicateEpic.keys, duplicateEpic.title, true)}
            >
              Create anyway
            </button>
            <button type="button" onClick={() => setDuplicateEpic(undefined)}>
              Cancel new epic
            </button>
          </div>
        ) : null}
      </div>

      <div
        className={`min-w-0 overflow-auto rounded-xl border ${borderDefault}`}
        ref={attachBox}
        data-testid="issues-table-scroll"
        style={{
          overflowAnchor: "none",
          height: boxHeight,
          scrollPaddingTop: 64,
        }}
      >
        <ViewportAnchor
          item="data-issue-row"
          className="w-max min-w-full"
          group="data-issue-group"
          scroller={scrollRef}
          enabled={() => previousControls.current === controlSignature}
        >
          <table
            aria-label="Project issues table"
            className="table-fixed border-separate border-spacing-0 text-sm"
            style={{ width: table.getTotalSize() + ACTION_WIDTH }}
          >
            <thead>
              {table.getHeaderGroups().map((group) => (
                <tr key={group.id}>
                  {group.headers.map((header) => (
                    <TableHeaderCell
                      compact={boxWidth < 640}
                      counts={header.column.id === openFilter ? openFacets : EMPTY_FACETS}
                      doneLoaded={doneLoaded}
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
                const entry = displayRows[virtual.index];
                if (entry === undefined) return null;
                const { row } = entry;
                return row.getIsGrouped() ? (
                  <GroupRow
                    columns={visibleColumns}
                    viewportWidth={boxWidth}
                    continued={entry.continued}
                    epics={epics}
                    grouping={grouping[0] ?? ""}
                    index={virtual.index}
                    key={`${entry.continued ? "continued-" : ""}${row.id}`}
                    measure={virtualizer.measureElement}
                    onMerge={mergeEpic}
                    shown={entry.shown}
                    onRename={renameEpic}
                    onStartAll={(key) =>
                      startNext(
                        allItems
                          .filter((item) => item.epicKey === key)
                          .map((item) => item.issue.key)
                      )
                    }
                    row={row}
                  />
                ) : (
                  <IssueRow
                    busy={busy}
                    index={virtual.index}
                    key={row.id}
                    measure={virtualizer.measureElement}
                    moved={moved}
                    onStartNext={(key) => startNext([key])}
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
        </ViewportAnchor>
        {leafCount === 0 ? (
          <p className={`p-6 text-center text-sm ${textMutedOnCanvas}`}>
            No issues match these filters.
          </p>
        ) : null}
      </div>

      <IssueTablePager table={table} items={items} pagerRef={pagerRef} />

      <IssueTableSelection
        table={table}
        epics={epics}
        busy={busy}
        unansweredEpic={unansweredEpic}
        startNext={startNext}
        moveTo={moveTo}
        newEpic={newEpic}
      />
      {confirming === undefined ? null : (
        <ConfirmDialog
          body={confirming.body}
          confirmLabel={confirming.confirmLabel}
          key={confirming.title}
          onCancel={cancelConfirming}
          onConfirm={confirmed}
          title={confirming.title}
        />
      )}
      {naming === undefined ? null : naming.kind === "new" ? (
        <EpicNameDialog
          key="new-epic"
          onCancel={cancelNaming}
          onSubmit={(name) => void newEpic(naming.keys, name)}
          submitLabel="Create epic"
          title="Name the new epic"
        />
      ) : (
        <EpicNameDialog
          initial={naming.title}
          key={`rename-${naming.key}`}
          onCancel={cancelNaming}
          onSubmit={(name) => renameEpic(naming.key, name)}
          submitLabel="Rename epic"
          title="Rename the epic"
        />
      )}
    </section>
  );
}
