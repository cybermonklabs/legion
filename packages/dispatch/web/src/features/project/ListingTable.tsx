import type { ReactNode } from "react";
import type { FilterChip } from "../../components/filter-chips";
import { useTableLayout } from "../../components/table-layout";
import { useAddressUpdate } from "../../lib/page-address";
import { ISSUES_NOT_LOADED, useIssueTableActions } from "./issues-table-actions";
import { LISTING_OWN } from "./issues-table-chips";
import {
  type IssueTableColumnId,
  type IssueTableWidths,
  issueTableLayout,
  listingColumns,
  ROW_ACTIONS_WIDTH,
} from "./issues-table-columns";
import {
  type AdvancesListing,
  answeredWhole,
  listingsOf,
  listsQueued,
  useAdvancesTableItems,
} from "./issues-table-data";
import { IssueTableFrame, ListingNotices, ReadStates } from "./issues-table-frame";
import {
  defaultView,
  narrowsNothing,
  readUrl,
  tableStorageKey,
  writeUrl,
} from "./issues-table-model";
import { IssueTableStatus } from "./issues-table-page";
import { picksOf, useIssueTable, useIssueTableReads } from "./issues-table-state";
import { useTableView } from "./issues-table-view";

/** The one layout key of every table scoped to a priority that names no layout of its own: the
 *  work advancing a priority is one table wherever it is drawn, so the columns a reader drags
 *  there are dragged once rather than once per priority. A priority's own page names its own
 *  layout (`priorities/PriorityPage.tsx`). */
const SCOPED_LAYOUT_KEY = "priority-work";

/** A listing table leaves out no tree: `exclude_tree` is a narrowing of the Issues tab alone. */
const NO_TREES: readonly string[] = [];
const NO_CHIPS: readonly FilterChip[] = [];

/**
 * The issue table a priority page draws over a listing of the issues that advance a priority,
 * which belong to no project (`AdvancesListing`), as the List was drawn before the views: its
 * view under its own prefix in the address (`stateKey`), its status strip over it, and no view
 * tabs, no Legion column and no Board. It reads its open issues, and its whole listing once the
 * reader lets closed issues through.
 */
export function ListingTable({
  empty,
  label,
  layout: ownLayout,
  listing,
  priorityTitles,
  stateKey,
}: {
  /** What the table says when it lists no row and the reader narrowed it by nothing - no filter
   *  but the statuses it opens with, no search, no issues picked (`narrowsNothing`), and neither
   *  Needs you nor Unread: that its listing holds no issue, in its caller's words. Unset, or once
   *  the reader narrowed it, it says no issues match these filters. */
  empty?: string;
  /** What this list is called, for a reader who cannot see where it sits; "Priority issues"
   *  where unset. */
  label?: string;
  /** The layout this table keeps, under its own key, where it is not every priority's one: the
   *  whole set of columns it opens with shown (`opens`), registry and listing columns alike,
   *  every other one opening hidden for the reader to show, the widths it gives any column in
   *  place of its own, or the columns it makes as wide as what they hold (`widths`), and the
   *  columns it pins at the left of a desktop's box in place of the registry's (`pins`,
   *  `issueTableLayout`). Given one, Title fills a desktop box: it takes what the other shown
   *  columns and the row's actions leave (`fills`). `rowActions: false` draws no row's actions
   *  after the columns, no cell for a row's Start next, and keeps no width for them, as on a
   *  listing that holds no issue in Legion's queue (`listsQueued`): Title takes that width too,
   *  and the selection's bar keeps Start next. `wrapTitles: true` wraps each row's title onto as
   *  many as eight lines, ending in '…' with the whole title in its tooltip where it runs
   *  longer, where a table clamps it to two lines that end in '…': a table read by its titles,
   *  as each fault list of the priorities page and a priority's work table are.
   *  `statusLegend: true` draws each segment of the status bar as its count alone where the
   *  segments with their names are wider than the bar, and the names in a legend under it
   *  (`IssueTableStatus`), so no name is cut at the bar's edge: each of those tables stands in a
   *  box narrower than a project's, whose bar keeps its names and scrolls sideways on a phone.
   *  Each of those gives its own; unset, the table opens as today's. */
  layout?: {
    readonly key: string;
    readonly opens: readonly IssueTableColumnId[];
    readonly widths?: IssueTableWidths;
    readonly rowActions?: boolean;
    readonly pins?: readonly IssueTableColumnId[];
    readonly wrapTitles?: boolean;
    readonly statusLegend?: boolean;
  };
  listing: AdvancesListing;
  /** Each priority's title by its id, in the one order the priorities read answers them in.
   *  Given, the table has the Advances column, which names each priority a row advances by its
   *  place in that order and its title (`#3`, `PriorityChip`); unset, it has none
   *  (`listingColumns`). */
  priorityTitles?: ReadonlyMap<string, string>;
  /** The prefix this table's own view takes in the address: `<stateKey>.sort` and the rest. */
  stateKey: string;
}): ReactNode {
  const address = useTableView(stateKey);
  const { params, view } = address;
  // Whether the table draws a row's actions after its columns - the cell for its Start next -
  // and the width it draws them at: where the listing can hold a row a reader starts next
  // (`listsQueued`) and its layout does not leave them out (`rowActions`). A fault list of the
  // priorities page holds no such row and a priority's work table leaves them out, so neither
  // draws them.
  const rowActions = listsQueued(listing) && ownLayout?.rowActions !== false;
  // The columns this table has - the listing columns its listing gives a use (`listingColumns`)
  // beside the registry's - which it opens with and at which widths.
  const columnsLayout = issueTableLayout(
    listingColumns(listing, priorityTitles !== undefined),
    ownLayout?.opens,
    ownLayout?.widths,
    rowActions ? ROW_ACTIONS_WIDTH : 0,
    ownLayout?.pins
  );
  // One stored layout per layout a caller names, and one for every other scoped table: the work
  // advancing any priority is one table, so a reader who drags a column on it drags it for every
  // priority, and not once per priority.
  const layout = useTableLayout(
    tableStorageKey(ownLayout?.key ?? SCOPED_LAYOUT_KEY),
    columnsLayout,
    view.sorting
  );
  const { flags, panel, userState, viewer } = useIssueTableReads(view.filters);
  // A listing its open read answers whole reads no closed issue, even where the Status filter
  // picks Done; any other reads its whole listing once the Status filter lets Done through.
  const whole = answeredWhole(listing);
  const closed = !whole && listingsOf(picksOf(view.filters, "status")).done;
  const rows = useAdvancesTableItems(listing, { closed, registry: panel.registry, viewer });
  const { issues, listingError, wholeIn } = rows;
  const actions = useIssueTableActions(listing);
  const loading = issues.isPending || userState.isPending;
  const failed = !loading && ((issues.isError && issues.data === undefined) || userState.isError);
  const loaded = !loading && !failed;
  const closedIn = !closed || wholeIn;
  const state = useIssueTable({
    actions,
    address,
    allItems: rows.allItems,
    columnsLayout,
    counted: loaded && listingError === undefined && closedIn,
    excluded: NO_TREES,
    flags,
    layout,
    listingIn: issues.data !== undefined && closedIn,
    loaded,
    // A listing table draws today's cells at today's widths (`useIssueTableColumns`).
    mode: { kind: "embedded", priorityTitles, wrapTitles: ownLayout?.wrapTitles },
    onBoard: false,
    panel,
    serverRowCount: undefined,
    userState: userState.data,
    viewKey: params.toString(),
  });
  const update = useAddressUpdate();
  // Clear filters shows while the table has a column filter, a search or one of the two flags,
  // which it takes out of the address.
  const narrowed = view.filters.length > 0 || view.search !== "" || flags.needsYou || flags.unread;
  const clearFilters = () =>
    update((current) => {
      const next = writeUrl(
        current,
        {
          ...readUrl(current, stateKey),
          // Back to the view the table opens with, not to no filter at all: an empty status pick
          // asks the server for every closed issue in the project.
          filters: defaultView.filters,
          search: "",
          page: 0,
        },
        stateKey
      );
      for (const flag of ["needs-you", "unread"]) next.delete(flag);
      return next;
    });
  return (
    <IssueTableFrame
      actions={actions}
      closedUnloaded={!whole && !wholeIn}
      empty={
        empty !== undefined && narrowsNothing(view) && !flags.needsYou && !flags.unread
          ? empty
          : undefined
      }
      legionQueue={false}
      loaded={loaded}
      name={label ?? "Priority issues"}
      narrowings={NO_CHIPS}
      notices={
        loaded ? (
          <ListingNotices
            closed={closed}
            closedPending={closed && rows.whole.isPending}
            error={listingError}
          />
        ) : null
      }
      onClear={narrowed ? clearFilters : undefined}
      own={LISTING_OWN}
      readStates={
        <ReadStates
          failure={
            failed
              ? {
                  text: ISSUES_NOT_LOADED,
                  retry: () => {
                    void issues.refetch();
                    void userState.refetch();
                  },
                }
              : undefined
          }
          loading={loading}
        />
      }
      rowActions={rowActions}
      state={state}
      // Its strip of every status the table holds, a press showing only that status and a second
      // press every status.
      strip={
        loaded ? (
          <IssueTableStatus
            items={rows.allItems}
            legend={ownLayout?.statusLegend}
            table={state.table}
          />
        ) : null
      }
    />
  );
}
