import { useQuery } from "@tanstack/react-query";
import type { FilterFn, RowSelectionState } from "@tanstack/react-table";
import { type ReactNode, useCallback, useEffect, useMemo, useState } from "react";
import { userStateQuery } from "../../api/queries";
import { InfoHint } from "../../components/InfoHint";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import {
  borderDefault,
  dangerText,
  focusVisibleRing,
  surfaceMutedBg,
  surfaceMutedHoverBg,
  textMutedOnCanvas,
  textMutedOnSurfaceMuted,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { useAgentRegistry } from "../refs/agent-registry";
import { setSelectedIssues } from "../search/bar-scope";
import { LandedWithoutAnswer } from "../writes/LandedWithoutAnswer";
import { ConfirmDialog } from "./ConfirmDialog";
import { DataTable, useDataTable } from "./DataTable";
import { EpicNameDialog } from "./EpicNameDialog";
import { useIssueFilters } from "./issue-filters";
import { useIssueTableActions } from "./issues-table-actions";
import { useIssueTableColumns } from "./issues-table-columns";
import { IssueTableSelection, IssueTableStatus, IssueTableToolbar } from "./issues-table-controls";
import { useProjectIssueItems } from "./issues-table-data";
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
import { GroupRow, IssueRow } from "./issues-table-rows";
import { useTableView } from "./issues-table-view";
import { phaseHoursTotal } from "./PhaseHoursBar";
import { issueIsUnread } from "./UnreadDot";

const fuzzy: FilterFn<Item> = (row, _id, query: string) => matchesSearch(row.original, query);

const ACTION_WIDTH = 104;

export function IssuesTable({ project }: { project: string }): ReactNode {
  const { params, setParams, view, setFilters, setGrouping, setPagination, setSearch, setSorting } =
    useTableView();
  const { sorting, grouping, filters, search } = view;
  const pagination = { pageIndex: view.page, pageSize: view.pageSize };
  const flags = useIssueFilters();
  const userState = useQuery(userStateQuery());
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
    hasLegion,
    issues,
    legion,
    listingError,
    missingParents,
  } = useProjectIssueItems(project, { wantsDone, registry });
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
  const columns = useIssueTableColumns(userState.data, true, hasLegion, phaseHoursMax);
  const tableColumns = useMemo(
    () =>
      columns.map((column) =>
        column.id === "status" ? { ...column, meta: { ...column.meta, doneLoaded } } : column
      ),
    [columns, doneLoaded]
  );
  const facets = useCallback(
    (id: string) => facetCounts(items, id, filters, search),
    [items, filters, search]
  );
  const model = useDataTable<Item>({
    columns: tableColumns,
    controlled: {
      filters,
      grouping,
      onFiltersChange: setFilters,
      onGroupingChange: setGrouping,
      onPaginationChange: setPagination,
      onSearchChange: setSearch,
      onSortingChange: setSorting,
      pagination,
      search,
      sorting,
    },
    facets,
    getRowId: (item) => item.issue.key,
    globalFilterFn: fuzzy,
    groupOrder: compareGroups,
    groupValue,
    hiddenByDefault: ISSUES_TAB_HIDDEN,
    layoutKey: project,
    longestKeyChars: all.reduce((most, issue) => Math.max(most, issue.key.length), 0),
    onOpenFilterChange: setOpenFilter,
    rows: items,
    searchColumn: "title",
    selection: { onChange: setSelection, state: selection },
  });
  const { layout, table } = model;
  const { attachAbove } = layout;

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
  const pageCount = table.getPageCount();
  useEffect(() => {
    if (
      (!wantsDone || everyIssue.data !== undefined) &&
      issues.data &&
      pagination.pageIndex >= Math.max(pageCount, 1)
    )
      table.setPageIndex(Math.max(pageCount - 1, 0));
  }, [wantsDone, everyIssue.data, issues.data, pagination.pageIndex, pageCount, table]);

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

      <DataTable
        label="Project issues table"
        model={model}
        scrollTestId="issues-table-scroll"
        renderGroup={({ entry, index, measure }) => (
          <GroupRow
            columns={table.getVisibleLeafColumns().length + 1}
            viewportWidth={layout.boxWidth}
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
        renderRow={({ continues, index, measure, row }) => (
          <IssueRow
            busy={busy}
            continues={continues}
            index={index}
            measure={measure}
            moved={moved}
            onStartNext={(key) => startNext([key])}
            row={row}
          />
        )}
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
