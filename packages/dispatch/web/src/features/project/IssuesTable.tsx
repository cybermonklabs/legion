import type { FilterFn, RowSelectionState } from "@tanstack/react-table";
import { type ReactNode, useEffect, useMemo, useState } from "react";
import { useShownQuery } from "../../api/cache/shown-query";
import { userStateQuery } from "../../api/queries";
import { DataTable, useDataTable } from "../../components/DataTable";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import {
  borderDefault,
  dangerText,
  focusVisibleRing,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { useAgentRegistry } from "../refs/agent-registry";
import { setSelectedIssues } from "../search/bar-scope";
import { LandedWithoutAnswer } from "../writes/LandedWithoutAnswer";
import { ConfirmDialog } from "./ConfirmDialog";
import { EpicNameDialog } from "./EpicNameDialog";
import { useIssueFilters } from "./issue-filters";
import { useIssueTableActions } from "./issues-table-actions";
import { useIssueTableColumns } from "./issues-table-columns";
import { IssueTableSelection, IssueTableStatus, IssueTableToolbar } from "./issues-table-controls";
import { useProjectIssueItems } from "./issues-table-data";
import { useTableLayout } from "./issues-table-layout";
import {
  compareGroups,
  facetCounts,
  groupValue,
  ISSUE_TABLE_COLUMNS,
  type Item,
  isEpic,
  matchesFilters,
  matchesSearch,
  UNCLAIMED_FACET,
} from "./issues-table-model";
import { GroupRow, IssueRow } from "./issues-table-rows";
import { useTableView } from "./issues-table-view";
import { issueIsUnread } from "./UnreadDot";

const EMPTY_FACETS: ReadonlyMap<string, number> = new Map();
/** Status offers Done before the closed issues it would count are read. */
const DONE_UNLOADED: ReadonlyMap<string, readonly string[]> = new Map([["status", ["done"]]]);

const fuzzy: FilterFn<Item> = (row, _id, query: string) => matchesSearch(row.original, query);

const ACTION_WIDTH = 104;

export function IssuesTable({ project }: { project: string }): ReactNode {
  const { params, view, setFilters, setGrouping, setPagination, setSearch, setSorting } =
    useTableView();
  const { sorting, grouping, filters, search } = view;
  const pagination = { pageIndex: view.page, pageSize: view.pageSize };
  const layout = useTableLayout(project, ISSUE_TABLE_COLUMNS);
  const { attachAbove, boxWidth } = layout;
  const flags = useIssueFilters();
  const userState = useShownQuery(userStateQuery());
  const [selection, setSelection] = useState<RowSelectionState>({});
  const [openFilter, setOpenFilter] = useState<string>();

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
    issues,
    listingError,
    missingParents,
  } = useProjectIssueItems(project, { wantsDone, registry });
  const items = useMemo(() => {
    const shown = allItems.filter(
      ({ issue }) =>
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
  }, [allItems, filters, flags.needsYou, flags.unread, grouping, search, userState.data]);

  const columns = useIssueTableColumns(userState.data, boxWidth < 640);

  const model = useDataTable<Item>({
    rows: items,
    columns,
    getRowId: (item) => item.issue.key,
    layout,
    state: { sorting, grouping, columnFilters: filters, globalFilter: search, pagination },
    onSortingChange: setSorting,
    onGroupingChange: setGrouping,
    onColumnFiltersChange: setFilters,
    onGlobalFilterChange: setSearch,
    onPaginationChange: setPagination,
    viewKey: params.toString(),
    selection: { state: selection, onChange: setSelection },
    search: { column: "title", filterFn: fuzzy },
    groupOrder: { compare: compareGroups, heading: groupValue },
  });
  const { table } = model;

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
  });

  // How many parents the walk could not read: every one of them when the walk itself failed,
  // else the ones it named. The rest of the rows keep the epic they were read with.
  const unreadableAncestors =
    missingParents.length === 0
      ? 0
      : ancestry.isError
        ? missingParents.length
        : (ancestry.data?.unreadable.length ?? 0);
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

  const visibleColumns = table.getVisibleLeafColumns().length + 1;

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
            Could not load {unreadableAncestors}{" "}
            {unreadableAncestors === 1 ? "ancestor" : "ancestors"}; affected rows show Unknown epic.{" "}
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
        {/* Issues by status: a click shows only that status, a second click every status. */}
        <IssueTableStatus table={table} items={allItems} epics={epics} />

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
            <p>{interruptedDescription}</p>
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
            The selected issues have not been moved. Find the epic in the project before moving
            them.{" "}
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

      <DataTable
        empty="No issues match these filters."
        filterCounts={openFacets}
        filterUnloaded={doneLoaded ? undefined : DONE_UNLOADED}
        label="Project issues table"
        model={model}
        onOpenFilterChange={setOpenFilter}
        openFilter={openFilter}
        renderGroup={({ entry, index, measure }) => (
          <GroupRow
            columns={visibleColumns}
            viewportWidth={boxWidth}
            continued={entry.continued}
            epics={epics}
            grouping={grouping[0] ?? ""}
            index={index}
            measure={measure}
            onMerge={mergeEpic}
            shown={entry.shown}
            onRename={renameEpic}
            onStartAll={(key) =>
              startNext(
                allItems.filter((item) => item.epicKey === key).map((item) => item.issue.key)
              )
            }
            row={entry.row}
          />
        )}
        renderRow={({ row, index, measure }) => (
          <IssueRow
            busy={busy}
            index={index}
            measure={measure}
            moved={moved}
            onStartNext={(key) => startNext([key])}
            row={row}
          />
        )}
        scrollTestId="issues-table-scroll"
        trailingWidth={ACTION_WIDTH}
      />

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
