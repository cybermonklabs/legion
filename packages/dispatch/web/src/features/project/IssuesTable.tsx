import { useQuery } from "@tanstack/react-query";
import type { FilterFn, RowSelectionState } from "@tanstack/react-table";
import { type ReactNode, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { userStateQuery } from "../../api/queries";
import {
  DataTable,
  type DataTableNames,
  type RowNoun,
  useDataTable,
} from "../../components/DataTable";
import type { FilterChip } from "../../components/filter-chips";
import { ARM_ON_PRESS, useFocusPlace } from "../../components/focus-place";
import { heldPress } from "../../components/held-press";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import type { PagerOptions } from "../../components/Pager";
import { focusListStop, useLastMarkedPlace } from "../../components/SelectionBar";
import { TABLE_FRAME_MARK } from "../../components/table-keys";
import { useTableLayout } from "../../components/table-layout";
import {
  addressPath,
  copyAddress,
  useAddressNavigate,
  usePageAddress,
} from "../../lib/page-address";
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
import { ISSUES_NOT_LOADED, useIssueTableActions } from "./issues-table-actions";
import {
  issueTableLayout,
  listingColumns,
  type ShownColumnId,
  useIssueTableColumns,
} from "./issues-table-columns";
import { answeredWhole, type IssueListing, useIssueTableItems } from "./issues-table-data";
import {
  ALL_ROWS,
  clearPicked,
  compareGroups,
  facetCounts,
  groupValue,
  ISSUE_GROUP_ATTRIBUTE,
  ISSUE_LINK_ATTRIBUTE,
  ISSUE_ROW_ATTRIBUTE,
  type Item,
  isEpic,
  matchesFilters,
  matchesSearch,
  PAGE_SIZES,
  tableStorageKey,
  UNCLAIMED_FACET,
} from "./issues-table-model";
import { IssueTableSelection, IssueTableStatus, IssueTableToolbar } from "./issues-table-page";
import { GroupRow, IssueRow } from "./issues-table-rows";
import { useTableView } from "./issues-table-view";
import { issueIsUnread } from "./UnreadDot";

const EMPTY_FACETS: ReadonlyMap<string, number> = new Map();
/** Status offers Done before the closed issues it would count are read. */
const DONE_UNLOADED: ReadonlyMap<string, readonly string[]> = new Map([["status", ["done"]]]);
/** The one layout key of every table scoped to a priority: the work advancing a priority is
 *  one table, drawn under an opened row of the priorities page and on the priority's own page,
 *  so the columns a reader drags there are dragged once rather than once per priority. */
const SCOPED_LAYOUT_KEY = "priority-work";

const fuzzy: FilterFn<Item> = (row, _id, query: string) => matchesSearch(row.original, query);

const ACTION_WIDTH = 104;

/** The checkbox a row draws for its selection (`issues-table-columns.tsx`'s select column), the
 *  one checkbox every row has. */
const ROW_CHECKBOX = 'input[type="checkbox"]';

/** The List's own words, which the table core reads and never invents: the attributes its rows
 *  and groups carry, the link opening a row follows, and the project scope's row actions that
 *  walk it, which every list of that scope shares. */
const ISSUE_TABLE_NAMES: DataTableNames = {
  groupAttribute: ISSUE_GROUP_ATTRIBUTE,
  keymap: {
    next: "project.next",
    open: "project.open",
    openInNewTab: "project.open-new-tab",
    openOnEnter: "project.open-enter",
    previous: "project.previous",
    scope: "project",
    select: "project.select",
  },
  linkAttribute: ISSUE_LINK_ATTRIBUTE,
  rowAttribute: ISSUE_ROW_ATTRIBUTE,
};

/** The same words for an issue table a page draws beside its main table over the same keys - the
 *  priorities page's fault lists, and the work under an opened priority - whose `j` and `k` walk
 *  it only from one of its own rows (`moveFrom: "row"`), so they never take those keys from the
 *  main table. */
const ISSUE_TABLE_BESIDE_NAMES: DataTableNames = {
  ...ISSUE_TABLE_NAMES,
  keymap: { ...ISSUE_TABLE_NAMES.keymap, moveFrom: "row" },
};

/** What the List's rows are, which its header filters count in and its pager counts: issues, in
 *  groups, over rows. */
const ISSUE_TABLE_NOUN: RowNoun = {
  group: { one: "group", many: "groups" },
  line: { one: "row", many: "rows" },
  many: "issues",
  one: "issue",
};

/** The line under the List: its page sizes, the one that means every row, and its test id. */
const ISSUE_TABLE_PAGER: PagerOptions = {
  allRows: ALL_ROWS,
  pageSizes: PAGE_SIZES,
  testId: "issues-table-pager",
};

export function IssuesTable({
  label,
  layout: ownLayout,
  listing,
  moveFrom = "page",
  priorityTitles,
  stateKey,
}: {
  /** What this list is called, for a reader who cannot see where it sits; by its listing when
   *  unset: a project's issues, or the issues advancing one priority. */
  label?: string;
  /** The layout this table keeps, under its own key, where it is not its listing's, and the
   *  columns it opens with shown: each fault list of the priorities page shows its listing
   *  columns and Updated, which a table opens with hidden (`issueTableLayout`). */
  layout?: { readonly key: string; readonly shown: readonly ShownColumnId[] };
  /** What this table lists: one project's issues, or the issues advancing one priority, which
   *  belong to no project (`issues-table-data.ts`). */
  listing: IssueListing;
  /** Where its `j` and `k` walk its rows from (`RowKeymap.moveFrom`): anywhere on the page the
   *  focus is in no other table, for the page's one table or its main one (the default); or only
   *  from one of its own rows, for a table drawn beside the page's main one. */
  moveFrom?: "page" | "row";
  /** Each priority's title by its id. Given, the table has the Advances column, which names each
   *  priority a row advances by its title; unset, it has none (`listingColumns`). */
  priorityTitles?: ReadonlyMap<string, string>;
  /** The prefix this table's own view takes in the address: `<stateKey>.sort` and the rest.
   *  Unset keeps the bare `sort`, `q`, `group`, `page` and `size` the project page publishes. */
  stateKey?: string;
}): ReactNode {
  // What this list is, for a reader who cannot see where it sits: a project's issues, or the
  // issues advancing one priority, which are of every project.
  const name = label ?? ("project" in listing ? "Project issues" : "Priority issues");
  // Whether the listing's one open read is every row it holds, so the table reads no closed
  // issue beside it (`answeredWhole`).
  const whole = answeredWhole(listing);
  const { params, view, setFilters, setGrouping, setPagination, setSearch, setSorting } =
    useTableView(stateKey);
  const address = usePageAddress();
  const navigate = useAddressNavigate();
  const { sorting, grouping, filters, search } = view;
  const pagination = { pageIndex: view.page, pageSize: view.pageSize };
  // The listing columns this table has: the ones its listing gives a use (`listingColumns`).
  const columnsLayout = issueTableLayout(
    listingColumns(listing, priorityTitles !== undefined),
    ownLayout?.shown
  );
  // One stored layout per project, and one for every scoped table: the work advancing a
  // priority is one table drawn in two places, so a reader who drags a column there drags it
  // for the priorities page and the priority's own page alike, and not once per priority.
  const layout = useTableLayout(
    tableStorageKey(ownLayout?.key ?? ("project" in listing ? listing.project : SCOPED_LAYOUT_KEY)),
    columnsLayout
  );
  const { attachAbove, boxWidth } = layout;
  const flags = useIssueFilters();
  const userState = useQuery(userStateQuery());
  const [selection, setSelection] = useState<RowSelectionState>({});
  // The issue the reader marked last - by its own checkbox, or `x` on its row: where the focus
  // goes when the bar goes with it (`returnFocus`, below). A select-all marks no row of the
  // reader's own, so it records none. The List's hand-off (`noteFocus`, below) never lands in the
  // search box (`searchRef`), which takes typed text.
  const lastMarked = useRef<string | null>(null);
  const searchRef = useRef<HTMLInputElement>(null);
  const recordMark = useCallback((key: string) => {
    lastMarked.current = key;
  }, []);
  const [openFilter, setOpenFilter] = useState<string>();

  const statusPicked = (filters.find((filter) => filter.id === "status")?.value ?? []) as string[];
  const wantsDone = !whole && (statusPicked.length === 0 || statusPicked.includes("done"));
  const claimPicked = (filters.find((filter) => filter.id === "claim")?.value ?? []) as string[];
  // Which claims have lapsed is the agent registry's answer, read only while the Claimed by
  // filter asks for it: because it is picked, or because its panel is open and counting.
  const wantsFree = claimPicked.includes(UNCLAIMED_FACET);
  const readsRegistry = wantsFree || openFilter === "claim";
  const registry = useAgentRegistry(readsRegistry);
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
  } = useIssueTableItems(listing, { wantsDone, registry });
  const loading = issues.isPending || userState.isPending;
  const failed = !loading && ((issues.isError && issues.data === undefined) || userState.isError);
  const loaded = !loading && !failed;
  // The issues an address names with `only=`, which the table shows alone until its chip is
  // removed. The narrowing is the reader's, so it outranks nothing: every other filter still
  // applies to what is left.
  const picked = view.selection;
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
  // What the table is narrowed to, and the way back to every row: removing the chip clears
  // `only=` and leaves every other control where the reader set it. It is a chip of the control
  // row, beside the flags' and the columns', drawn by the same `filterChips` and named as they
  // are, `Remove Showing: <title> filter`, and its removal leaves a history entry, so Back gives
  // the narrowing back (`useAddressNavigate`'s `replace: false`). The toolbar is drawn in every
  // state, so the chip counts what it shows only once the rows the table waits on are in: not
  // beside the loading skeleton, not after a failed read, and not while the closed issues it
  // asked for are still out, where it would count rows the table has not read. Until then it
  // names its narrowing alone.
  const counted = loaded && listingError === undefined && (!wantsDone || doneLoaded);
  const pickedChip: FilterChip | undefined =
    picked === null
      ? undefined
      : {
          key: "only",
          label: `Showing: ${picked.label}`,
          text: counted
            ? `Showing: ${picked.label} · ${items.length} of ${picked.keys.size} ${picked.keys.size === 1 ? "issue" : "issues"}`
            : undefined,
          remove: () =>
            navigate(
              addressPath(address.pathname, clearPicked(copyAddress(address.search)), address.hash),
              { replace: false }
            ),
        };

  const columns = useIssueTableColumns(
    userState.data,
    boxWidth < 640,
    recordMark,
    columnsLayout,
    priorityTitles
  );

  const model = useDataTable<Item>({
    rows: items,
    columns,
    getRowId: (item) => item.issue.key,
    layout,
    names: moveFrom === "row" ? ISSUE_TABLE_BESIDE_NAMES : ISSUE_TABLE_NAMES,
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
  // The issues this page of the table lists, in its order: its rows, not its group headings.
  const leaves = () => model.displayRows.flatMap(({ row }) => (row.getIsGrouped() ? [] : [row.id]));
  // The List's own stop: its named table, which is drawn with no row as well.
  const tableStop = () => model.layout.scrollRef.current?.querySelector("table") ?? null;
  // The bar's hand-off (`useLastMarkedPlace`): the checkbox of the issue marked last while the
  // table draws its row, out of sight or not. After a press that took that issue out of the view
  // - off the list, or out of what the filters show, as Take out of epic does under the Epic
  // filter: a row the reader can see at its place, else before it. With no issue marked, as after
  // a select-all from the table's head: the first row the reader can see below the table's sticky
  // head. Each is shown whole at its nearest edge, below that head too. `head` is a header cell:
  // the cells stick, and the `thead` does not (`DataTable`). Else the table's own stop, with no
  // scroll. A row the window has not drawn is out of their sight, so it is never where the focus
  // lands in the place of an issue that went.
  const landAtPlace = useLastMarkedPlace({
    control: (key) =>
      model.layout.scrollRef.current
        ?.querySelector(`[${ISSUE_ROW_ATTRIBUTE}="${CSS.escape(key)}"]`)
        ?.querySelector<HTMLElement>(ROW_CHECKBOX) ?? null,
    head: () => model.layout.scrollRef.current?.querySelector("thead th") ?? null,
    lastMarked: () => lastMarked.current,
    list: tableStop,
    order: leaves,
    shown: Object.keys(selection).length !== 0,
  });
  // The bar gone with the focus (`SelectionBar`): the hand-off above (`landAtPlace`) has put the
  // focus itself, save where the table lists the issue marked last and its window has not drawn
  // that row: the row is scrolled to, and its checkbox takes the focus as it lands (`focusRow`).
  // A table action clears the selection only once the list read after it has settled
  // (`useIssueTableActions`), so the view read here is the one the action left.
  const returnFocus = () => {
    if (landAtPlace()) return null;
    // `landAtPlace` moved nothing, so the issue marked last is listed and its row is not drawn.
    const key = lastMarked.current;
    return key === null ? focusListStop(tableStop()) : model.focusRow(key, ROW_CHECKBOX);
  };
  // A control of the table's head - a column filter, or the list it opened in a portal of its
  // own - goes with the table when a press in it sends the List back to its skeleton. A notice's
  // button under the toolbar goes with the notice it answers - the interrupted action's Finish
  // and Dismiss, the possible duplicate's Create anyway and Cancel new epic, the unanswered
  // epic's Dismiss - and its focus goes to the control now at its place among the notices, else
  // the one before it. Each of these takes the focus only while the reader can see it; where none
  // is in sight, the main content's own stop takes it with no scroll (`useFocusPlace`). Where the
  // head or the notices hold no other control, this hand-off walks back over the main content to
  // the nearest control before them that the reader can see and that takes no typed text, so
  // never into the search box. Only the head and the notices are counted: the selection bar, the
  // rows and the dialogs each hand the focus on themselves. The handler sits on the List's
  // section, which stays in every branch, beside the capture handlers that arm it at a press
  // inside the section (`armOnPress`): React bubbles a column filter's portalled list's events to
  // it too.
  const sectionRef = useRef<HTMLElement>(null);
  const noticesRef = useRef<HTMLDivElement>(null);
  const noteFocus = useFocusPlace(
    (element) => {
      const section = sectionRef.current;
      if (section === null) return null;
      // A portal's element is counted only where a control of the head was before it.
      if (!section.contains(element)) return section.querySelector("thead");
      const notices = noticesRef.current;
      return notices?.contains(element) ? notices : element.closest("thead");
    },
    () => null
  );

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
    leaving,
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
    listing,
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
  // The header filters count what the table shows once the rows it waits on are in, as the
  // chip does (`counted`): never the open issues alone while the closed ones it asked for are
  // out. While the Claimed by filter reads the registry, they also wait until the registry's
  // latest read has succeeded (`known`), so they stay hidden before its first answer and after a
  // read that failed: until then no session's claim reads lapsed (`claimIsTakeable`), so Nobody
  // or lapsed would leave out each claim whose session has gone, in its own count and, picked, in
  // every other filter's.
  const filterCounted = counted && (!readsRegistry || registry.known);

  const visibleColumns = table.getVisibleLeafColumns().length + 1;
  // A column filter's panel hangs off its header and goes with the table: it is closed then, as
  // a panel whose anchor has left the page closes (`AnchoredPanel`), so the table coming back
  // does not open it again and take the focus from wherever it went meanwhile.
  useEffect(() => {
    if (!loaded) setOpenFilter(undefined);
  }, [loaded]);

  // The toolbar is drawn in every branch, at the one place in the tree, as the Board draws its
  // `controls`: a filter press that sends the List off to read issues it has not loaded - Clear
  // filters back to the open issues, which a List opened on the closed ones has never read -
  // puts the skeleton in place of the table alone, and the control pressed, with the keyboard
  // focus on it, stays where it was. Everything else here needs the listing and waits for it.
  // The section is the table's frame: its toolbar, box and pager, which another table's
  // page-wide keys leave to it (`TABLE_FRAME_ATTRIBUTE`).
  return (
    <section
      aria-label={name}
      className="space-y-3 sm:pb-24"
      {...TABLE_FRAME_MARK}
      {...ARM_ON_PRESS}
      onFocus={noteFocus}
      ref={sectionRef}
    >
      <div className="space-y-3" ref={attachAbove}>
        {loaded ? (
          <>
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
                {unreadableAncestors === 1 ? "ancestor" : "ancestors"}; affected rows show Unknown
                epic.{" "}
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
          </>
        ) : null}

        <IssueTableToolbar
          // Shift+F and Shift+V belong to the page's one table, or its main one: a table drawn
          // beside it leaves them to that one.
          keyScope={moveFrom === "page" ? "project" : undefined}
          searchRef={searchRef}
          selection={pickedChip}
          stateKey={stateKey}
          table={table}
          unclaimedUnchecked={
            wantsFree && registry.isError
              ? registry.refilling
                ? "refilling"
                : "unreachable"
              : undefined
          }
        />

        {loaded ? (
          <div className="space-y-3" ref={noticesRef}>
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
            {duplicateEpic && newEpic ? (
              <div role="alert" className={textSecondaryOnCanvas}>
                <p>{duplicateEpic.message}</p>
                <button
                  className="refused:cursor-not-allowed refused:opacity-50"
                  type="button"
                  {...heldPress(
                    busy,
                    () =>
                      void newEpic(duplicateEpic.keys, {
                        force: true,
                        intoWork: duplicateEpic.intoWork,
                        title: duplicateEpic.title,
                      })
                  )}
                >
                  Create anyway
                </button>
                <button type="button" onClick={() => setDuplicateEpic(undefined)}>
                  Cancel new epic
                </button>
              </div>
            ) : null}
          </div>
        ) : null}
      </div>

      {loading ? <LoadingSkeleton label="Loading issues" /> : null}
      {failed ? (
        <p className={dangerText} role="alert">
          {ISSUES_NOT_LOADED}{" "}
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
      ) : null}
      {loaded ? (
        <>
          <DataTable
            empty="No issues match these filters."
            filterCounted={filterCounted}
            filterCounts={openFacets}
            filterUnloaded={doneLoaded || whole ? undefined : DONE_UNLOADED}
            label={`${name} table`}
            leaving={leaving}
            model={model}
            noun={ISSUE_TABLE_NOUN}
            onOpenFilterChange={setOpenFilter}
            openFilter={openFilter}
            pager={ISSUE_TABLE_PAGER}
            renderGroup={({ entry, index, measure }) => (
              <GroupRow
                busy={busy}
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
            size="compact"
            stackColumn="title"
            trailingWidth={ACTION_WIDTH}
          />

          <IssueTableSelection
            returnFocus={returnFocus}
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
              changes={confirming.changes}
              confirmLabel={confirming.confirmLabel}
              key={confirming.title}
              onCancel={cancelConfirming}
              onConfirm={confirmed}
              title={confirming.title}
            />
          )}
          {naming === undefined ? null : naming.kind === "new" ? (
            newEpic === undefined ? null : (
              <EpicNameDialog
                key="new-epic"
                onCancel={cancelNaming}
                onSubmit={(name) => void newEpic(naming.keys, { title: name })}
                submitLabel="Create epic"
                title="Name the new epic"
              />
            )
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
        </>
      ) : null}
    </section>
  );
}
