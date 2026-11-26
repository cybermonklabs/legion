import { useQuery } from "@tanstack/react-query";
import type { FilterFn, GroupingState, Row, RowSelectionState, Table } from "@tanstack/react-table";
import { type RefObject, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { userStateQuery, whoAmIQuery } from "../../api/queries";
import type { UserState } from "../../api/types";
import { type DataTableModel, type DataTableNames, useDataTable } from "../../components/DataTable";
import type { FilterChip } from "../../components/filter-chips";
import type { LayoutColumns, TableLayoutModel } from "../../components/table-layout";
import {
  addressPath,
  copyAddress,
  type TableAddress,
  useAddressNavigate,
  useNavigationState,
  usePageAddress,
} from "../../lib/page-address";
import { type AgentRegistry, useAgentRegistry } from "../refs/agent-registry";
import { setSelectedIssues } from "../search/bar-scope";
import { type IssueFiltersState, useIssueFilters } from "./issue-filters";
import type { IssueTableActions } from "./issues-table-actions";
import { type IssueTableMode, useIssueTableColumns } from "./issues-table-columns";
import {
  clearPicked,
  compareGroups,
  facetCounts,
  groupValue,
  ISSUE_GROUP_ATTRIBUTE,
  ISSUE_LINK_ATTRIBUTE,
  ISSUE_ROW_ATTRIBUTE,
  type Item,
  matchesSearch,
  NOBODY_FACET,
  UNCLAIMED_FACET,
  type View,
} from "./issues-table-model";
import { inExcludedTree } from "./issues-views";
import { issueIsUnread } from "./UnreadDot";

const EMPTY_FACETS: ReadonlyMap<string, number> = new Map();
const fuzzy: FilterFn<Item> = (row, _id, query: string) => matchesSearch(row.original, query);

/** The table's grouping on the Board, whose Group by picks the Board's columns instead: its rows
 *  are then the view's rows in the view's sort alone. */
const NO_GROUPING: GroupingState = [];
const NO_PICKS: readonly string[] = [];

/** The List's own words, which the table core reads and never invents: the attributes its rows
 *  and groups carry, the link opening a row follows, and the keys that walk it, under the ids
 *  and labels the palette and the shortcut help have always shown. */
const ISSUE_TABLE_NAMES: DataTableNames = {
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

/** What a column filter of `filters` picks, as strings; none where it has no such filter. */
export function picksOf(filters: View["filters"], id: string): readonly string[] {
  const value = filters.find((filter) => filter.id === id)?.value;
  return Array.isArray(value) ? value.map(String) : NO_PICKS;
}

/** What a view narrows its rows by beside its filters and its search: the issues a link names,
 *  the Needs you and Unread flags, the reader's read state that Unread reads, and the trees it
 *  leaves out. */
export interface Narrowings {
  readonly selection: View["selection"];
  readonly needsYou: boolean;
  readonly unread: boolean;
  readonly userState: UserState | undefined;
  readonly excluded: readonly string[];
}

/** Whether `item` is a row that every one of `narrowings` lists. The table's rows and the tab
 *  counts taken from the open listing (`openListingCount`) both narrow by it. */
export function passesNarrowings({ facts, issue }: Item, narrowings: Narrowings): boolean {
  const { excluded, needsYou, selection, unread, userState } = narrowings;
  return (
    (selection === null || selection.keys.has(issue.key)) &&
    (!needsYou || issue.open_asks > 0) &&
    (!unread || issueIsUnread(issue, userState?.[issue.key]?.last_read_seq ?? 0)) &&
    !inExcludedTree(issue.key, facts?.root_key, excluded)
  );
}

/** The column filter whose panel stands open, and the agent registry the Claimed by filter
 *  reads to tell which claims have lapsed. */
export interface FilterPanel {
  readonly openFilter: string | undefined;
  readonly setOpenFilter: (id: string | undefined) => void;
  readonly registry: AgentRegistry;
  /** Whether the Claimed by filter picks Nobody or lapsed (`UNCLAIMED_FACET`). */
  readonly wantsFree: boolean;
  /** Why the Claimed by filter cannot tell which claims have lapsed, while it is asked to. */
  readonly unclaimedUnchecked: "refilling" | "unreachable" | undefined;
}

/** The open column filter of a table whose view holds `filters`. Which claims have lapsed is the
 *  agent registry's answer, read only while the Claimed by filter asks for it: because it is
 *  picked, or because its panel is open and counting. A table's rows are built against it, so
 *  it is held above the read of the rows. */
export function useFilterPanel(filters: View["filters"]): FilterPanel {
  const [openFilter, setOpenFilter] = useState<string>();
  const wantsFree = picksOf(filters, "claim").includes(UNCLAIMED_FACET);
  const registry = useAgentRegistry(wantsFree || openFilter === "claim");
  let unclaimedUnchecked: FilterPanel["unclaimedUnchecked"];
  if (wantsFree && registry.isError)
    unclaimedUnchecked = registry.refilling ? "refilling" : "unreachable";
  return { openFilter, setOpenFilter, registry, wantsFree, unclaimedUnchecked };
}

/** What every issue table reads as it opens, beside its rows: the project page's flags
 *  (`useIssueFilters`), the reader's read state, the signed-in login, and the open column filter
 *  of a view that holds `filters` (`useFilterPanel`). */
export function useIssueTableReads(filters: View["filters"]) {
  const flags = useIssueFilters();
  const userState = useQuery(userStateQuery());
  const viewer = useQuery(whoAmIQuery()).data?.login;
  const panel = useFilterPanel(filters);
  return { flags, userState, viewer, panel };
}

/** What every issue table holds over the rows its listing answered, as `useIssueTable` gives it. */
export interface IssueTableState {
  readonly model: DataTableModel<Item>;
  readonly table: Table<Item>;
  /** The rows the table lists under the filters and the search it is showing, in row order. */
  readonly listedRows: readonly Row<Item>[];
  /** The issue the reader marked last, by its own checkbox or `x` on its row. */
  readonly lastMarked: RefObject<string | null>;
  /** The chip of the issues an address names with `only=`, where it names any. */
  readonly pickedChip: FilterChip | undefined;
  readonly panel: FilterPanel;
  /** What each value of the open column filter would list (`facetCounts`). */
  readonly openFacets: ReadonlyMap<string, number>;
  /** Whether a header filter draws the count of `value` of the column `column`: once every read
   *  it counts has answered, and a read it does not count never hides it. Every count counts the
   *  rows the table waits on (`counted`), as the `only=` chip does: never the open issues alone
   *  while the closed ones it asked for are out. While a Nobody-or-lapsed narrowing is on and the
   *  agent registry's first read is out, the listed rows can still change when it answers, so no
   *  column draws a count. Once the registry answers or settles on its error (where the page
   *  shows `unclaimed-unchecked` and counts every claim as held), each count the registry does
   *  not decide draws: every other column, and Claimed by's Nobody. Every other Claimed by value
   *  - Nobody or lapsed, and each holder - still waits until the registry's latest read has
   *  succeeded (`known`), since only it decides which claims have lapsed (`claimIsTakeable`). */
  readonly filterCounted: (column: string, value: string) => boolean;
  /** Starts `keys` next, over the rows the table holds now (`IssueTableActions.startNext`). */
  readonly startNext: (keys: readonly string[], confirmFirst?: boolean) => void;
  /** Finishes the action a tab left behind, and clears this table's selection once it has. */
  readonly finish: () => void;
}

/**
 * What every issue table holds over the rows its listing answered, whatever page draws it: its
 * selection, the rows its narrowings list, its columns and its `DataTable` model, and the
 * selection kept to the rows it lists. `allItems` is every row the listing answered;
 * `listingIn` says every listing the view reads has answered, `loaded` that the rows are drawn,
 * and `counted` that the rows the table waits on are in, which the `only=` chip counts by.
 * `onBoard` says the Issues tab draws the Board in the table's place, which takes the selection
 * and the table's keys with it. `actions` are the table's bulk actions, which run above it.
 */
export function useIssueTable({
  actions,
  address,
  allItems,
  columnsLayout,
  counted,
  excluded,
  flags,
  layout,
  listingIn,
  loaded,
  mode,
  onBoard,
  panel,
  serverRowCount,
  userState,
  viewKey,
}: {
  actions: IssueTableActions;
  address: TableAddress<View>;
  allItems: readonly Item[];
  /** The columns the table has, their widths and their minimums. */
  columnsLayout: LayoutColumns;
  counted: boolean;
  /** The trees the view leaves out (`exclude_tree`), a narrowing of the Issues tab alone. */
  excluded: readonly string[];
  flags: IssueFiltersState;
  layout: TableLayoutModel;
  listingIn: boolean;
  loaded: boolean;
  mode: IssueTableMode;
  onBoard: boolean;
  panel: FilterPanel;
  /** The count of every row of a server's page of rows, which the table pages by. */
  serverRowCount: number | undefined;
  userState: UserState | undefined;
  /** The view the reader picked, and whether its first draw is whole (`DataTableOptions`). */
  viewKey: string;
}): IssueTableState {
  const { view, setFilters, setGrouping, setPagination, setSearch, setSorting } = address;
  const { sorting, grouping, filters, search } = view;
  const pagination = { pageIndex: view.page, pageSize: view.pageSize };
  const pageAddress = usePageAddress();
  const navigate = useAddressNavigate();
  // What the navigation to this entry carried, which the `only=` chip's remove carries on.
  const entryState = useNavigationState();
  const [selection, setSelection] = useState<RowSelectionState>({});
  const clearSelection = () => setSelection({});
  // The issue the reader marked last - by its own checkbox, or `x` on its row: where the focus
  // goes when the bar goes with it (`IssueTableFrame`). A select-all marks no row of the
  // reader's own, so it records none.
  const lastMarked = useRef<string | null>(null);
  const recordMark = useCallback((key: string) => {
    lastMarked.current = key;
  }, []);
  // The selection is the table's: the Board in its place takes it with the table, as leaving the
  // page does, and the table drawn again over the Board comes back with no row selected.
  const [selectionLayout, setSelectionLayout] = useState(onBoard);
  if (selectionLayout !== onBoard) {
    setSelectionLayout(onBoard);
    setSelection({});
  }

  // The issues an address names with `only=`, which the table shows alone until its chip is
  // removed. The narrowing is the reader's, so it outranks nothing: every other filter still
  // applies to what is left.
  const picked = view.selection;
  const items = useMemo(() => {
    const narrowings: Narrowings = {
      selection: picked,
      needsYou: flags.needsYou,
      unread: flags.unread,
      userState,
      excluded,
    };
    return allItems.filter((item) => passesNarrowings(item, narrowings));
  }, [allItems, excluded, flags.needsYou, flags.unread, picked, userState]);
  // What the table is narrowed to, and the way back to every row: removing the chip clears
  // `only=` and leaves every other control where the reader set it. It is a chip of the control
  // row, beside the flags' and the columns', drawn by the same `filterChips` and named as they
  // are, `Remove Showing: <title> filter`, and its removal leaves a history entry, so Back gives
  // the narrowing back (`useAddressNavigate`'s `replace: false`). That entry carries what the
  // entry before it carried, as every write of the address does, so a saved view open on the
  // Issues tab stays open. The toolbar is drawn in every state, so the chip counts what it shows
  // only once the rows the table waits on are in (`counted`): not beside the loading skeleton,
  // not after a failed read, and not while the closed issues it asked for are still out, where
  // it would count rows the table has not read. Until then it names its narrowing alone.
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
              addressPath(
                pageAddress.pathname,
                clearPicked(copyAddress(pageAddress.search)),
                pageAddress.hash
              ),
              { replace: false, state: entryState }
            ),
        };

  const columns = useIssueTableColumns(
    userState,
    layout.responsive.compact,
    recordMark,
    columnsLayout,
    sorting,
    mode
  );
  // The headings' order is read once, when the table is made (`groupOrder`), so it reads the
  // order each column's filter lists its values in from the columns of the latest draw: Legion
  // phase lists the board's columns (`IssueTableMode`), which a later board may change.
  const drawnColumns = useRef(columns);
  drawnColumns.current = columns;
  const model = useDataTable<Item>({
    rows: items,
    columns,
    getRowId: (item) => item.issue.key,
    layout,
    names: ISSUE_TABLE_NAMES,
    // The Board's own keys answer while it shows, and its help lists no key of the table it hides.
    keys: !onBoard,
    state: {
      sorting,
      grouping: onBoard ? NO_GROUPING : grouping,
      columnFilters: filters,
      globalFilter: search,
      pagination,
    },
    onSortingChange: setSorting,
    onGroupingChange: setGrouping,
    onColumnFiltersChange: setFilters,
    onGlobalFilterChange: setSearch,
    onPaginationChange: setPagination,
    viewKey,
    selection: { state: selection, onChange: setSelection, onMark: recordMark },
    search: { column: "title", filterFn: fuzzy },
    groupOrder: {
      compare: (id, a, b) =>
        compareGroups(
          id,
          a,
          b,
          drawnColumns.current.find((column) => column.id === id)?.meta?.order
        ),
      heading: groupValue,
    },
    // A server's page: the table pages by the count of every row it holds.
    ...(serverRowCount === undefined ? {} : { serverPage: { rowCount: serverRowCount } }),
  });
  const { table } = model;

  // The keys the table lists under the filters and the search it is showing, in row order. A key
  // it no longer lists drops out of the selection - another filter, another project - so a row
  // that is listed again comes back unselected; this is the rule `hooks/useMarkedKeys.ts` holds
  // the Inbox's marks to, written against the table's own `rowSelection` rather than a second
  // selection beside it. Joined into a string, so the pruning runs when the keys change rather
  // than on every render that rebuilds the rows, and `undefined` while the listing is still out,
  // which prunes nothing.
  const listedRows = table.getFilteredRowModel().rows;
  const listedKeys = !listingIn ? undefined : listedRows.map((row) => row.id).join(" ");
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
  const selectedSignature = listedRows
    .filter((row) => row.getIsSelected())
    .map((row) => row.id)
    .join(" ");
  useEffect(() => {
    setSelectedIssues(selectedSignature === "" ? [] : selectedSignature.split(" "));
    return () => setSelectedIssues([]);
  }, [selectedSignature]);

  const pageCount = table.getPageCount();
  useEffect(() => {
    if (listingIn && pagination.pageIndex >= Math.max(pageCount, 1))
      table.setPageIndex(Math.max(pageCount - 1, 0));
  }, [listingIn, pagination.pageIndex, pageCount, table]);

  // A column filter's panel hangs off its header and goes with the table: it is closed then, as
  // a panel whose anchor has left the page closes (`AnchoredPanel`), so the table coming back
  // does not open it again and take the focus from wherever it went meanwhile.
  const { openFilter, setOpenFilter } = panel;
  useEffect(() => {
    if (!loaded) setOpenFilter(undefined);
  }, [loaded, setOpenFilter]);
  const openFacets = useMemo(
    () => (openFilter ? facetCounts(items, openFilter, filters, search) : EMPTY_FACETS),
    [items, openFilter, filters, search]
  );
  const registryPending = panel.wantsFree && !panel.registry.known && !panel.registry.isError;
  const filterCounted = (column: string, value: string) =>
    counted &&
    !registryPending &&
    (column !== "claim" || value === NOBODY_FACET || panel.registry.known);

  return {
    model,
    table,
    listedRows,
    lastMarked,
    pickedChip,
    panel,
    openFacets,
    filterCounted,
    startNext: (keys, confirmFirst) =>
      actions.startNext(keys, { items: allItems, clearSelection }, confirmFirst),
    finish: () => actions.finish(clearSelection),
  };
}
