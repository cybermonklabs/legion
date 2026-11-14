import { useQuery } from "@tanstack/react-query";
import type { Column, FilterFn, RowSelectionState, VisibilityState } from "@tanstack/react-table";
import { type ReactNode, useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  legionBoardQuery,
  legionSettingsQuery,
  projectDoneIssuesQuery,
  userStateQuery,
  whoAmIQuery,
} from "../../api/queries";
import { Chip } from "../../components/Chip";
import { ColumnFilter } from "../../components/ColumnFilter";
import { ColumnsMenu } from "../../components/ColumnsMenu";
import { DataTable, type DataTableNames, useDataTable } from "../../components/DataTable";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import type { PagerOptions } from "../../components/Pager";
import { QueryError } from "../../components/QueryError";
import { useTableLayout } from "../../components/table-layout";
import { readStore, writeStore } from "../../components/table-storage";
import { useAddressParams, useAddressUpdate } from "../../lib/page-address";
import {
  borderDefault,
  dangerText,
  focusVisibleRing,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { useAgentText } from "../refs/AgentName";
import { useAgentRegistry } from "../refs/agent-registry";
import { setSelectedIssues } from "../search/bar-scope";
import { useKeymap } from "../shell/keymap";
import { useUserPreference } from "../shell/userPreference";
import { LandedWithoutAnswer } from "../writes/LandedWithoutAnswer";
import { ConfirmDialog } from "./ConfirmDialog";
import { EpicNameDialog } from "./EpicNameDialog";
import { IssueBoard } from "./IssueBoard";
import { type ActiveView, IssueViewTabs, LayoutSwitch, type ViewLayout } from "./IssueViewTabs";
import { useIssueFilters } from "./issue-filters";
import {
  PhaseHoursContext,
  PhaseHoursModeToggle,
  phaseHours,
  usePhaseHoursMode,
} from "./issues-phase-hours";
import { useIssueTableActions } from "./issues-table-actions";
import { ViewBar } from "./issues-table-bar";
import { useIssueTableColumns } from "./issues-table-columns";
import { type DoneRead, useProjectIssueItems } from "./issues-table-data";
import {
  ALL_ROWS,
  buildItems,
  COLUMN_MIN,
  COLUMNS,
  type ColumnId,
  compareGroups,
  facetCounts,
  groupValue,
  ISSUE_GROUP_ATTRIBUTE,
  ISSUE_LINK_ATTRIBUTE,
  ISSUE_ROW_ATTRIBUTE,
  type Item,
  isBoardColumn,
  isEpic,
  matchesFilters,
  matchesSearch,
  PAGE_SIZES,
  readUrl,
  tableStorageKey,
  UNCLAIMED_FACET,
  viewLayoutColumns,
} from "./issues-table-model";
import { type ExtraChip, IssueTableSelection, IssueTableToolbar } from "./issues-table-page";
import { GroupRow, IssueRow } from "./issues-table-rows";
import { useTableView } from "./issues-table-view";
import {
  type BuiltInViewId,
  isViewChanged,
  readSavedViews,
  resetView,
  type StoredViews,
  viewFor,
  viewQueryKey,
  viewsFor,
  windowRead,
} from "./issues-views";
import { RangeFilter } from "./RangeFilter";
import { issueIsUnread } from "./UnreadDot";

const EMPTY_FACETS: ReadonlyMap<string, number> = new Map();
/** Status offers Done before the closed issues it would count are read. */
const DONE_UNLOADED: ReadonlyMap<string, readonly string[]> = new Map([["status", ["done"]]]);
const NONE: readonly string[] = [];

const fuzzy: FilterFn<Item> = (row, _id, query: string) => matchesSearch(row.original, query);

const ACTION_WIDTH = 104;

/** The checkbox a row draws for its selection (`issues-table-columns.tsx`'s select column), the
 *  one checkbox every row has. */
const ROW_CHECKBOX = 'input[type="checkbox"]';

/** The List's own words, which the table core reads and never invents: the attributes its rows
 *  and groups carry, the link opening a row follows, what its filters count, and the keys that
 *  walk it, under the ids and labels the palette and the shortcut help have always shown. */
const ISSUE_TABLE_NAMES: DataTableNames = {
  filterCountNoun: { one: "issue", many: "issues" },
  groupAttribute: ISSUE_GROUP_ATTRIBUTE,
  keymap: {
    next: { id: "list-next", keys: "j", label: "Next issue" },
    open: { id: "list-open", keys: "o", label: "Open issue" },
    openOnEnter: { id: "list-open-enter", keys: "Enter", label: "Open the focused issue" },
    previous: { id: "list-previous", keys: "k", label: "Previous issue" },
    scope: "project",
    select: { id: "list-select", keys: "x", label: "Select or deselect the focused issue" },
  },
  linkAttribute: ISSUE_LINK_ATTRIBUTE,
  rowAttribute: ISSUE_ROW_ATTRIBUTE,
};

/** The line under the List: how many issues of how many, in how many groups, over how many
 *  rows. */
const ISSUE_TABLE_PAGER: PagerOptions = {
  allRows: ALL_ROWS,
  nouns: {
    group: { one: "group", many: "groups" },
    item: { one: "issue", many: "issues" },
    row: { one: "row", many: "rows" },
  },
  pageSizes: PAGE_SIZES,
  testId: "issues-table-pager",
};

/** Where one view's columns, widths and layout are kept in this browser, per project. All open
 *  keeps the key the table has always used, so today's stored layout becomes All open's. */
const viewStorageKey = (project: string, view: BuiltInViewId) =>
  view === "open" ? tableStorageKey(project) : `${tableStorageKey(project)}.view.${view}`;

/** The column a bar counts by, and the parameter its press narrows. */
const BAR_COLUMN = {
  stage: "stage",
  waiting_reason: "waiting_reason",
  slot_use: "slot_use",
} as const;

/** Whether an address lists closed issues: its Status filter picks Done, its Stage filter picks
 *  Done, or it lifts the Status filter (`nofilter=1`). A view with no Status filter of its own
 *  (Waiting line, In flight, Slots) lists the open issues its Legion filters select. */
function listsClosed(params: URLSearchParams, hasLegion: boolean): boolean {
  const { filters } = readUrl(params, hasLegion);
  const picks = (id: string) => {
    const value = filters.find((filter) => filter.id === id)?.value;
    return Array.isArray(value) ? value.map(String) : undefined;
  };
  const status = picks("status");
  if (status !== undefined) return status.includes("done");
  return params.get("nofilter") === "1" || (picks("stage")?.includes("done") ?? false);
}

/** Which done issues an address reads: none, the window its Closed filter names, or every done
 *  issue where it names none. */
function doneRead(params: URLSearchParams, hasLegion: boolean, nowMs: number): DoneRead {
  if (!listsClosed(params, hasLegion)) return undefined;
  const closed = readUrl(params, hasLegion).filters.find((filter) => filter.id === "closed");
  return (typeof closed?.value === "string" ? windowRead(closed.value, nowMs) : undefined) ?? {};
}

/** The Issues tab: the project's one list of issues, in views, in the table or the Board. */
export function IssuesTable({ project }: { project: string }): ReactNode {
  const settings = useQuery(legionSettingsQuery());
  if (settings.isPending) return <LoadingSkeleton label="Loading issues" />;
  const hasLegion = (settings.data ?? []).some((setting) => setting.project === project);
  return <ViewsTable hasLegion={hasLegion} key={project} project={project} />;
}

function ViewsTable({ hasLegion, project }: { hasLegion: boolean; project: string }): ReactNode {
  const params = useAddressParams();
  const update = useAddressUpdate();
  const { view, setFilters, setGrouping, setPagination, setSearch, setSorting } =
    useTableView(hasLegion);
  const { sorting, grouping, filters, search } = view;
  const pagination = { pageIndex: view.page, pageSize: view.pageSize };
  const views = viewsFor(hasLegion);
  const builtIn = viewFor(params.get("view"), hasLegion);
  const viewer = useQuery(whoAmIQuery()).data?.login;
  const board = useQuery({ ...legionBoardQuery({ kind: "legion", project }), enabled: hasLegion });
  const flags = useIssueFilters();
  const userState = useQuery(userStateQuery());
  const [selection, setSelection] = useState<RowSelectionState>({});
  const lastMarked = useRef<string | null>(null);
  const searchRef = useRef<HTMLInputElement>(null);
  const recordMark = useCallback((key: string) => {
    lastMarked.current = key;
  }, []);
  const [openFilter, setOpenFilter] = useState<string>();
  const [phaseMode, setPhaseMode] = usePhaseHoursMode();

  // Saved views: this browser's, for the signed-in login, per project.
  const [stored, setStored] = useUserPreference<StoredViews>(
    "project.saved-views",
    readSavedViews,
    (value) => JSON.stringify(value)
  );
  const saved = stored.projects[project] ?? [];
  const [chosenSaved, setChosenSaved] = useState<string>();
  const queryKey = viewQueryKey(params);
  const matchedSaved = saved.find(
    (entry) => viewQueryKey(new URLSearchParams(entry.query)) === queryKey
  )?.name;
  const savedName =
    chosenSaved !== undefined && saved.some((entry) => entry.name === chosenSaved)
      ? chosenSaved
      : matchedSaved;
  const savedView = saved.find((entry) => entry.name === savedName);
  const active: ActiveView =
    savedView === undefined
      ? { kind: "built-in", id: builtIn.id }
      : { kind: "saved", name: savedView.name };
  const changed =
    savedView === undefined
      ? isViewChanged(params)
      : viewQueryKey(new URLSearchParams(savedView.query)) !== queryKey;

  // The layout: the address's for this load, else the one kept for the view, else today's
  // `project.issue-view` choice (All open's, its `legion` value In flight's Board).
  const storeKey = viewStorageKey(project, builtIn.id);
  const [legacyView] = useUserPreference(
    "project.issue-view",
    (value) => value ?? "list",
    (value) => value
  );
  const [keptLayouts, setKeptLayouts] = useState<Record<string, ViewLayout>>({});
  const keptLayout =
    keptLayouts[storeKey] ??
    (readStore(storeKey).layout === "board"
      ? "board"
      : readStore(storeKey).layout === "table"
        ? "table"
        : undefined);
  const urlLayout = params.get("layout");
  const layoutChoice: ViewLayout =
    urlLayout === "board" || urlLayout === "table"
      ? urlLayout
      : (keptLayout ??
        ((builtIn.id === "open" && legacyView === "board") ||
        (builtIn.id === "in-flight" && legacyView === "legion")
          ? "board"
          : "table"));
  const setLayout = (next: ViewLayout) => {
    if (params.has("layout"))
      update((current) => {
        current.set("layout", next);
        return current;
      });
    else {
      writeStore(storeKey, { layout: next });
      setKeptLayouts((current) => ({ ...current, [storeKey]: next }));
    }
  };
  useKeymap("project", [
    {
      id: "toggle-view",
      keys: "v",
      label: "Switch the table and the Board",
      run: () => setLayout(layoutChoice === "board" ? "table" : "board"),
    },
  ]);
  const [showIcebox, setShowIcebox] = useUserPreference(
    "project.board-edges",
    (value) => value === "shown",
    (shown) => (shown ? "shown" : "hidden")
  );
  const [lanes, setLanes] = useUserPreference(
    "project.board-lanes",
    (value) => value === "shown",
    (shown) => (shown ? "shown" : "hidden")
  );

  // The columns: the view's own, the address's `cols=` for this load, the board's only where
  // the project has a Legion daemon.
  const viewCols = useMemo(
    () => (hasLegion ? builtIn.cols : builtIn.cols.filter((id) => !isBoardColumn(id))),
    [builtIn, hasLegion]
  );
  const storedLayout = useTableLayout(storeKey, viewLayoutColumns(viewCols));
  const colsParam = params.get("cols");
  // Start next is offered only on the views that list Todo issues; elsewhere its column is not
  // drawn, so the view's own columns get its width.
  const trailingWidth = ["open", "waiting", "mine"].includes(builtIn.id) ? ACTION_WIDTH : 0;
  const layout = useMemo(() => {
    const kept =
      storedLayout.order.length > 0
        ? storedLayout.order
        : [
            "select",
            ...viewCols,
            ...COLUMNS.map((column) => column.id).filter(
              (id) => id !== "select" && !viewCols.includes(id)
            ),
          ];
    // On a phone Select and Key are the pinned columns, and Title, which fills the rest of the
    // box, comes straight after them, so a row at rest reads its key and its title.
    const phone = storedLayout.boxWidth < 640;
    const phoneOrder = (ids: readonly string[]) => [
      "select",
      "key",
      "title",
      ...ids.filter((id) => id !== "select" && id !== "key" && id !== "title"),
    ];
    const order = phone ? phoneOrder(kept) : kept;
    // A header drag on a phone keeps that rule: a column dropped on Title lands right after it
    // (the core puts it before Title, and Title is put back after Key), a column dropped on
    // another moves among the columns after Title, and Title dragged anywhere stays where it
    // is. What is stored is the order drawn, so it survives a reload.
    const setOrder: typeof storedLayout.setOrder = phone
      ? (next) =>
          storedLayout.setOrder((current) =>
            phoneOrder(typeof next === "function" ? next(current) : next)
          )
      : storedLayout.setOrder;
    const hidden: VisibilityState = hasLegion
      ? {}
      : Object.fromEntries(COLUMNS.filter((c) => c.board).map((c) => [c.id, false]));
    const shown = colsParam === null ? undefined : new Set(["select", ...colsParam.split(",")]);
    const visibility: VisibilityState =
      shown === undefined
        ? { ...storedLayout.visibility, ...hidden }
        : {
            ...Object.fromEntries(COLUMNS.map((column) => [column.id, shown.has(column.id)])),
            ...hidden,
          };
    // Until the reader drags a width, a view's columns are fitted to the box: Title gives first,
    // down to 180 px, then every other column toward its minimum, then Title again down to
    // 156 px, so the view's own columns are on screen rather than past its right edge.
    const responsive = (() => {
      const { boxWidth: box, responsive: drawn, sizing } = storedLayout;
      if (Object.keys(sizing).length > 0 || box < 640) return drawn;
      const ids = order.filter((id) => visibility[id] !== false);
      const widths = { ...drawn.sizing };
      let over = ids.reduce((sum, id) => sum + (widths[id] ?? 0), 0) + trailingWidth - box + 2;
      if (over <= 0) return drawn;
      const title = widths.title;
      if (title !== undefined && ids.includes("title")) {
        const cut = Math.min(over, Math.max(0, title - 180));
        widths.title = title - cut;
        over -= cut;
      }
      const slack = ids
        .filter((id) => id !== "select" && id !== "title")
        .map((id) => [id, Math.max(0, (widths[id] ?? 0) - (COLUMN_MIN[id] ?? 0))] as const);
      const total = slack.reduce((sum, [, room]) => sum + room, 0);
      if (over > 0 && total > 0) {
        const given = Math.min(over, total);
        for (const [id, room] of slack) widths[id] = (widths[id] ?? 0) - (given * room) / total;
        over -= given;
      }
      // The last of it, where every other column is at its minimum: All open's eight columns
      // and Start next beside the sidebar at 1440 px.
      const narrowed = widths.title;
      if (over > 0 && narrowed !== undefined && ids.includes("title"))
        widths.title = narrowed - Math.min(over, Math.max(0, narrowed - 156));
      return { ...drawn, sizing: widths };
    })();
    if (shown === undefined) return { ...storedLayout, order, responsive, setOrder, visibility };
    return {
      ...storedLayout,
      order,
      setOrder,
      responsive,
      visibility,
      // `cols=` holds for this load and writes nothing: a change of columns rewrites it.
      setVisibility: (next: VisibilityState | ((current: VisibilityState) => VisibilityState)) => {
        const resolved = typeof next === "function" ? next(visibility) : next;
        const ids = COLUMNS.map((column) => column.id).filter(
          (id) => id !== "select" && resolved[id] !== false
        );
        update((current) => {
          current.set("cols", ids.join(","));
          return current;
        });
      },
    };
  }, [colsParam, hasLegion, storedLayout, trailingWidth, update, viewCols]);
  const { attachAbove, boxWidth } = layout;

  // The reads: the open listing, the done issues the view lists, and the board.
  const nowMs = Date.now();
  const done = doneRead(params, hasLegion, nowMs);
  const claimPicked = (filters.find((filter) => filter.id === "claim")?.value ?? []) as string[];
  const wantsFree = claimPicked.includes(UNCLAIMED_FACET);
  const registry = useAgentRegistry(wantsFree || openFilter === "claim");
  const {
    all,
    allItems,
    ancestry,
    doneIssues,
    doneLoaded,
    epics,
    issues,
    listingError,
    missingParents,
  } = useProjectIssueItems(project, {
    done,
    registry,
    board: board.data,
    hasLegion,
    viewer,
  });

  // The narrowings that are no column: the flags, `exclude_tree` and `only`.
  const excludedText = params.getAll("exclude_tree").join(",");
  const excluded = useMemo(
    () => (excludedText === "" ? [] : excludedText.split(",")),
    [excludedText]
  );
  const onlyParam = params.get("only");
  const only = useMemo(
    () => (onlyParam === null ? undefined : new Set(onlyParam.split(",").filter(Boolean))),
    [onlyParam]
  );
  // A view whose Status filter picks Done alone holds only the done rows it read, so its footer
  // counts its rows against those rather than against the open listing beside them.
  const statusValue = filters.find((filter) => filter.id === "status")?.value;
  const doneOnly =
    Array.isArray(statusValue) && statusValue.length === 1 && statusValue[0] === "done";
  const items = useMemo(() => {
    const shown = allItems.filter(
      ({ issue, facts }) =>
        (!doneOnly || issue.status === "done") &&
        (!flags.needsYou || issue.open_asks > 0) &&
        (!flags.unread || issueIsUnread(issue, userState.data?.[issue.key]?.last_read_seq ?? 0)) &&
        !excluded.some((tree) => tree === issue.key || tree === facts?.root_key) &&
        (only === undefined || only.has(issue.key))
    );
    if (grouping[0] !== "epic") return shown;
    const headings = new Set(
      shown
        .filter(
          (item) =>
            !isEpic(item.issue) && matchesFilters(item, filters) && matchesSearch(item, search)
        )
        .map(({ epicKey }) => epicKey)
    );
    return shown.filter(({ issue }) => !isEpic(issue) || !headings.has(issue.key));
  }, [
    allItems,
    doneOnly,
    filters,
    flags.needsYou,
    flags.unread,
    grouping,
    search,
    userState.data,
    only,
    excluded,
  ]);

  const columns = useIssueTableColumns(userState.data, boxWidth < 640, recordMark);

  const model = useDataTable<Item>({
    rows: items,
    columns,
    getRowId: (item) => item.issue.key,
    layout,
    names: ISSUE_TABLE_NAMES,
    state: { sorting, grouping, columnFilters: filters, globalFilter: search, pagination },
    onSortingChange: setSorting,
    onGroupingChange: setGrouping,
    onColumnFiltersChange: setFilters,
    onGlobalFilterChange: setSearch,
    onPaginationChange: setPagination,
    viewKey: params.toString(),
    selection: { state: selection, onChange: setSelection, onMark: recordMark },
    search: { column: "title", filterFn: fuzzy },
    groupOrder: { compare: compareGroups, heading: groupValue },
  });
  const { table } = model;
  const returnFocus = () => {
    const leaves = model.displayRows.flatMap(({ row }) => (row.getIsGrouped() ? [] : [row.id]));
    const last = lastMarked.current;
    const key = last !== null && leaves.includes(last) ? last : leaves[0];
    return key === undefined ? searchRef.current : model.focusRow(key, ROW_CHECKBOX);
  };

  const listedRows = table.getFilteredRowModel().rows;
  const listedKeys =
    issues.data === undefined ? undefined : listedRows.map((row) => row.id).join(" ");
  useEffect(() => {
    if (listedKeys === undefined) return;
    const listed = new Set(listedKeys === "" ? [] : listedKeys.split(" "));
    setSelection((current) =>
      Object.keys(current).every((key) => listed.has(key))
        ? current
        : Object.fromEntries(Object.entries(current).filter(([key]) => listed.has(key)))
    );
  }, [listedKeys]);

  const selectedSignature = listedRows
    .filter((row) => row.getIsSelected())
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

  const unreadableAncestors =
    missingParents.length === 0
      ? 0
      : ancestry.isError
        ? missingParents.length
        : (ancestry.data?.unreadable.length ?? 0);
  const pageCount = table.getPageCount();
  useEffect(() => {
    if (
      (done === undefined || doneIssues.data !== undefined) &&
      issues.data &&
      pagination.pageIndex >= Math.max(pageCount, 1)
    )
      table.setPageIndex(Math.max(pageCount - 1, 0));
  }, [done, doneIssues.data, issues.data, pagination.pageIndex, pageCount, table]);
  const openFacets = useMemo(
    () => (openFilter ? facetCounts(items, openFilter, filters, search) : EMPTY_FACETS),
    [items, openFilter, filters, search]
  );

  // Each tab's count, from the data the page holds: the open listing (and the board) for every
  // view but Finished, whose done read counts once the view has been opened in the page.
  const [finishedOpened, setFinishedOpened] = useState(builtIn.id === "finished");
  if (builtIn.id === "finished" && !finishedOpened) setFinishedOpened(true);
  const finishedWindow = windowRead("28d", nowMs) ?? {};
  const finishedRead = useQuery({
    ...projectDoneIssuesQuery(project, finishedWindow),
    enabled: finishedOpened,
  });
  const text = useAgentText();
  const counts = useMemo(() => {
    const result = new Map<string, number | undefined>();
    for (const entry of views) {
      const own = readUrl(new URLSearchParams([["view", entry.id]]), hasLegion).filters;
      if (entry.id === "finished") {
        const rows = finishedRead.data;
        result.set(
          entry.id,
          rows === undefined
            ? undefined
            : buildItems(rows, text.label, [], undefined, { board: board.data, hasLegion })
                .filter((item) => item.issue.closed_at !== null)
                .filter((item) => matchesFilters(item, own)).length
        );
        continue;
      }
      // A view whose rows come from data the page does not hold yet says no count, not 0.
      const needsBoard = hasLegion && (entry.legionOnly === true || entry.id === "in-flight");
      if (issues.data === undefined || (needsBoard && board.data === undefined)) {
        result.set(entry.id, undefined);
        continue;
      }
      result.set(entry.id, allItems.filter((item) => matchesFilters(item, own)).length);
    }
    return result;
  }, [views, hasLegion, allItems, finishedRead.data, board.data, text, issues.data]);
  const savedCounts = useMemo(() => {
    const result = new Map<string, number | undefined>();
    for (const entry of saved) {
      const query = new URLSearchParams(entry.query);
      if (listsClosed(query, hasLegion)) {
        result.set(entry.name, entry.name === savedName ? listedRows.length : undefined);
        continue;
      }
      const own = readUrl(query, hasLegion).filters;
      result.set(entry.name, allItems.filter((item) => matchesFilters(item, own)).length);
    }
    return result;
  }, [saved, hasLegion, allItems, savedName, listedRows.length]);

  const selectView = (id: string) => {
    setChosenSaved(undefined);
    const next = viewFor(id, hasLegion);
    update(() => {
      const fresh = new URLSearchParams();
      if (next.id !== "open") fresh.set("view", next.id);
      return fresh;
    });
  };
  const selectSaved = (name: string) => {
    const entry = saved.find((candidate) => candidate.name === name);
    if (entry === undefined) return;
    setChosenSaved(name);
    update(() => new URLSearchParams(entry.query));
  };
  const writeSaved = (next: readonly { name: string; query: string }[]) =>
    setStored({ version: 1, projects: { ...stored.projects, [project]: [...next] } });
  const saveView = (name: string) => {
    const query = new URLSearchParams(params);
    query.delete("page");
    if (!query.has("cols"))
      query.set(
        "cols",
        table
          .getVisibleLeafColumns()
          .map((column) => column.id)
          .filter((id) => id !== "select")
          .join(",")
      );
    query.set("layout", layoutChoice);
    const entry = { name, query: query.toString() };
    writeSaved([...saved.filter((candidate) => candidate.name !== name), entry]);
    setChosenSaved(name);
    update(() => new URLSearchParams(entry.query));
  };
  const deleteSaved = (name: string) => {
    writeSaved(saved.filter((candidate) => candidate.name !== name));
    setChosenSaved(undefined);
  };
  const reset = () => {
    if (savedView !== undefined) {
      update(() => new URLSearchParams(savedView.query));
      return;
    }
    update((current) => resetView(current, hasLegion));
  };

  // The bar: the listed rows by the view's Bar by column.
  const barBy = builtIn.bar;
  const barColumn = barBy === undefined ? undefined : BAR_COLUMN[barBy];
  const barPicks = (() => {
    if (barColumn === undefined || !params.has(`f.${barColumn}`)) return undefined;
    const value = filters.find((filter) => filter.id === barColumn)?.value;
    return Array.isArray(value) ? value.map(String) : undefined;
  })();
  const narrowTo = (values: readonly string[] | undefined) => {
    if (barColumn === undefined) return;
    if (values === undefined)
      update((current) => {
        current.delete(`f.${barColumn}`);
        current.delete("page");
        return current;
      });
    else table.getColumn(barColumn)?.setFilterValue([...values]);
  };

  // The chips of the narrowings an address carries that are no column's filter.
  const extraChips: ExtraChip[] = [
    ...excluded.map((tree) => ({
      label: `Not tree ${tree}`,
      remove: () =>
        update((current) => {
          const rest = current.getAll("exclude_tree").filter((value) => value !== tree);
          current.delete("exclude_tree");
          for (const value of rest) current.append("exclude_tree", value);
          return current;
        }),
    })),
    ...(only === undefined
      ? []
      : [
          {
            label: params.get("onlyLabel") ?? `${only.size} issues`,
            remove: () =>
              update((current) => {
                current.delete("only");
                current.delete("onlyLabel");
                return current;
              }),
          },
        ]),
  ];
  // The Columns menu lists the Legion columns and the stage first, where a reader looks for
  // them, then the issue's own columns.
  const legionFirst = (id: string) =>
    isBoardColumn(id) || id === "stage" || id === "queue" || id === "closed" ? 0 : 1;
  const offeredColumns: Column<Item>[] = table
    .getAllLeafColumns()
    .filter((column) => hasLegion || !isBoardColumn(column.id))
    .sort((a, b) => legionFirst(a.id) - legionFirst(b.id));

  // A view with a Legion filter waits for the board: it never lists unfiltered rows under the
  // view's name.
  const legionFiltered =
    hasLegion &&
    filters.some(
      (filter) => isBoardColumn(filter.id) || filter.id === "stage" || filter.id === "queue"
    );
  const pageRows = table.getRowModel().rows;
  const pageMax = Math.max(0, ...pageRows.map((row) => phaseHours(row.original.facts).total));
  const statusPicks = (() => {
    const value = filters.find((filter) => filter.id === "status")?.value;
    return Array.isArray(value) ? value.map(String) : undefined;
  })();
  const stagePicks = (() => {
    const value = filters.find((filter) => filter.id === "stage")?.value;
    return Array.isArray(value) ? value.map(String) : undefined;
  })();
  // TanStack hands back the same sorted model until the rows, the filters or the sort change.
  const sortedModel = table.getSortedRowModel();
  const boardRows = useMemo(
    () => sortedModel.flatRows.filter((row) => !row.getIsGrouped()).map((row) => row.original),
    [sortedModel]
  );

  const tabs = (
    <IssueViewTabs
      active={active}
      boardHeader={board.data?.header}
      boardPending={board.isPending}
      changed={changed}
      counts={counts}
      hasLegion={hasLegion}
      onDelete={deleteSaved}
      onReset={reset}
      onSave={saveView}
      onSelect={selectView}
      onSelectSaved={selectSaved}
      saved={saved}
      savedCounts={savedCounts}
      views={views}
    />
  );

  const loading =
    issues.isPending ||
    userState.isPending ||
    (done !== undefined && doneIssues.isPending) ||
    (legionFiltered && board.isPending);
  const failed = (issues.isError && issues.data === undefined) || userState.isError;
  const boardFailed = legionFiltered && board.isError;

  const toolbarTrailing = (
    <>
      <LayoutSwitch layout={layoutChoice} onLayout={setLayout} />
      {layoutChoice === "board" ? (
        <>
          <Chip onClick={() => setShowIcebox(!showIcebox)} selected={showIcebox} size="compact">
            Show Icebox
          </Chip>
          <Chip onClick={() => setLanes(!lanes)} selected={lanes} size="compact">
            Lanes
          </Chip>
        </>
      ) : (
        <ColumnsMenu columns={offeredColumns} size="compact" />
      )}
    </>
  );

  return (
    <section aria-label="Project issues" className="space-y-2 sm:pb-24" data-view={builtIn.id}>
      <div className="space-y-2" ref={attachAbove}>
        {tabs}
        <ViewBar
          by={barBy}
          items={loading ? [] : listedRows.map((row) => row.original)}
          narrowTo={narrowTo}
          notStartedNumber={builtIn.notStartedNumber === true}
          picked={barPicks}
        />
        <IssueTableToolbar
          columns={offeredColumns}
          extraChips={extraChips}
          // Only while the view is changed: a built-in view whose address carries more than its
          // own parameters, or a saved view whose address differs from its saved query.
          onClear={changed ? reset : undefined}
          searchRef={searchRef}
          table={table}
          trailing={toolbarTrailing}
          unclaimedUnchecked={
            wantsFree && registry.isError
              ? registry.refilling
                ? "refilling"
                : "unreachable"
              : undefined
          }
        />
        {listingError ? (
          <p className={dangerText} role="alert">
            Could not load {done !== undefined ? "closed " : ""}issues.{" "}
            <button
              aria-label={
                done !== undefined ? "Retry loading closed issues" : "Retry loading issues"
              }
              onClick={() => void listingError.refetch()}
              type="button"
            >
              Retry
            </button>
          </p>
        ) : null}
        {unreadableAncestors > 0 ? (
          <p className={dangerText} role="alert">
            Could not load {unreadableAncestors}{" "}
            {unreadableAncestors === 1 ? "ancestor" : "ancestors"}; affected rows show Unknown epic.{" "}
            <button onClick={() => void ancestry.refetch()} type="button">
              Retry loading ancestors
            </button>
          </p>
        ) : null}
        {notice ? (
          <p
            aria-live="polite"
            className={`text-sm ${notice.kind === "error" ? dangerText : textSecondaryOnCanvas}`}
            role={notice.kind === "error" ? "alert" : "status"}
          >
            {notice.text}
          </p>
        ) : null}
        {interrupted ? (
          <section aria-label="Interrupted table action" className={textSecondaryOnCanvas}>
            <p>{interruptedDescription}</p>
            <button
              className={`min-h-11 rounded-lg border px-3 text-sm ${borderDefault} ${focusVisibleRing}`}
              onClick={finish}
              type="button"
            >
              Finish
            </button>{" "}
            <button
              className={`min-h-11 rounded-lg border px-3 text-sm ${borderDefault} ${focusVisibleRing}`}
              onClick={dismiss}
              type="button"
            >
              Dismiss
            </button>
          </section>
        ) : null}
        {unansweredEpic ? (
          <LandedWithoutAnswer what={`The epic “${unansweredEpic}”`}>
            The selected issues have not been moved. Find the epic in the project before moving
            them.{" "}
            <button onClick={dismissUnansweredEpic} type="button">
              Dismiss
            </button>
          </LandedWithoutAnswer>
        ) : null}
        {duplicateEpic ? (
          <div className={textSecondaryOnCanvas} role="alert">
            <p>{duplicateEpic.message}</p>
            <button
              disabled={busy}
              onClick={() => void newEpic(duplicateEpic.keys, duplicateEpic.title, true)}
              type="button"
            >
              Create anyway
            </button>
            <button onClick={() => setDuplicateEpic(undefined)} type="button">
              Cancel new epic
            </button>
          </div>
        ) : null}
      </div>

      {failed ? (
        <p className={dangerText} role="alert">
          Could not load issues.{" "}
          <button
            onClick={() => {
              void issues.refetch();
              void userState.refetch();
              if (missingParents.length) void ancestry.refetch();
            }}
            type="button"
          >
            Retry loading issues
          </button>
        </p>
      ) : boardFailed ? (
        <QueryError
          message="Could not read the Legion board, which this view needs."
          onRetry={() => void board.refetch()}
          retrying={board.isFetching}
        />
      ) : loading ? (
        <LoadingSkeleton label="Loading issues" />
      ) : layoutChoice === "board" ? (
        <IssueBoard
          hasLegion={hasLegion}
          lanes={lanes}
          listsDone={done !== undefined}
          project={project}
          rows={boardRows}
          showIcebox={showIcebox}
          stagePicks={stagePicks}
          statusPicks={statusPicks}
          userState={userState.data}
        />
      ) : (
        <PhaseHoursContext value={{ mode: phaseMode, pageMax }}>
          <DataTable
            empty={<span className={textMutedOnCanvas}>No issues match these filters</span>}
            filterCounts={openFacets}
            filterUnloaded={done !== undefined && !doneLoaded ? DONE_UNLOADED : undefined}
            label="Project issues table"
            model={model}
            onOpenFilterChange={setOpenFilter}
            openFilter={openFilter}
            pager={ISSUE_TABLE_PAGER}
            renderFilter={({ column, open, onOpenChange }) =>
              column.columnDef.meta?.range ? (
                <RangeFilter column={column} onOpenChange={onOpenChange} open={open} />
              ) : (
                <span className="flex items-center gap-1">
                  {column.id === ("phase_hours" satisfies ColumnId) ? (
                    <PhaseHoursModeToggle mode={phaseMode} onChange={setPhaseMode} />
                  ) : null}
                  <ColumnFilter
                    column={column}
                    countNoun={ISSUE_TABLE_NAMES.filterCountNoun}
                    counts={column.id === openFilter ? openFacets : EMPTY_FACETS}
                    onOpenChange={onOpenChange}
                    open={open}
                    unloaded={
                      done === undefined || doneLoaded
                        ? NONE
                        : (DONE_UNLOADED.get(column.id) ?? NONE)
                    }
                  />
                </span>
              )
            }
            renderGroup={({ entry, index, measure }) => (
              <GroupRow
                columns={table.getVisibleLeafColumns().length + 1}
                continued={entry.continued}
                epics={epics}
                grouping={grouping[0] ?? ""}
                index={index}
                measure={measure}
                notStartedNumber={builtIn.notStartedNumber === true}
                onMerge={mergeEpic}
                onRename={renameEpic}
                onStartAll={(key) =>
                  startNext(
                    allItems.filter((item) => item.epicKey === key).map((item) => item.issue.key)
                  )
                }
                row={entry.row}
                shown={entry.shown}
                viewportWidth={boxWidth}
              />
            )}
            renderRow={({ row, index, measure }) => (
              <IssueRow
                busy={busy}
                index={index}
                measure={measure}
                moved={moved}
                onStartNext={trailingWidth === 0 ? undefined : (key) => startNext([key])}
                row={row}
              />
            )}
            scrollTestId="issues-table-scroll"
            trailingWidth={trailingWidth}
          />
        </PhaseHoursContext>
      )}

      <IssueTableSelection
        busy={busy}
        epics={epics}
        moveTo={moveTo}
        newEpic={newEpic}
        returnFocus={returnFocus}
        startNext={startNext}
        table={table}
        unansweredEpic={unansweredEpic}
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
