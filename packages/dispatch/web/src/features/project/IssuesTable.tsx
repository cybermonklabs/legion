import { useQuery } from "@tanstack/react-query";
import type { ColumnFiltersState, FilterFn, RowSelectionState } from "@tanstack/react-table";
import { type ReactNode, useCallback, useEffect, useLayoutEffect, useMemo, useState } from "react";
import { projectDoneIssuesQuery, userStateQuery, whoAmIQuery } from "../../api/queries";
import { InfoHint } from "../../components/InfoHint";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { useSearchParamsUpdate } from "../../lib/url-array-params";
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
import { buildProjectPath } from "../refs/routes";
import { setSelectedIssues } from "../search/bar-scope";
import { useUserPreference } from "../shell/userPreference";
import { LandedWithoutAnswer } from "../writes/LandedWithoutAnswer";
import { ConfirmDialog } from "./ConfirmDialog";
import { DataTable, useDataTable } from "./DataTable";
import { EpicNameDialog } from "./EpicNameDialog";
import { IssueViewTabs } from "./IssueViewTabs";
import { useIssueFilters } from "./issue-filters";
import { useIssueTableActions } from "./issues-table-actions";
import { type PhaseHoursDisplay, useIssueTableColumns } from "./issues-table-columns";
import { IssueTableSelection, IssueTableStatus, IssueTableToolbar } from "./issues-table-controls";
import { doneWindowOf, useHasLegion, useProjectIssueItems } from "./issues-table-data";
import {
  buildItems,
  type ColumnId,
  clearPicked,
  compareGroups,
  effectiveFilters,
  facetCounts,
  groupValue,
  type Item,
  isEpic,
  isLegionFilter,
  matchesFilters,
  matchesSearch,
  readExcludedTrees,
  readSelection,
  readUrl,
  UNCLAIMED_FACET,
  viewIsChanged,
} from "./issues-table-model";
import { GroupRow, IssueRow } from "./issues-table-rows";
import {
  activeSavedView,
  deleteView,
  readSavedViews,
  saveView,
  writeSavedViews,
} from "./issues-table-saved-views";
import { layoutKeyOf, readPhaseMode, writePhaseMode } from "./issues-table-storage";
import { useTableView } from "./issues-table-view";
import { type BuiltInViewId, viewCount, viewOf } from "./issues-table-views";
import { phaseHoursTotal } from "./PhaseHoursBar";
import { issueIsUnread } from "./UnreadDot";

const fuzzy: FilterFn<Item> = (row, _id, query: string) => matchesSearch(row.original, query);

const ACTION_WIDTH = 104;

export function IssuesTable({ project }: { project: string }): ReactNode {
  const { hasLegion, pending } = useHasLegion(project);
  if (pending) return <LoadingSkeleton label="Loading issues" />;
  return <IssuesTableContent hasLegion={hasLegion} project={project} />;
}

function IssuesTableContent({
  hasLegion,
  project,
}: {
  hasLegion: boolean;
  project: string;
}): ReactNode {
  const { params, setParams, view, setFilters, setGrouping, setPagination, setSearch, setSorting } =
    useTableView(hasLegion);
  const updateParams = useSearchParamsUpdate();
  const { sorting, grouping, filters, search } = view;
  const pagination = { pageIndex: view.page, pageSize: view.pageSize };
  const requestedView = viewOf(params);
  const activeView =
    requestedView.legionOnly && !hasLegion ? viewOf(new URLSearchParams()) : requestedView;
  const requestedBarView = viewOf(params);
  const barView =
    requestedBarView.legionOnly && !hasLegion ? viewOf(new URLSearchParams()) : requestedBarView;
  const barColumn = barView.bar;
  const barFilter =
    barColumn === null
      ? undefined
      : readUrl(barView.params(hasLegion), hasLegion).filters.find(
          (filter) => filter.id === barColumn
        );
  const barViewValue = Array.isArray(barFilter?.value) ? barFilter.value.map(String) : undefined;
  const layoutKey = layoutKeyOf(project, activeView.id);
  const [savedViewsByProject, setSavedViewsByProject] = useUserPreference(
    "project.saved-views",
    readSavedViews,
    writeSavedViews
  );
  const savedViews = savedViewsByProject[project] ?? [];
  const activeSaved = activeSavedView(savedViews, params);
  const flags = useIssueFilters();
  const userState = useQuery(userStateQuery());
  const viewer = useQuery(whoAmIQuery());
  const [selection, setSelection] = useState<RowSelectionState>({});
  const [openFilter, setOpenFilter] = useState<string>();
  const [phaseMode, setStoredPhaseMode] = useState(() => readPhaseMode(layoutKey));
  const [phaseMax, setPhaseMax] = useState<number>();
  useEffect(() => {
    setStoredPhaseMode(readPhaseMode(layoutKey));
    setPhaseMax(undefined);
  }, [layoutKey]);
  const setPhaseMode = useCallback(
    (mode: "share" | "hours") => {
      setStoredPhaseMode(mode);
      writePhaseMode(layoutKey, mode);
    },
    [layoutKey]
  );
  const phase = useMemo<PhaseHoursDisplay>(
    () => ({ max: phaseMax, mode: phaseMode, setMode: setPhaseMode }),
    [phaseMax, phaseMode, setPhaseMode]
  );
  const timelineHref = useCallback(
    (key: string) => buildProjectPath({ issue: key, kind: "evals", project }),
    [project]
  );

  const statusValue = filters.find((filter) => filter.id === "status")?.value;
  const statusPicked = Array.isArray(statusValue) ? statusValue.map(String) : [];
  const done = doneWindowOf(filters);
  const doneOnly =
    done !== null && statusPicked.length > 0 && statusPicked.every((status) => status === "done");
  const doneDefaulted = done !== null && !filters.some((filter) => filter.id === "closed");
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
    doneRead,
    epics,
    issues,
    legion,
    listingError,
    missingParents,
    openItems,
  } = useProjectIssueItems(project, {
    done,
    doneDefaulted,
    doneOnly,
    registry,
    viewer: viewer.data?.login,
  });
  const boardReady = !hasLegion || legion.data !== undefined;
  const finishedRead = useQuery({ ...projectDoneIssuesQuery(project, "28d"), enabled: false });
  const finishedItems = useMemo(() => {
    if (!finishedRead.data?.length) return [];
    return buildItems(
      finishedRead.data,
      (actor) => actor.id,
      [],
      () => true,
      hasLegion ? (legion.data ?? null) : undefined,
      viewer.data?.login
    );
  }, [finishedRead.data, hasLegion, legion.data, viewer.data?.login]);
  const counts = useMemo<Partial<Record<BuiltInViewId, number | string>>>(() => {
    if (issues.data === undefined) return {};
    const now = Date.now();
    const count = (id: BuiltInViewId, rows: readonly Item[]) =>
      viewCount(viewOf(new URLSearchParams(`view=${id}`)), rows, hasLegion, now);
    const next: Partial<Record<BuiltInViewId, number | string>> = {
      mine: count("mine", openItems),
      open: count("open", openItems),
      triage: count("triage", openItems),
    };
    if (boardReady) {
      next["in-flight"] = count("in-flight", openItems);
      if (hasLegion) {
        next.waiting = count("waiting", openItems);
        next.slots =
          legion.data === undefined
            ? undefined
            : `${legion.data.header.slots_in_use}/${legion.data.header.slot_cap}`;
      }
      if (finishedRead.data !== undefined) next.finished = count("finished", finishedItems);
    }
    return next;
  }, [
    boardReady,
    finishedItems,
    finishedRead.data,
    hasLegion,
    issues.data,
    legion.data,
    openItems,
  ]);
  const excludedTrees = useMemo(() => readExcludedTrees(params), [params]);
  const picked = useMemo(() => readSelection(params), [params]);
  const items = useMemo(() => {
    const shown = allItems.filter(
      ({ facts, issue }) =>
        (picked === null || picked.keys.has(issue.key)) &&
        !excludedTrees.has(facts?.tree ?? "") &&
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
            !isEpic(item.issue) &&
            matchesFilters(item, filters, undefined, boardReady) &&
            matchesSearch(item, search)
        )
        .map(({ epicKey }) => epicKey)
    );
    return shown.filter(({ issue }) => !isEpic(issue) || !headings.has(issue.key));
  }, [
    allItems,
    boardReady,
    excludedTrees,
    filters,
    flags.needsYou,
    flags.unread,
    grouping,
    picked,
    search,
    userState.data,
  ]);

  const columns = useIssueTableColumns(userState.data, true, hasLegion, phase, timelineHref);
  const tableColumns = useMemo(
    () =>
      columns.map((column) =>
        column.id === "status" ? { ...column, meta: { ...column.meta, doneLoaded } } : column
      ),
    [columns, doneLoaded]
  );
  const hiddenByDefault = useMemo(() => {
    const visible = new Set(activeView.cols);
    return Object.fromEntries(
      tableColumns.flatMap((column) => {
        if (column.id === undefined || column.id === "select") return [];
        return [[column.id, visible.has(column.id as ColumnId)]];
      })
    );
  }, [activeView.cols, tableColumns]);
  const columnsOverride = useMemo(() => {
    const columns = params.get("cols");
    return columns === null ? undefined : ["select", ...columns.split(",").filter(Boolean)];
  }, [params]);
  const dropColumnsOverride = useCallback(() => {
    if (!params.has("cols")) return;
    updateParams((next) => next.delete("cols"));
  }, [params, updateParams]);
  const tableFilters = effectiveFilters(filters, boardReady);
  const onFiltersChange = useCallback(
    (update: ColumnFiltersState | ((current: ColumnFiltersState) => ColumnFiltersState)) => {
      const next = typeof update === "function" ? update(tableFilters) : update;
      if (boardReady) {
        setFilters(next);
        return;
      }
      setFilters([
        ...next.filter((filter) => !isLegionFilter(filter)),
        ...filters.filter((filter) => isLegionFilter(filter)),
      ]);
    },
    [boardReady, filters, setFilters, tableFilters]
  );
  const facets = useCallback(
    (id: string) => facetCounts(items, id, filters, search, boardReady),
    [boardReady, items, filters, search]
  );
  const model = useDataTable<Item>({
    columns: tableColumns,
    controlled: {
      filters: tableFilters,
      grouping,
      onFiltersChange,
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
    columnsOverride,
    hiddenByDefault,
    layoutKey,
    longestKeyChars: all.reduce((most, issue) => Math.max(most, issue.key.length), 0),
    onOpenFilterChange: setOpenFilter,
    onVisibilityChanged: dropColumnsOverride,
    rows: items,
    searchColumn: "title",
    selection: { onChange: setSelection, state: selection },
  });
  const pagePhaseHoursMax =
    phaseMode === "hours"
      ? model.pageLeaves.reduce(
          (most, item) => Math.max(most, phaseHoursTotal(item.facts?.phase_hours ?? {})),
          0
        ) || undefined
      : undefined;
  useLayoutEffect(() => {
    if (phaseMode === "hours") setPhaseMax(pagePhaseHoursMax);
  }, [pagePhaseHoursMax, phaseMode]);
  const { layout, table } = model;
  const { attachAbove } = layout;
  const viewChanged = activeSaved === undefined && viewIsChanged(params, view, hasLegion);
  const resetView = useCallback(() => {
    const next = new URLSearchParams();
    if (activeView.id !== "open") next.set("view", activeView.id);
    setParams(next);
  }, [activeView.id, setParams]);
  const saveCurrentView = useCallback(
    (name: string) => {
      const query = new URLSearchParams(params);
      query.delete("page");
      query.delete("cols");
      const columns = table
        .getVisibleLeafColumns()
        .map((column) => column.id)
        .filter((id) => id !== "select");
      if (columns.length > 0) query.set("cols", columns.join(","));
      setSavedViewsByProject({
        ...savedViewsByProject,
        [project]: saveView(savedViews, { name, query: query.toString() }),
      });
      setParams(query);
    },
    [params, project, savedViews, savedViewsByProject, setParams, setSavedViewsByProject, table]
  );
  const deleteCurrentView = useCallback(() => {
    if (activeSaved === undefined) return;
    setSavedViewsByProject({
      ...savedViewsByProject,
      [project]: deleteView(savedViews, activeSaved.name),
    });
  }, [activeSaved, project, savedViews, savedViewsByProject, setSavedViewsByProject]);
  // The keys the table lists under the filters and the search it is showing, in row order. A key
  // it no longer lists drops out of the selection - another filter, another project - so a row
  // that is listed again comes back unselected; this is the rule `hooks/useMarkedKeys.ts` holds
  // the Inbox's marks to, written against the table's own `rowSelection` rather than a second
  // selection beside it. Joined into a string, so the pruning runs when the keys change rather
  // than on every render that rebuilds the rows, and `undefined` while the listing is still out,
  // which prunes nothing.
  const listedKeys =
    issues.data === undefined || (done !== null && !doneLoaded)
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
      (done === null || doneLoaded) &&
      issues.data &&
      pagination.pageIndex >= Math.max(pageCount, 1)
    )
      table.setPageIndex(Math.max(pageCount - 1, 0));
  }, [done, doneLoaded, issues.data, pagination.pageIndex, pageCount, table]);

  if (issues.isPending || userState.isPending || (activeView.id === "mine" && viewer.isPending))
    return <LoadingSkeleton label="Loading issues" />;
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
    <section
      aria-label="Project issues"
      className="space-y-3 sm:pb-24"
      data-issues-tab
      data-view={activeSaved === undefined ? activeView.id : `saved:${activeSaved.name}`}
      data-view-changed={viewChanged || undefined}
    >
      <div className="space-y-3" ref={attachAbove}>
        {listingError ? (
          <p className={dangerText} role="alert">
            Could not load {done !== null ? "closed " : ""}issues.{" "}
            <button
              type="button"
              aria-label={done !== null ? "Retry loading closed issues" : "Retry loading issues"}
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
        {doneRead?.isPending ? (
          <p aria-live="polite" className={textMutedOnCanvas}>
            Loading closed issues…
          </p>
        ) : null}
        {filters.some(isLegionFilter) && !boardReady ? (
          <p data-legion-pending-chip className={textMutedOnCanvas}>
            Waiting for Legion data
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
        <IssueViewTabs counts={counts} hasLegion={hasLegion} project={project} saved={savedViews} />
        {doneDefaulted || excludedTrees.size > 0 ? (
          <div className={`flex flex-wrap gap-2 text-sm ${textSecondaryOnCanvas}`}>
            {doneDefaulted ? (
              <span
                className={`inline-flex min-h-9 items-center rounded-full border px-3 ${borderDefault} ${surfaceMutedBg}`}
                data-closed-chip
              >
                Closed: last 28 days
              </span>
            ) : null}
            {[...excludedTrees].map((key) => (
              <span
                className={`inline-flex min-h-9 items-center gap-2 rounded-full border px-3 ${borderDefault} ${surfaceMutedBg}`}
                data-exclude-tree={key}
                key={key}
              >
                Without {key}&apos;s tree
                <button
                  aria-label={`Include ${key}'s tree`}
                  className={`-mr-1 rounded-full px-1.5 ${surfaceMutedHoverBg}`}
                  onClick={() =>
                    updateParams((next) => {
                      const remaining = next
                        .getAll("exclude_tree")
                        .filter((candidate) => candidate !== key);
                      next.delete("exclude_tree");
                      remaining.forEach((candidate) => {
                        next.append("exclude_tree", candidate);
                      });
                    })
                  }
                  type="button"
                >
                  ×
                </button>
              </span>
            ))}
          </div>
        ) : null}
        {barColumn === null ? null : (
          <IssueTableStatus
            board={legion.data}
            column={barColumn}
            hasLegion={hasLegion}
            rows={model.listedLeaves}
            table={table}
            viewValue={barViewValue}
          />
        )}
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
          onDeleteView={activeSaved === undefined ? undefined : deleteCurrentView}
          onResetView={viewChanged ? resetView : undefined}
          onSaveView={saveCurrentView}
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
