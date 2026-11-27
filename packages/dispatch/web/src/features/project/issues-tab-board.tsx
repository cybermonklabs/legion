import { type ReactNode, useMemo } from "react";
import type { UserState } from "../../api/types";
import type { FilterChip } from "../../components/filter-chips";
import type { TableAddress } from "../../lib/page-address";
import { type BoardBy, boardColumns, rankRefusal } from "./board-model";
import { BoardControlRow, IssueBoard } from "./IssueBoard";
import { ToggleButton } from "./IssueViewTabs";
import type { IssueFiltersState } from "./issue-filters";
import type { LegionPhaseColumn } from "./issues-legion";
import { type OwnNarrowings, viewChips } from "./issues-table-chips";
import type { Item, View } from "./issues-table-model";
import { type IssueTableState, picksOf } from "./issues-table-state";

/** The Board's own toggles, which are the reader's picks for every view of the project. */
export interface BoardToggles {
  readonly showIcebox: boolean;
  readonly setShowIcebox: (shown: boolean) => void;
  readonly lanes: boolean;
  readonly setLanes: (shown: boolean) => void;
}

/**
 * The Board of a view of the Issues tab, in the table's place: the view's rows, in columns by
 * status or, in a project with a Legion daemon (`hasLegion`), by Legion phase
 * (`group=legion_phase`, which the table groups by too), one column for each of the board's
 * columns (`phases`). Over it `header` (the view tabs), the listing's `notices` and the control
 * row, whose chips are the table's (`viewChips`) and the view's search where it differs from the
 * view's own, since the Board has no header to show a column's filter (the spec's Design, "The
 * Board"); `readStates` stand in its place while the rows are out. Its cards are every row the
 * view lists, in the view's sort, on no page: the table is held ungrouped on the Board
 * (`useIssueTable`), so its sorted rows are those rows alone.
 */
export function IssuesTabBoard({
  address,
  allItems,
  excluded,
  flags,
  hasLegion,
  header,
  layoutSwitch,
  listsDone,
  loaded,
  name,
  notices,
  onClear,
  own,
  phases,
  project,
  readStates,
  state,
  toggles,
  userState,
  viewControls,
  viewKey,
}: {
  address: TableAddress<View>;
  /** Every row the view has read, whose labels the Labels picker offers. */
  allItems: readonly Item[];
  /** The trees the view leaves out (`exclude_tree`), each a chip and a line of the Filters panel. */
  excluded: readonly FilterChip[];
  flags: IssueFiltersState;
  hasLegion: boolean;
  header: ReactNode;
  layoutSwitch: ReactNode;
  /** The view lists done issues, which the Done rail holds. */
  listsDone: boolean;
  loaded: boolean;
  name: string;
  notices: ReactNode;
  /** Clear filters, while the view's narrowings differ from its own. */
  onClear: (() => void) | undefined;
  own: OwnNarrowings;
  /** The board's columns, in its order and under its labels (`legionPhasesOf`). */
  phases: readonly LegionPhaseColumn[];
  project: string;
  readStates: ReactNode;
  state: IssueTableState;
  toggles: BoardToggles;
  userState: UserState | undefined;
  /** Save view, and in a project with a Legion daemon the (i) hint after it. */
  viewControls: ReactNode;
  /** The view the reader picked, and whether its first draw is whole (`useTabReads`). */
  viewKey: string;
}): ReactNode {
  const { table } = state;
  const { view, setGrouping, setSearch } = address;
  const { filters, grouping, search, sorting } = view;
  const by: BoardBy = hasLegion && grouping[0] === "legion_phase" ? "legion_phase" : "status";
  const sortedRows = table.getSortedRowModel();
  const rows = useMemo(() => sortedRows.rows.map((row) => row.original), [sortedRows]);
  // The labels the Labels picker offers: every label of the rows the view has read.
  const labelOptions = useMemo(
    () =>
      [...new Set(allItems.flatMap((item) => item.issue.labels ?? []))].sort((a, b) =>
        a.localeCompare(b)
      ),
    [allItems]
  );
  // The view's search, where it differs from the view's own, is a chip of the Board's row too.
  const searched: FilterChip[] =
    search.trim() === "" || search === own.search
      ? []
      : [{ key: "search", label: `Search: ${search.trim()}`, remove: () => setSearch(own.search) }];
  const firstSort = sorting[0];
  return (
    <section aria-label={name} className="space-y-3 sm:pb-24">
      {header}
      {notices}
      <BoardControlRow
        chips={[
          ...searched,
          ...viewChips({ columns: table.getAllLeafColumns(), flags, narrowings: excluded, own }),
        ]}
        controls={
          <>
            {layoutSwitch}
            {/* Icebox and Done are the status Board's own: by Legion phase no drop changes a
                status, so it draws neither the Icebox rail nor the Done rail. */}
            {by === "status" ? (
              <ToggleButton onChange={toggles.setShowIcebox} pressed={toggles.showIcebox}>
                Show Icebox
              </ToggleButton>
            ) : null}
            <ToggleButton onChange={toggles.setLanes} pressed={toggles.lanes}>
              Lanes
            </ToggleButton>
            {viewControls}
          </>
        }
        groupBy={
          hasLegion
            ? {
                value: by,
                onChange: (next) => setGrouping(next === "legion_phase" ? [next] : []),
              }
            : undefined
        }
        labelOptions={labelOptions}
        narrowings={excluded}
        onClear={onClear}
        onSearch={setSearch}
        search={search}
        selection={state.pickedChip}
      />
      {readStates}
      {loaded ? (
        <IssueBoard
          by={by}
          columns={boardColumns({
            by,
            listsDone,
            phaseFilterPicks: picksOf(filters, "legion_phase"),
            phases,
            rows,
            showIcebox: toggles.showIcebox,
            statusPicks: picksOf(filters, "status"),
          })}
          lanes={toggles.lanes}
          project={project}
          rankRefusal={rankRefusal(
            firstSort,
            String(
              firstSort === undefined
                ? ""
                : (table.getColumn(firstSort.id)?.columnDef.header ?? firstSort.id)
            )
          )}
          rows={rows}
          userState={userState}
          // Its lanes and its Icebox are the reader's own picks too.
          viewKey={`${viewKey}\n${toggles.lanes}\n${toggles.showIcebox}`}
        />
      ) : null}
    </section>
  );
}
