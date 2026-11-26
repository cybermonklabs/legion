import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useEffect, useLayoutEffect, useMemo, useRef } from "react";
import { apiErrorMessage } from "../../api/client";
import { legionDaemonQuery } from "../../api/queries";
import { ColumnsMenu } from "../../components/ColumnsMenu";
import type { FilterChip } from "../../components/filter-chips";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { QueryError } from "../../components/QueryError";
import {
  addressPath,
  copyAddress,
  useAddressNavigate,
  useAddressParams,
  useAddressUpdate,
  useNavigationState,
  usePageAddress,
  useTableAddress,
} from "../../lib/page-address";
import { useKeymap } from "../shell/keymap";
import { focusOnDocument } from "../shell/roving";
import { useUserPreference } from "../shell/userPreference";
import {
  DaemonHint,
  IssueViewTabs,
  LayoutSwitch,
  type ReadFailure,
  ViewControls,
  viewTabButtonId,
} from "./IssueViewTabs";
import { legionPhasesOf } from "./issues-legion";
import { type BoardToggles, IssuesTabBoard } from "./issues-tab-board";
import { type HeldDoneRead, useHeldDone, useTabCounts } from "./issues-tab-counts";
import { useTabTableLayout } from "./issues-tab-layout";
import { useTabReads } from "./issues-tab-reads";
import {
  ISSUES_NOT_LOADED,
  type IssueTableActions,
  useIssueTableActions,
} from "./issues-table-actions";
import { countStatuses, IssueStatusBar } from "./issues-table-bar";
import { type OwnNarrowings, ownNarrowings } from "./issues-table-chips";
import { IssueTableFrame, ListingNotices, ReadStates } from "./issues-table-frame";
import { readUrl, tableStorageKey, type View, writeUrl } from "./issues-table-model";
import { useIssueTable, useIssueTableReads } from "./issues-table-state";
import {
  type ActiveView,
  BUILT_IN_VIEWS,
  type BuiltInView,
  builtInAddress,
  clearNarrowings,
  currentAddress,
  expandView,
  isSavedViewChanged,
  isViewChanged,
  matchSavedView,
  narrowingsDiffer,
  type SavedViews,
  savedViewQuery,
  useExcludedTrees,
  useSavedViews,
  useViewLayout,
  type ViewLayout,
  viewFor,
  viewLayoutKey,
  viewsGrammar,
} from "./issues-views";

/** What the Issues tab is called, for a reader who cannot see where it sits. */
const NAME = "Project issues";

/** What an answer the server gave no words for is called in an alert. */
const NO_ANSWER = "Dispatch did not answer.";

/** What Done's alert says where its page read failed on an answer that does not name the sort it
 *  was sent (`UnsortedPageError`): a server older than the list route's `sort` answers it while a
 *  release rolls out. */
const UNSORTED_DONE_PAGE =
  "The server did not sort the closed issues by close time. Retry when the server update is complete.";

/** A read that can fail and be made again: what `readFailure` reads off a query. */
interface Rereadable {
  readonly error: unknown;
  readonly isFetching: boolean;
  readonly refetch: () => Promise<unknown>;
}

/** A failed read as the alert line under the tabs names it: its error, its Retry, and whether
 *  that Retry is out. */
function readFailure(query: Rereadable): ReadFailure {
  return {
    message: apiErrorMessage(query.error, NO_ANSWER),
    retry: () => void query.refetch(),
    retrying: query.isFetching,
  };
}

/** What a history entry of the Issues tab carries beside its address: the name of the saved view
 *  open on it (`savedViewOf`). */
const SAVED_VIEW_STATE = "savedView";

/** The saved view a history entry names as the one open on it, or none. */
function savedViewOf(state: unknown): string | undefined {
  if (typeof state !== "object" || state === null) return undefined;
  const name = Reflect.get(state, SAVED_VIEW_STATE);
  return typeof name === "string" ? name : undefined;
}

/** The Issues tab's table in the address: the project table's own spelling, with the view it
 *  names between that spelling and the address (`viewsGrammar`). A module constant, since
 *  `useTableAddress` memoizes what it reads on the grammar's identity. */
const PROJECT_GRAMMAR = viewsGrammar<View>({
  read: (params) => readUrl(params),
  write: (params, view) => writeUrl(params, view),
});

/** What the Issues tab holds above its views, which a view's table and Board read: the
 *  project's daemon, the view the tabs have open and the controls that change it. */
interface IssuesTabPlace {
  readonly project: string;
  /** The project names a Legion daemon, as its daemon setting last answered. */
  readonly hasLegion: boolean;
  /** The daemon setting's read failed with no answer: the project is drawn with no daemon. */
  readonly daemonFailure: ReadFailure | undefined;
  /** The built-in view the address opens, which a saved view narrows. */
  readonly base: BuiltInView;
  readonly active: ActiveView;
  readonly changed: boolean;
  /** The address the open view opens at: its built-in address, or its saved query. */
  readonly ownAddress: URLSearchParams;
  /** What the open view narrows by of itself, at `ownAddress` (`ownNarrowings`). */
  readonly own: OwnNarrowings;
  readonly saved: SavedViews;
  /** Opens the view at `address`, with `name` the saved view open on it, or none. A press on a
   *  tab pushes a history entry (`push`), so Back gives the view before it, but a press on the
   *  open view's tab at the address it is on writes over the entry; a save or a delete writes
   *  over the entry too. */
  readonly openView: (address: URLSearchParams, name: string | undefined, push: boolean) => void;
  readonly layout: ViewLayout;
  readonly setLayout: (layout: ViewLayout) => void;
  readonly toggles: BoardToggles;
  /** Done's own read once Done has been opened in the page, which counts Done's tab. */
  readonly heldDone: HeldDoneRead | undefined;
  readonly holdDone: (read: HeldDoneRead) => void;
}

/**
 * The project's Issues tab: its one list of issues, in views (Open, Done and the reader's saved
 * ones), in the table or the Board. It reads whether the project names a Legion daemon first,
 * and shows the loading state until that read answers; a failed read draws the project as one
 * with no daemon, with the error under the tabs, and a later failed read keeps the answer the
 * page drew. It runs the table's bulk actions and their recovery above the views, so a Start
 * next goes on across a change of view and ends only when the reader leaves the Issues tab or
 * the page, or closes the browser tab.
 */
export function IssuesTable({ project }: { project: string }): ReactNode {
  const daemon = useQuery(legionDaemonQuery(project));
  const params = useAddressParams();
  const here = usePageAddress();
  const update = useAddressUpdate();
  const navigate = useAddressNavigate();
  // An older address in today's spelling, written over the address the page opened on, so the
  // tab shows its mark and Clear filters takes what it added away.
  const rewritten = currentAddress(params)?.toString();
  useEffect(() => {
    if (rewritten !== undefined) update(() => copyAddress(rewritten));
  }, [rewritten, update]);
  const saved = useSavedViews(project);
  // The saved view the reader opened, which stays the open tab while they change it: by its tab,
  // or by an address that is its query, which opens it as a press on its tab does. Its name goes
  // with the history entry (`savedViewOf`), which every write of the address keeps, so Back to a
  // saved view the reader changed marks its tab again. An address that is another saved view's
  // query, while the open one is changed, opens that one instead. An address that names nothing
  // is the page as it opens (`page-address.ts`), Open unchanged, so no saved view stays open over
  // it: the project's link in the page's header lands there with the tab still drawn. A change of
  // a saved view never empties the address, since the view's query holds its columns and its
  // layout (`savedViewQuery`).
  const chosen = savedViewOf(useNavigationState());
  const matched = matchSavedView(saved.views, params);
  const bare = params.toString() === "" && matched === undefined;
  const chosenView = bare ? undefined : saved.views.find((view) => view.name === chosen);
  const opened =
    matched !== undefined && (chosenView === undefined || isSavedViewChanged(params, chosenView));
  const savedView = opened ? matched : chosenView;
  // A saved view its address opened is named in the history entry too, so it stays the open tab
  // as the reader changes it.
  const openedName = opened ? matched.name : undefined;
  useEffect(() => {
    if (openedName !== undefined)
      navigate(addressPath(here.pathname, params, here.hash), {
        state: { [SAVED_VIEW_STATE]: openedName },
      });
  }, [here.hash, here.pathname, navigate, openedName, params]);
  // Whether the last move was a press on a view tab, which `openView` pushes.
  const tabPressed = useRef(false);
  const openView = (address: URLSearchParams, name: string | undefined, push: boolean) => {
    tabPressed.current = push;
    // A press on the tab of the view open now, at the address it is on, writes over its entry,
    // so the press leaves no second entry at the same address.
    const same = address.toString() === params.toString() && name === savedView?.name;
    navigate(addressPath(here.pathname, address), {
      replace: !push || same,
      state: name === undefined ? undefined : { [SAVED_VIEW_STATE]: name },
    });
  };
  const base = viewFor(expandView(params).get("view"));
  // The address the open view opens at, and what it narrows by of itself: one of each while the
  // open view stays, so a render of the view parses neither again.
  const savedQuery = savedView?.query;
  const ownAddress = useMemo(
    () => (savedQuery === undefined ? builtInAddress(base.id) : copyAddress(savedQuery)),
    [savedQuery, base.id]
  );
  const own = useMemo(() => ownNarrowings(ownAddress), [ownAddress]);
  // One object while the open view stays, so the tab counts, which read it, are worked out again
  // only when it changes.
  const savedName = savedView?.name;
  const active = useMemo<ActiveView>(
    () =>
      savedName === undefined
        ? { kind: "built-in", id: base.id }
        : { kind: "saved", name: savedName },
    [savedName, base.id]
  );
  // A press on a tab that opens another built-in view draws new tabs in place of the pressed one
  // (the view's key below), and the focus goes with it: the open view's new tab takes it, so the
  // keyboard stays on the tabs.
  useLayoutEffect(() => {
    if (!tabPressed.current) return;
    tabPressed.current = false;
    if (focusOnDocument()) document.getElementById(viewTabButtonId(active))?.focus();
  });
  const tableKey = tableStorageKey(project);
  const [layout, setLayout] = useViewLayout(tableKey, active);
  const [showIcebox, setShowIcebox] = useUserPreference(
    "project.board-edges",
    (stored) => stored === "shown",
    (shown) => (shown ? "shown" : "hidden")
  );
  const [lanes, setLanes] = useUserPreference(
    "project.board-lanes",
    (stored) => stored === "shown",
    (shown) => (shown ? "shown" : "hidden")
  );
  useKeymap("project", [
    {
      id: "toggle-view",
      keys: "v",
      label: "Toggle Table / Board",
      run: () => setLayout(layout === "board" ? "table" : "board"),
    },
  ]);
  const [heldDone, holdDone] = useHeldDone();
  // The bulk actions and their recovery, above the view's key below: a press on another view
  // draws a new table, and a move the old one began runs on.
  const actions = useIssueTableActions({ project });
  if (daemon.isPending) return <LoadingSkeleton label="Loading issues" />;
  const hasLegion = daemon.data !== undefined && daemon.data !== null;
  const daemonFailure =
    daemon.isError && daemon.data === undefined ? readFailure(daemon) : undefined;
  const tab: IssuesTabPlace = {
    project,
    hasLegion,
    daemonFailure,
    base,
    active,
    changed:
      savedView === undefined ? isViewChanged(params) : isSavedViewChanged(params, savedView),
    ownAddress,
    own,
    saved,
    openView,
    layout,
    setLayout,
    toggles: { showIcebox, setShowIcebox, lanes, setLanes },
    heldDone,
    holdDone,
  };
  // One view per view's stored layout, and per daemon answer: the columns a view opens with,
  // and the key its widths and order are kept under, are read once as the table mounts.
  return (
    <IssuesTabView
      actions={actions}
      key={`${viewLayoutKey(tableKey, base.id)}|${hasLegion}`}
      tab={tab}
    />
  );
}

/**
 * One view of the Issues tab: its rows as `useTabReads` reads them, in the table, laid out by the
 * view's columns (`useTabTableLayout`), or on the Board, under the view tabs with their counts
 * (`useTabCounts`). The table and the Board share one table model (`useIssueTable`), whose
 * selection goes with the layout, and one rule for their chips (`viewChips`).
 */
function IssuesTabView({
  actions,
  tab,
}: {
  actions: IssueTableActions;
  tab: IssuesTabPlace;
}): ReactNode {
  const address = useTableAddress(PROJECT_GRAMMAR);
  const update = useAddressUpdate();
  const { params, view } = address;
  const { hasLegion, own, project } = tab;
  const { columnsLayout, layout } = useTabTableLayout(address, tab);
  const { flags, panel, userState, viewer } = useIssueTableReads(view.filters);
  // The trees an Evals link leaves out (`exclude_tree`), a narrowing of the Issues tab alone: a
  // row is in a tree when it is the tree's root or its facts name that root.
  const [excluded, keepTree] = useExcludedTrees();
  const onBoard = tab.layout === "board";
  const reads = useTabReads({
    address,
    excluded,
    flags,
    hasLegion,
    onBoard,
    project,
    registry: panel.registry,
    userState,
    viewer,
  });
  const { board, done, doneRead, doneTotal, issues, items: factsRead, readsOpen } = reads;
  const phases = legionPhasesOf(board.data);
  const state = useIssueTable({
    actions,
    address,
    allItems: reads.allItems,
    columnsLayout,
    counted: reads.counted,
    excluded,
    flags,
    layout,
    listingIn: reads.listingIn,
    loaded: reads.loaded,
    mode: { kind: "tab", hasLegion, phases },
    onBoard,
    panel,
    serverRowCount: doneTotal,
    userState: userState.data,
    viewKey: reads.viewKey,
  });
  const { listedRows, table } = state;
  // Each tree left out, as a chip of the control row and a line of the Filters panel, each with
  // the remove that gives its rows back.
  const excludedChips = useMemo<FilterChip[]>(
    () =>
      excluded.map((tree) => ({
        key: `exclude_tree:${tree}`,
        label: `Not in tree ${tree}`,
        remove: () => keepTree(tree),
      })),
    [excluded, keepTree]
  );
  const counts = useTabCounts({
    active: tab.active,
    boardIn: !hasLegion || board.data !== undefined,
    done,
    heldDone: tab.heldDone,
    holdDone: tab.holdDone,
    items: reads.allItems,
    // A server's page of done rows counts its total, which is every row it lists.
    listedCount: reads.counted ? (doneTotal ?? listedRows.length) : undefined,
    openIn: readsOpen && issues.data !== undefined,
    project,
    readsOpen,
    saved: tab.saved.views,
    userState: userState.data,
  });
  const narrowed = narrowingsDiffer(params, tab.ownAddress);
  const clearView = () => update((current) => clearNarrowings(current, tab.ownAddress));

  const header = board.data?.header;
  const tabs = (
    <IssueViewTabs
      active={tab.active}
      boardFailure={hasLegion && board.isError ? readFailure(board) : undefined}
      changed={tab.changed}
      counts={counts.built}
      daemonFailure={tab.daemonFailure}
      daemonReadError={
        hasLegion && header !== undefined && header.read_at !== null && !header.read_ok
          ? (header.read_error ?? "")
          : undefined
      }
      factsFailure={hasLegion && factsRead.isError ? readFailure(factsRead) : undefined}
      onDelete={(saved) => {
        tab.saved.remove(saved);
        tab.openView(params, undefined, false);
      }}
      onSelect={(id) => tab.openView(builtInAddress(id), undefined, true)}
      onSelectSaved={(saved) => {
        const entry = tab.saved.views.find((one) => one.name === saved);
        if (entry !== undefined) tab.openView(copyAddress(entry.query), entry.name, true);
      }}
      saved={tab.saved.views}
      savedCounts={counts.saved}
      views={BUILT_IN_VIEWS}
    />
  );
  // Save view, in the control row after the Columns menu: the view as the address has it, with
  // the columns it shows and its layout, under a name the reader gives. The (i) hint follows it,
  // in a project with a Legion daemon.
  const active = tab.active;
  const viewControls = (
    <ViewControls
      changed={tab.changed}
      check={tab.saved.check}
      initialName={active.kind === "saved" ? active.name : ""}
      onSave={(saving) => {
        const query = savedViewQuery(params, {
          cols: table.getVisibleLeafColumns().map((column) => column.id),
          layout: tab.layout,
        });
        const answer = tab.saved.save(saving, query);
        if (answer.kind === "problem") return;
        tab.openView(copyAddress(query), answer.name, false);
      }}
    >
      {hasLegion ? <DaemonHint header={header} reading={board.isPending} /> : null}
    </ViewControls>
  );
  const layoutSwitch = <LayoutSwitch layout={tab.layout} onLayout={tab.setLayout} />;
  // What the listing reads say over the rows, in both layouts, once the rows are in. Done's page
  // read that failed on an answer the server did not sort says so in place of a failed read's
  // words, and keeps its Retry.
  const notices = reads.loaded ? (
    <ListingNotices
      closed={done !== undefined}
      closedPending={done !== undefined && readsOpen && doneRead.isPending}
      error={reads.listingError}
      errorText={reads.unsorted ? UNSORTED_DONE_PAGE : undefined}
    />
  ) : null;
  const readStates = (
    <ReadStates
      failure={
        reads.failed
          ? {
              text: reads.unsorted ? UNSORTED_DONE_PAGE : ISSUES_NOT_LOADED,
              retry: () => {
                if (readsOpen) void issues.refetch();
                else void doneRead.refetch();
                void userState.refetch();
              },
            }
          : undefined
      }
      loading={reads.loading}
    >
      {reads.legionFailed ? (
        reads.boardWaits && board.isError ? (
          <QueryError
            message="Could not read the Legion board, which this view needs."
            onRetry={() => void board.refetch()}
            retrying={board.isFetching}
          />
        ) : (
          <QueryError
            message="Could not read the Legion columns, which this view needs."
            onRetry={() => void factsRead.refetch()}
            retrying={factsRead.isFetching}
          />
        )
      ) : null}
    </ReadStates>
  );

  if (onBoard)
    return (
      <IssuesTabBoard
        address={address}
        allItems={reads.allItems}
        excluded={excludedChips}
        flags={flags}
        hasLegion={hasLegion}
        header={tabs}
        layoutSwitch={layoutSwitch}
        listsDone={done !== undefined}
        loaded={reads.loaded}
        name={NAME}
        notices={notices}
        onClear={narrowed ? clearView : undefined}
        own={own}
        phases={phases}
        project={project}
        readStates={readStates}
        state={state}
        toggles={tab.toggles}
        userState={userState.data}
        viewControls={viewControls}
        viewKey={reads.viewKey}
      />
    );

  // The bar of the rows the table lists by status, once they are in; a server's page of done
  // rows counts its total, which is every row it lists. A press on a legend entry narrows the
  // view to that status, and a second press gives the view's own statuses back.
  const bar =
    reads.loaded && tab.base.bar && reads.counted ? (
      <IssueStatusBar
        counts={
          doneTotal === undefined
            ? countStatuses(listedRows.map((row) => row.original.issue.status))
            : new Map([["done", doneTotal]])
        }
        narrowTo={(status) => {
          const column = table.getColumn("status");
          if (status !== undefined) {
            column?.setFilterValue([status]);
            return;
          }
          const expanded = expandView(tab.ownAddress);
          column?.setFilterValue(
            expanded.get("nofilter") === "1" ? undefined : expanded.getAll("status")
          );
        }}
        picked={reads.statusPicks}
      />
    ) : null;
  return (
    <IssueTableFrame
      actions={actions}
      closedUnloaded={!reads.doneLoaded}
      controls={
        <>
          {layoutSwitch}
          <ColumnsMenu columns={table.getAllLeafColumns()} size="toolbar" />
          {viewControls}
        </>
      }
      header={tabs}
      legionQueue
      loaded={reads.loaded}
      name={NAME}
      narrowings={excludedChips}
      notices={notices}
      onClear={narrowed ? clearView : undefined}
      own={own}
      readStates={readStates}
      rowActions
      state={state}
      strip={bar}
    />
  );
}
