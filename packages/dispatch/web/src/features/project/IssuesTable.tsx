import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  type ColumnFiltersState,
  type ColumnOrderState,
  type ColumnSizingState,
  type ExpandedState,
  type FilterFn,
  flexRender,
  type GroupingState,
  getCoreRowModel,
  getExpandedRowModel,
  getFilteredRowModel,
  getGroupedRowModel,
  getPaginationRowModel,
  getSortedRowModel,
  type PaginationState,
  type Row,
  type RowSelectionState,
  type SortingState,
  useReactTable,
  type VisibilityState,
} from "@tanstack/react-table";
import { useVirtualizer } from "@tanstack/react-virtual";
import { type ReactNode, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import {
  issueQuery,
  legionBoardQuery,
  legionSettingsQuery,
  projectIssuesQuery,
  userStateQuery,
} from "../../api/queries";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { capSearchQuery } from "../../lib/query-param";
import {
  borderDefault,
  dangerText,
  focusVisibleRing,
  selectedCardBg,
  surfaceBg,
  tableResizeActive,
  tableResizeHover,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { claimIsTakeable } from "../issue/ClaimChip";
import { LegionSettingsPanel } from "../legion/LegionSettingsPanel";
import { useAgentText } from "../refs/AgentName";
import { useAgentRegistry } from "../refs/agent-registry";
import { useKeymap } from "../shell/keymap";
import { closestMatching } from "../shell/roving";
import { ViewportAnchor } from "../shell/ViewportAnchor";
import { LandedWithoutAnswer } from "../writes/LandedWithoutAnswer";
import { projectIssuesQueryKey, useIssueFilters } from "./issue-filters";
import { useIssueTableActions } from "./issues-table-actions";
import {
  COLUMN_MIN,
  COLUMN_SIZES,
  COMPACT_COLUMN_MIN,
  PHONE_PINNED_SHARE,
  PINNED,
  PINNED_SHARE,
  useIssueTableColumns,
} from "./issues-table-columns";
import {
  ColumnFilter,
  GroupRow,
  IssueTablePager,
  IssueTableSelection,
  IssueTableStatus,
  IssueTableToolbar,
} from "./issues-table-controls";
import {
  buildItems,
  defaultView,
  type EpicAncestor,
  facetCounts,
  type Item,
  isEpic,
  matchesSearch,
  readUrl,
  type View,
  writeUrl,
} from "./issues-table-model";
import { tableStorageKey } from "./issues-table-recovery";
import { issueIsUnread } from "./UnreadDot";

const EMPTY_FACETS: ReadonlyMap<string, number> = new Map();

const fuzzy: FilterFn<Item> = (row, _id, query: string) => matchesSearch(row.original, query);

const ACTION_WIDTH = 104;

interface Layout {
  sizing: ColumnSizingState;
  order: ColumnOrderState;
  visibility: VisibilityState;
}
function loadLayout(project: string): Layout {
  const empty = {
    sizing: {},
    order: [],
    visibility: { updated: false, slot: false, roles: false, ci: false },
  };
  try {
    const saved = JSON.parse(localStorage.getItem(tableStorageKey(project)) ?? "{}");
    const sizing = Object.fromEntries(
      Object.entries(saved.sizing ?? {}).filter(
        ([id, size]) =>
          id in COLUMN_SIZES &&
          typeof size === "number" &&
          Number.isFinite(size) &&
          size >= 36 &&
          size <= 2000
      )
    );
    const order = Array.isArray(saved.order)
      ? [
          ...new Set<string>(
            saved.order.filter((id: unknown) => typeof id === "string" && id in COLUMN_SIZES)
          ),
        ]
      : [];
    const visibility = Object.fromEntries(
      Object.entries(saved.visibility ?? {}).filter(
        ([id, visible]) =>
          id in COLUMN_SIZES && !PINNED.includes(id) && typeof visible === "boolean"
      )
    );
    return {
      sizing: sizing as ColumnSizingState,
      order,
      visibility: { ...empty.visibility, ...visibility },
    };
  } catch {
    return empty;
  }
}

export function IssuesTable({ project }: { project: string }): ReactNode {
  const queryClient = useQueryClient();
  const text = useAgentText();
  const [params, setParams] = useSearchParams();
  const legionSettings = useQuery(legionSettingsQuery());
  const hasLegion = (legionSettings.data ?? []).some((setting) => setting.project === project);
  const view = useMemo(() => {
    const parsed = readUrl(params);
    if (hasLegion) return parsed;
    const available = (id: string) => !["phase", "slot", "roles", "ci"].includes(id);
    const sorting = parsed.sorting.filter(({ id }) => available(id));
    return {
      ...parsed,
      sorting: parsed.sorting.length && !sorting.length ? defaultView.sorting : sorting,
      grouping: parsed.grouping.filter(available),
      filters: parsed.filters.filter(({ id }) => available(id)),
    };
  }, [params, hasLegion]);
  const { sorting, grouping, filters, search } = view;
  const pagination = { pageIndex: view.page, pageSize: view.pageSize };
  const updateView = (update: (current: View) => View) =>
    setParams((current) => writeUrl(current, update(readUrl(current))), { replace: true });
  const setSorting = (update: SortingState | ((current: SortingState) => SortingState)) =>
    updateView((current) => ({
      ...current,
      sorting: typeof update === "function" ? update(current.sorting) : update,
      page: 0,
    }));
  const setGrouping = (update: GroupingState | ((current: GroupingState) => GroupingState)) =>
    updateView((current) => ({
      ...current,
      grouping: typeof update === "function" ? update(current.grouping) : update,
      page: 0,
    }));
  const setFilters = (
    update: ColumnFiltersState | ((current: ColumnFiltersState) => ColumnFiltersState)
  ) =>
    updateView((current) => ({
      ...current,
      filters: typeof update === "function" ? update(current.filters) : update,
      page: 0,
    }));
  const setSearch = (search: string) =>
    updateView((current) => ({ ...current, search: capSearchQuery(search), page: 0 }));
  const setPagination = (
    update: PaginationState | ((current: PaginationState) => PaginationState)
  ) =>
    updateView((current) => {
      const next =
        typeof update === "function"
          ? update({ pageIndex: current.page, pageSize: current.pageSize })
          : update;
      return { ...current, page: next.pageIndex, pageSize: next.pageSize };
    });
  const layout = useMemo(() => loadLayout(project), [project]);
  const [visibility, setVisibility] = useState<VisibilityState>(layout.visibility);
  const flags = useIssueFilters();
  const registry = useAgentRegistry(flags.unclaimed);
  const userState = useQuery(userStateQuery());
  const legion = useQuery({ ...legionBoardQuery({ kind: "legion", project }), enabled: hasLegion });
  const [sizing, setSizing] = useState<ColumnSizingState>(layout.sizing);
  const [boxWidth, setBoxWidth] = useState(1000);
  const responsiveLayout = useMemo(() => {
    const widths = Object.fromEntries(
      Object.entries(COLUMN_SIZES).map(([id, fallback]) => [
        id,
        Math.max(COLUMN_MIN[id] ?? 116, sizing[id] ?? fallback),
      ])
    );
    // On a phone the key and title are the row, so they take most of the box, leaving an edge of
    // the next column to show the table scrolls sideways; wider boxes keep room for the rest.
    const budget = boxWidth * (boxWidth < 640 ? PHONE_PINNED_SHARE : PINNED_SHARE);
    if (boxWidth < 640) {
      widths.key = COMPACT_COLUMN_MIN.key;
      widths.title = Math.max(
        COMPACT_COLUMN_MIN.title,
        Math.min(widths.title ?? 380, budget - (widths.select ?? 44) - widths.key)
      );
      return { sizing: widths, pinned: ["select", "key", "title"] };
    }
    const pinned = [...PINNED];
    const width = () => pinned.reduce((sum, id) => sum + (widths[id] ?? 0), 0);
    widths.title = Math.max(
      COLUMN_MIN.title ?? 116,
      (widths.title ?? 380) - Math.max(0, width() - budget)
    );
    // At readable minimums, unpin from the right rather than cover the viewport.
    while (pinned.length > 1 && width() > budget) pinned.pop();
    return { sizing: widths, pinned };
  }, [boxWidth, sizing]);
  const [order, setOrder] = useState<ColumnOrderState>(layout.order);
  const [expanded, setExpanded] = useState<ExpandedState>(true);
  const [selection, setSelection] = useState<RowSelectionState>({});
  const [openFilter, setOpenFilter] = useState<string>();
  const [dragColumn, setDragColumn] = useState<string>();
  useEffect(() => {
    try {
      const saved = JSON.parse(localStorage.getItem(tableStorageKey(project)) ?? "{}");
      localStorage.setItem(
        tableStorageKey(project),
        JSON.stringify({ ...saved, sizing, order, visibility })
      );
    } catch {
      /* The URL still works when the browser refuses local preferences. */
    }
  }, [project, sizing, order, visibility]);

  const statusPicked = (filters.find((filter) => filter.id === "status")?.value ?? []) as string[];
  const wantsDone = statusPicked.length === 0 || statusPicked.includes("done");
  const openIssues = useQuery(projectIssuesQuery(project, [], true));
  const everyIssue = useQuery({ ...projectIssuesQuery(project, []), enabled: wantsDone });
  const issues = wantsDone && everyIssue.data !== undefined ? everyIssue : openIssues;
  const doneLoaded = issues === everyIssue && everyIssue.data !== undefined;
  const listingError =
    wantsDone && everyIssue.isError ? everyIssue : issues.isError ? issues : undefined;
  const all = issues.data ?? [];
  const missingParents = useMemo(() => {
    const keys = new Set(all.map((issue) => issue.key));
    return [
      ...new Set(
        all.flatMap((issue) => (issue.parent && !keys.has(issue.parent) ? [issue.parent] : []))
      ),
    ];
  }, [all]);
  const ancestry = useQuery({
    queryKey: [...projectIssuesQueryKey(project, []), "ancestors", ...missingParents],
    enabled: missingParents.length > 0,
    retry: false,
    queryFn: async () => {
      const known = new Map<string, EpicAncestor>(all.map((issue) => [issue.key, issue]));
      const ancestors: EpicAncestor[] = [];
      for (;;) {
        const missing = [
          ...new Set(
            [...known.values()].flatMap((issue) =>
              issue.parent && !known.has(issue.parent) ? [issue.parent] : []
            )
          ),
        ];
        if (missing.length === 0) return ancestors;
        for (const parent of await Promise.all(
          missing.map((key) => queryClient.fetchQuery(issueQuery(key)))
        )) {
          known.set(parent.key, parent);
          ancestors.push(parent);
        }
      }
    },
  });
  const epics = useMemo(() => all.filter(isEpic), [all]);
  const allItems = useMemo(
    () => buildItems(all, text.label, ancestry.data, hasLegion ? (legion.data ?? null) : undefined),
    [all, text, ancestry.data, hasLegion, legion.data]
  );
  const items = useMemo(
    () =>
      allItems.filter(
        ({ issue }) =>
          (!flags.needsYou || issue.open_asks > 0) &&
          (!flags.unread ||
            issueIsUnread(issue, userState.data?.[issue.key]?.last_read_seq ?? 0)) &&
          (!flags.unclaimed || issue.claim === null || claimIsTakeable(issue.claim, registry))
      ),
    [allItems, flags.needsYou, flags.unread, flags.unclaimed, registry, userState.data]
  );

  const columns = useIssueTableColumns(userState.data, boxWidth < 640, hasLegion, legion.data);

  const table = useReactTable({
    data: items,
    columns,
    state: {
      sorting:
        grouping[0] === "phase" && !sorting.some(({ id }) => id === "phase")
          ? [{ id: "phase", desc: false }, ...sorting]
          : sorting,
      grouping,
      columnFilters: filters,
      columnVisibility: visibility,
      pagination,
      globalFilter: search,
      expanded,
      rowSelection: selection,
      columnSizing: responsiveLayout.sizing,
      columnOrder: order,
      columnPinning: { left: responsiveLayout.pinned },
    },
    getRowId: (item) => item.issue.key,
    onColumnSizingChange: setSizing,
    onColumnOrderChange: setOrder,
    columnResizeMode: "onChange",
    onSortingChange: setSorting,
    onGroupingChange: setGrouping,
    onColumnFiltersChange: setFilters,
    onColumnVisibilityChange: setVisibility,
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
    getSortedRowModel: getSortedRowModel(),
    getGroupedRowModel: getGroupedRowModel(),
    getExpandedRowModel: getExpandedRowModel(),
    getPaginationRowModel: getPaginationRowModel(),
  });

  const {
    busy,
    notice,
    moved,
    duplicateEpic,
    setDuplicateEpic,
    unansweredEpic,
    setUnansweredEpic,
    moveTo,
    startNext,
    newEpic,
    renameEpic,
    mergeEpic,
    interrupted,
    finish,
    dismiss,
  } = useIssueTableActions({
    project,
    hasLegion,
    issues: all,
    items: allItems,
    clearSelection: () => setSelection({}),
  });

  const scrollRef = useRef<HTMLDivElement>(null);
  const pagerRef = useRef<HTMLDivElement>(null);
  const [boxHeight, setBoxHeight] = useState(400);
  useLayoutEffect(() => {
    const place = () => {
      const box = scrollRef.current;
      if (!box) return;
      const bounds = box.getBoundingClientRect();
      const header = box.querySelector("thead")?.getBoundingClientRect().height ?? 61;
      const firstRows = [...box.querySelectorAll<HTMLElement>("[data-issue-row]")].slice(0, 3);
      const rowsHeight = firstRows.reduce(
        (height, row) => height + row.getBoundingClientRect().height,
        0
      );
      const available =
        window.innerHeight -
        (bounds.top + window.scrollY) -
        (pagerRef.current?.getBoundingClientRect().height ?? 80) -
        32;
      setBoxHeight(Math.max(400, header + rowsHeight + 2, available));
      setBoxWidth(bounds.width);
    };
    place();
    window.addEventListener("resize", place);
    return () => window.removeEventListener("resize", place);
  });
  const pageRows = table.getRowModel().rows;
  const continued =
    pageRows[0] !== undefined && !pageRows[0].getIsGrouped()
      ? pageRows[0].getParentRow()
      : undefined;
  const displayRows: { row: Row<Item>; continued?: boolean }[] = [
    ...(continued === undefined ? [] : [{ row: continued, continued: true }]),
    ...pageRows.map((row) => ({ row })),
  ];
  const virtualizer = useVirtualizer({
    count: displayRows.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: (index) => (displayRows[index]?.row.getIsGrouped() ? 48 : 64),
    getItemKey: (index) =>
      `${displayRows[index]?.continued ? "continued-" : ""}${displayRows[index]?.row.id}`,
    initialRect: { width: 1000, height: 600 },
    overscan: 12,
  });

  const pendingFocus = useRef<string | null>(null);
  const focusedRow = () => closestMatching(document.activeElement, "[data-issue-row]");
  const moveFocus = (direction: 1 | -1) => {
    const leaves = displayRows.filter(({ row }) => !row.getIsGrouped());
    const current = leaves.findIndex(
      ({ row }) => row.id === focusedRow()?.getAttribute("data-issue-row")
    );
    const target =
      leaves[
        current < 0
          ? direction === 1
            ? 0
            : leaves.length - 1
          : Math.max(0, Math.min(leaves.length - 1, current + direction))
      ];
    if (!target) return;
    pendingFocus.current = target.row.id;
    const element = scrollRef.current?.querySelector<HTMLElement>(
      `[data-issue-row="${CSS.escape(target.row.id)}"]`
    );
    if (element) {
      element.focus();
      pendingFocus.current = null;
    } else virtualizer.scrollToIndex(displayRows.indexOf(target), { align: "auto" });
  };
  useLayoutEffect(() => {
    if (!pendingFocus.current) return;
    const element = scrollRef.current?.querySelector<HTMLElement>(
      `[data-issue-row="${CSS.escape(pendingFocus.current)}"]`
    );
    if (element) {
      element.focus({ preventScroll: true });
      pendingFocus.current = null;
    }
  });
  const openFocusedRow = () =>
    focusedRow()?.querySelector<HTMLElement>("[data-issue-link]")?.click();
  useKeymap("project", [
    {
      id: "list-next",
      keys: "j",
      label: "Next issue",
      palette: false,
      run: () => moveFocus(1),
      when: () => displayRows.some(({ row }) => !row.getIsGrouped()),
    },
    {
      id: "list-previous",
      keys: "k",
      label: "Previous issue",
      palette: false,
      run: () => moveFocus(-1),
      when: () => displayRows.some(({ row }) => !row.getIsGrouped()),
    },
    {
      id: "list-open",
      keys: "o",
      label: "Open issue",
      run: openFocusedRow,
      when: () => focusedRow() !== null,
    },
    {
      id: "list-open-enter",
      keys: "Enter",
      label: "Open the focused issue",
      palette: false,
      run: openFocusedRow,
      when: () => document.activeElement?.matches("[data-issue-row]") === true,
    },
  ]);
  const controlSignature = params.toString();
  const previousControls = useRef(controlSignature);
  useLayoutEffect(() => {
    if (previousControls.current !== controlSignature) scrollRef.current?.scrollTo({ top: 0 });
    previousControls.current = controlSignature;
  }, [controlSignature]);
  useEffect(() => {
    if (
      (!wantsDone || everyIssue.data !== undefined) &&
      issues.data &&
      pagination.pageIndex >= Math.max(table.getPageCount(), 1)
    )
      table.setPageIndex(Math.max(table.getPageCount() - 1, 0));
  });
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

  return (
    <section aria-label="Project issues" className="space-y-3 pb-24">
      <div className="space-y-3">
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
        {missingParents.length > 0 && ancestry.isError ? (
          <p className={dangerText} role="alert">
            Could not load some ancestors; affected rows show Unknown epic.{" "}
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
            Could not refresh Legion data.{" "}
            <button type="button" onClick={() => void legion.refetch()}>
              Retry Legion data
            </button>
          </p>
        ) : null}
        {hasLegion && legion.isPending ? (
          <p className={textMutedOnCanvas}>Loading Legion data…</p>
        ) : null}
        <IssueTableStatus
          table={table}
          items={allItems}
          epics={epics}
          doneLoaded={doneLoaded}
          hasLegion={hasLegion}
          board={legion.data}
        />
        {hasLegion && legion.data ? (
          <details>
            <summary className={`cursor-pointer text-sm ${textSecondaryOnCanvas}`}>
              Legion settings
            </summary>
            <LegionSettingsPanel board={legion.data} />
          </details>
        ) : null}

        <IssueTableToolbar table={table} />

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
              {interrupted.kind === "newEpic"
                ? `New epic${interrupted.target ? ` ${interrupted.target}` : ""}: ${interrupted.keys.join(", ")} are not yet adopted. Finish checks which open issues still need to move.`
                : interrupted.kind === "startNext"
                  ? `Start next: ${interrupted.keys.join(", ")} may still need to move up, in that order.`
                  : `Merge ${interrupted.source} into ${interrupted.target}: remaining open issues, the merge reason and closing the source still need to be checked.`}
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
            The selected issues have not been moved. Find the epic in the project before moving
            them.{" "}
            <button type="button" onClick={() => setUnansweredEpic(undefined)}>
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
        ref={scrollRef}
        data-testid="issues-table-scroll"
        style={{
          overflowAnchor: "none",
          height: boxHeight,
          scrollPaddingTop: 64,
          scrollPaddingLeft: responsiveLayout.pinned.reduce(
            (width, id) => width + (responsiveLayout.sizing[id] ?? 0),
            0
          ),
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
                  {group.headers.map((header) => {
                    const pinned = header.column.getIsPinned() === "left";
                    return (
                      <th
                        aria-sort={
                          header.column.getIsSorted() === "asc"
                            ? "ascending"
                            : header.column.getIsSorted() === "desc"
                              ? "descending"
                              : undefined
                        }
                        className={`sticky top-0 border-b px-1 py-2 pr-2 text-left align-middle text-xs font-semibold md:px-2 ${borderDefault} ${surfaceBg} ${textSecondaryOnCanvas} ${pinned ? "z-30" : "z-20"} ${pinned && header.column.getIsLastColumn("left") ? "border-r" : ""}`}
                        key={header.id}
                        onDragOver={(event) => {
                          if (dragColumn !== undefined && !pinned) event.preventDefault();
                        }}
                        onDrop={() => {
                          if (
                            dragColumn === undefined ||
                            pinned ||
                            dragColumn === header.column.id
                          ) {
                            return;
                          }
                          const current = table.getAllLeafColumns().map((column) => column.id);
                          const next = current.filter((id) => id !== dragColumn);
                          next.splice(next.indexOf(header.column.id), 0, dragColumn);
                          setOrder(next);
                          setDragColumn(undefined);
                        }}
                        style={{
                          width: header.getSize(),
                          left: pinned ? header.column.getStart("left") : undefined,
                        }}
                      >
                        <span
                          className={`flex justify-between ${boxWidth < 640 && header.column.id === "title" ? "flex-col items-center" : "items-center gap-0 md:gap-1"}`}
                        >
                          {header.column.getCanSort() ? (
                            <button
                              className="flex min-w-0 items-center gap-1 truncate whitespace-nowrap"
                              draggable={!pinned}
                              onClick={header.column.getToggleSortingHandler()}
                              onDragEnd={() => setDragColumn(undefined)}
                              onDragStart={() => setDragColumn(header.column.id)}
                              title={
                                pinned
                                  ? "Sort (shift-click to add a second sort)"
                                  : "Sort (shift-click to add a second sort); drag to move the column"
                              }
                              type="button"
                            >
                              {flexRender(header.column.columnDef.header, header.getContext())}
                              <span className={textMutedOnCanvas}>
                                {{ asc: "▲", desc: "▼" }[header.column.getIsSorted() as string] ??
                                  ""}
                              </span>
                            </button>
                          ) : (
                            flexRender(header.column.columnDef.header, header.getContext())
                          )}
                          {header.column.getCanFilter() ? (
                            <ColumnFilter
                              column={header.column}
                              doneLoaded={doneLoaded}
                              counts={header.column.id === openFilter ? openFacets : EMPTY_FACETS}
                              onOpenChange={(open) =>
                                setOpenFilter(open ? header.column.id : undefined)
                              }
                              open={openFilter === header.column.id}
                            />
                          ) : null}
                        </span>
                        {header.column.getCanResize() ? (
                          <span
                            aria-hidden
                            className={`absolute top-0 right-0 h-full w-1.5 cursor-col-resize touch-none select-none ${header.column.getIsResizing() ? tableResizeActive : tableResizeHover}`}
                            onDoubleClick={() => header.column.resetSize()}
                            onMouseDown={header.getResizeHandler()}
                            onTouchStart={header.getResizeHandler()}
                          />
                        ) : null}
                      </th>
                    );
                  })}
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
                  <tr
                    className={`group align-top outline-none focus-visible:ring-2 ${focusVisibleRing} ${moved.has(row.original.issue.key) ? selectedCardBg : ""}`}
                    data-index={virtual.index}
                    aria-label={`${row.original.issue.key} ${row.original.issue.title}`}
                    data-issue-row={row.original.issue.key}
                    data-issue-group={row.getParentRow()?.id ?? "ungrouped"}
                    data-moved={moved.has(row.original.issue.key) || undefined}
                    tabIndex={-1}
                    key={row.id}
                    ref={virtualizer.measureElement}
                  >
                    {row.getVisibleCells().map((cell) => {
                      const pinned = cell.column.getIsPinned() === "left";
                      return (
                        <td
                          className={`overflow-hidden border-b px-2 py-2 ${borderDefault} ${pinned ? `sticky z-10 ${moved.has(row.original.issue.key) ? selectedCardBg : surfaceBg}` : ""} ${pinned && cell.column.getIsLastColumn("left") ? "border-r" : ""} ${row.depth > 0 && cell.column.id === "select" ? "pl-6" : ""}`}
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
                    <td className={`border-b px-2 py-2 text-right ${borderDefault}`}>
                      {row.original.queue !== undefined && row.original.queue > 1 ? (
                        <button
                          className={`rounded-md border px-2 py-0.5 text-xs whitespace-nowrap opacity-0 group-hover:opacity-100 focus:opacity-100 ${borderDefault} ${surfaceBg}`}
                          disabled={busy}
                          onClick={() => startNext([row.original.issue.key])}
                          type="button"
                        >
                          Start next
                        </button>
                      ) : null}
                    </td>
                  </tr>
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
    </section>
  );
}
