import { type ReactNode, useRef } from "react";
import { DataTable, type RowNoun } from "../../components/DataTable";
import type { FilterChip } from "../../components/filter-chips";
import { ARM_ON_PRESS, useFocusPlace } from "../../components/focus-place";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import type { PagerOptions } from "../../components/Pager";
import { useLastMarkedPlace } from "../../components/SelectionBar";
import { TABLE_FRAME_MARK } from "../../components/table-keys";
import {
  borderDefault,
  dangerText,
  focusVisibleRing,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { ConfirmDialog } from "./ConfirmDialog";
import type { IssueTableActions } from "./issues-table-actions";
import type { OwnNarrowings } from "./issues-table-chips";
import { ROW_ACTIONS_WIDTH } from "./issues-table-columns";
import { ALL_ROWS, ISSUE_ROW_ATTRIBUTE, PAGE_SIZES } from "./issues-table-model";
import { IssueTableSelection, IssueTableToolbar } from "./issues-table-page";
import { GroupRow, IssueRow } from "./issues-table-rows";
import type { IssueTableState } from "./issues-table-state";

/** The checkbox a row draws for its selection (`issues-table-columns.tsx`'s select column), the
 *  one checkbox every row has. */
const ROW_CHECKBOX = 'input[type="checkbox"]';

/** Status offers Done before the closed issues it would count are read. */
const DONE_UNLOADED: ReadonlyMap<string, readonly string[]> = new Map([["status", ["done"]]]);

/** What the List's rows are, which its header filters count in and its pager counts: issues, in
 *  groups, over rows. */
const ISSUE_TABLE_NOUN: RowNoun = {
  group: { one: "group", many: "groups" },
  line: { one: "row", many: "rows" },
  many: "issues",
  one: "issue",
};

/** The line under the List: its page sizes, the one that means every row, and its test id. It
 *  counts no total: the Issues tab's count of the open view is on its tab, and the priority pages'
 *  tables draw none (R13). */
const ISSUE_TABLE_PAGER: PagerOptions = {
  allRows: ALL_ROWS,
  pageSizes: PAGE_SIZES,
  testId: "issues-table-pager",
  totals: false,
};

/** The headers that stack in a box under 640 px (`DataTable`'s `stackColumns`): Title's sort over
 *  its filter, and Time per phase's name over its Share and Hours switch and its filter, which
 *  side by side pass a phone's window. A table another page draws has no Time per phase. */
const STACKED_HEADERS: readonly string[] = ["title", "phase_hours"];

/** What the table says when it lists no row, unless the page words it itself (`empty`). */
const NO_MATCH = "No issues match these filters.";

/** A read that failed and can be made again, as a notice names it. */
interface Refetchable {
  readonly refetch: () => Promise<unknown>;
}

/**
 * What the listing's reads say over the rows once the rows are in, in the table and on the Board:
 * a failed read of the listing with its Retry (`error`), in `errorText` where the caller names
 * the failure itself; and the closed issues still being read beside the open ones
 * (`closedPending`). `closed` says the view reads closed issues, which both words name.
 */
export function ListingNotices({
  closed,
  closedPending,
  error,
  errorText,
}: {
  closed: boolean;
  closedPending: boolean;
  error: Refetchable | undefined;
  errorText?: string;
}): ReactNode {
  return (
    <>
      {error ? (
        <p className={dangerText} role="alert">
          {errorText ?? `Could not load ${closed ? "closed " : ""}issues.`}{" "}
          <button
            type="button"
            aria-label={closed ? "Retry loading closed issues" : "Retry loading issues"}
            onClick={() => void error.refetch()}
          >
            Retry
          </button>
        </p>
      ) : null}
      {closedPending ? (
        <p aria-live="polite" className={textMutedOnCanvas}>
          Loading closed issues…
        </p>
      ) : null}
    </>
  );
}

/**
 * What stands in place of the rows while the reads they wait on are out (`loading`) or have
 * failed (`failure`, its words and its Retry). `children` is what else a page says there: the
 * Issues tab's failed read of the Legion facts a view needs.
 */
export function ReadStates({
  children,
  failure,
  loading,
}: {
  children?: ReactNode;
  failure: { readonly text: string; readonly retry: () => void } | undefined;
  loading: boolean;
}): ReactNode {
  return (
    <>
      {loading ? <LoadingSkeleton label="Loading issues" /> : null}
      {failure ? (
        <p className={dangerText} role="alert">
          {failure.text}{" "}
          <button type="button" onClick={failure.retry}>
            Retry loading issues
          </button>
        </p>
      ) : null}
      {children}
    </>
  );
}

/**
 * An issue table as every page draws it: `header` (the Issues tab's view tabs), the listing's
 * `notices`, the `strip` of its rows by status, the toolbar, what the table's bulk actions say,
 * `readStates` in place of the rows while they are out, and once they are in the rows, the
 * selection bar and the confirm dialog.
 *
 * The toolbar is drawn in every state, at the one place in the tree, as the Board draws its
 * `controls`: a filter press that sends the List off to read issues it has not loaded - Clear
 * filters back to the open issues, which a List opened on the closed ones has never read - puts
 * the skeleton in place of the table alone, and the control pressed, with the keyboard focus on
 * it, stays where it was. Everything else here needs the listing and waits for it (`loaded`).
 * The section is the table's frame: its toolbar, box and pager, which another table's page-wide
 * keys leave to it (`TABLE_FRAME_ATTRIBUTE`).
 */
export function IssueTableFrame({
  actions,
  closedUnloaded,
  controls,
  empty,
  header,
  legionQueue,
  loaded,
  name,
  narrowings,
  notices,
  onClear,
  own,
  readStates,
  rowActions,
  state,
  strip,
}: {
  actions: IssueTableActions;
  /** The closed issues are not read yet, so the Status filter does not count Done. */
  closedUnloaded: boolean;
  /** The page's own controls in the toolbar, in the place of the columns menu. */
  controls?: ReactNode;
  /** What the table says when it lists no row and its reader narrowed it by nothing; unset, or
   *  where the reader narrowed it, it says no issues match these filters. */
  empty?: string;
  header?: ReactNode;
  /** A group heading names the next issue in the Legion queue (`GroupRow`). */
  legionQueue: boolean;
  loaded: boolean;
  /** What this list is called, for a reader who cannot see where it sits. */
  name: string;
  /** The narrowings that are no column and no flag (`exclude_tree`), with their removes. */
  narrowings: readonly FilterChip[];
  notices: ReactNode;
  /** **Clear filters**, unset while nothing is narrowed (`IssueTableToolbar`). */
  onClear: (() => void) | undefined;
  own: OwnNarrowings;
  readStates: ReactNode;
  /** Whether the table draws a row's actions after its columns - the cell for its Start next -
   *  and keeps the width for them (`ROW_ACTIONS_WIDTH`): not where its listing holds no issue
   *  in Legion's queue or its layout leaves them out. The selection's bar keeps Start next. */
  rowActions: boolean;
  state: IssueTableState;
  strip: ReactNode;
}): ReactNode {
  const { lastMarked, model, panel, table } = state;
  const { attachAbove, boxWidth } = model.layout;
  const grouping = table.getState().grouping[0] ?? "";
  // The issues this page of the table lists, in its order: its rows, not its group headings.
  const leaves = () => model.displayRows.flatMap(({ row }) => (row.getIsGrouped() ? [] : [row.id]));
  // The List's own stop: its named table, which is drawn with no row as well.
  const tableStop = () => model.layout.scrollRef.current?.querySelector("table") ?? null;
  // The bar's hand-off (`useLastMarkedPlace`): the checkbox of the issue marked last while the
  // table draws its row, out of sight or not. After a press that took that issue out of the view
  // - off the list, or out of what the filters show: a row with any part of its mark in sight at
  // its place, else the nearest before it, else the nearest after it. With no issue marked, as
  // after a select-all from the table's head: the first row the reader can see below the sticky
  // head. Each mark is shown whole at its nearest edge. `head` is a header cell: the cells stick,
  // and the `thead` does not (`DataTable`). Else the table's own stop, with no scroll. A row the
  // window has not drawn is out of their sight, so it is never where the focus lands in the place
  // of an issue that went. What the table's own actions say is leaving (`actions.leaving`): each
  // issue an action's writes are for, from the reader's press until the action and the read after
  // it are over, or until its write is refused or stopped. The hand-off waits for each such row
  // to leave the list by name, as the Inbox's does, so a landing never stands on a row the press
  // is taking away however long the write and its read take, and a focus the reader left in such
  // a row is landed once the row has gone.
  const landAtPlace = useLastMarkedPlace({
    control: (key) =>
      model.layout.scrollRef.current
        ?.querySelector(`[${ISSUE_ROW_ATTRIBUTE}="${CSS.escape(key)}"]`)
        ?.querySelector<HTMLElement>(ROW_CHECKBOX) ?? null,
    drawRow: (key, onLand) => model.focusRow(key, ROW_CHECKBOX, "nearest", onLand),
    // The box's height made current right before each landing, once the foot's pass has taken
    // away the end room main held while the bar stood at the foot: the table sizes the box to the
    // page at a `ResizeObserver`'s read, a frame after whatever changed the page, and the bar's
    // going is such a change. At 390x664 the box stood at 434 px when the bar went, from a read
    // long before, and at 414 a frame later, which left a landing shown whole in the old box below
    // the new one's edge (builds Jobs `infra-3135-infra-3135-r30q-frames` and `-r30r-frames`).
    // Fitted at the bar's going instead, at 1440x900 the box took 400 px while main still held
    // 100 px of end room for the bar, and 430 once the foot's pass had taken it away, a frame
    // after the landing.
    fit: model.layout.fitHeight,
    head: () => model.layout.scrollRef.current?.querySelector("thead th") ?? null,
    lastMarked: () => lastMarked.current,
    leaving: (key) => actions.leaving?.(key) === true,
    list: tableStop,
    order: leaves,
    shown: Object.keys(table.getState().rowSelection).length !== 0,
  });
  // The bar gone with the focus (`SelectionBar`): the hand-off above (`landAtPlace`) puts the
  // focus itself, including where the table lists the issue marked last and its window has not
  // drawn that row (`drawRow` above, through `focusRow`), each landing against the box `fit`
  // gives its height first. A table action clears the selection only once the list read after it
  // has settled (`useIssueTableActions`), so the view read here is the one the action left.
  const returnFocus = () => {
    landAtPlace.land();
    return null;
  };
  // A control of the table's head - a column filter, or the list it opened in a portal of its
  // own - goes with the table when a press in it sends the List back to its skeleton. A notice's
  // button under the toolbar goes with the notice it answers - the interrupted action's Finish
  // and Dismiss - and its focus goes to the control now at its place among the notices, else the
  // one before it. Each of these takes the focus only while the reader can see it; where none is
  // in sight, the main content's own stop takes it with no scroll (`useFocusPlace`). Where the
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
  const { busy, notice, moved, interrupted, interruptedDescription, confirming } = actions;
  const visibleColumns = table.getVisibleLeafColumns().length + (rowActions ? 1 : 0);
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
        {header}
        {notices}
        {strip}

        <IssueTableToolbar
          controls={controls}
          narrowings={narrowings}
          onClear={onClear}
          own={own}
          selection={state.pickedChip}
          table={table}
          unclaimedUnchecked={panel.unclaimedUnchecked}
        />

        {loaded ? (
          <div className="space-y-3" ref={noticesRef}>
            <p
              aria-live="polite"
              role={notice ? (notice.kind === "error" ? "alert" : "status") : undefined}
              className={`min-h-5 text-sm ${notice?.kind === "error" ? dangerText : textSecondaryOnCanvas}`}
            >
              {/* The table's own notice of what a table action did. */}
              {notice?.text}
            </p>
            {interrupted ? (
              <section aria-label="Interrupted table action" className={textSecondaryOnCanvas}>
                <p>{interruptedDescription}</p>
                <button
                  type="button"
                  onClick={state.finish}
                  className={`min-h-11 rounded-lg border px-3 text-sm ${borderDefault} ${focusVisibleRing}`}
                >
                  Finish
                </button>{" "}
                <button
                  type="button"
                  onClick={actions.dismiss}
                  className={`min-h-11 rounded-lg border px-3 text-sm ${borderDefault} ${focusVisibleRing}`}
                >
                  Dismiss
                </button>
              </section>
            ) : null}
          </div>
        ) : null}
      </div>

      {readStates}
      {loaded ? (
        <>
          <DataTable
            empty={empty ?? NO_MATCH}
            filterCounted={state.filterCounted}
            filterCounts={state.openFacets}
            filterUnloaded={closedUnloaded ? DONE_UNLOADED : undefined}
            // A reader at the top keeps the first row on screen too, on the Issues tab and on a
            // priority's page (R11).
            holdAtTop
            label={`${name} table`}
            leaving={actions.leaving}
            model={model}
            noun={ISSUE_TABLE_NOUN}
            onOpenFilterChange={panel.setOpenFilter}
            openFilter={panel.openFilter}
            pager={ISSUE_TABLE_PAGER}
            renderGroup={({ entry, index, measure }) => (
              <GroupRow
                columns={visibleColumns}
                viewportWidth={boxWidth}
                continued={entry.continued}
                index={index}
                legionQueue={legionQueue}
                measure={measure}
                show={table.getColumn(grouping)?.columnDef.meta?.show}
                shown={entry.shown}
                row={entry.row}
              />
            )}
            renderRow={({ row, index, measure }) => (
              <IssueRow
                busy={busy}
                index={index}
                measure={measure}
                moved={moved}
                onStartNext={rowActions ? (key) => state.startNext([key]) : undefined}
                row={row}
              />
            )}
            scrollTestId="issues-table-scroll"
            size="compact"
            stackColumns={STACKED_HEADERS}
            trailingWidth={rowActions ? ROW_ACTIONS_WIDTH : 0}
          />

          <IssueTableSelection
            returnFocus={returnFocus}
            table={table}
            busy={busy}
            startNext={state.startNext}
          />
          {confirming === undefined ? null : (
            <ConfirmDialog
              body={confirming.body}
              confirmLabel={confirming.confirmLabel}
              key={confirming.title}
              onCancel={actions.cancelConfirming}
              onConfirm={actions.confirmed}
              title={confirming.title}
            />
          )}
        </>
      ) : null}
    </section>
  );
}
