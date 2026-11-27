import type { Column } from "@tanstack/react-table";
import {
  columnFilterChip,
  columnFilterPicks,
  type FilterChip,
} from "../../components/filter-chips";
import { type IssueFiltersState, readIssueFlags } from "./issue-filters";
import { sameSet } from "./issues-filter-picks";
import { defaultView, type Item, readUrl, UNCLAIMED_FACET, type View } from "./issues-table-model";
import { expandView, readExcludedTrees } from "./issues-views";

/** What the open view narrows its rows by of itself: its column filters, its search and its two
 *  flags, which draw no chip (`viewChips`), and the issues it names (`only=`) and the trees it
 *  leaves out (`exclude_tree`), which its tab's count narrows by too (`openListingCount`). */
export interface OwnNarrowings {
  readonly filters: View["filters"];
  readonly search: string;
  readonly needsYou: boolean;
  readonly unread: boolean;
  readonly selection: View["selection"];
  readonly excluded: readonly string[];
}

/** What a table another page draws narrows by of itself: the open statuses it opens with, which
 *  its Clear filters goes back to (`defaultView`), and nothing else. */
export const LISTING_OWN: OwnNarrowings = {
  filters: defaultView.filters,
  search: "",
  needsYou: false,
  unread: false,
  selection: null,
  excluded: [],
};

/** What the view at `own`, the address the Issues tab's open view opens at (its built-in
 *  address, or a saved view's query), narrows by of itself, all of it read from the address
 *  with its view written out (`expandView`). */
export function ownNarrowings(own: URLSearchParams): OwnNarrowings {
  const expanded = expandView(own);
  const view = readUrl(expanded);
  return {
    filters: view.filters,
    search: view.search,
    ...readIssueFlags(expanded),
    selection: view.selection,
    excluded: readExcludedTrees(expanded),
  };
}

/**
 * The chips of the control row, in the table and on the Board alike: each narrowing that differs
 * from `own`, the open view's own, added or replacing one of its values, whether its column is
 * shown or hidden. First the flags (Needs you, Unread, and Unclaimed, which is Claimed by's
 * "Nobody or lapsed"), then `narrowings`, the narrowings that are no column (`exclude_tree`), then
 * every other column filter in the table's column order, in its header's words
 * (`columnFilterChip`): one chip for each label the view does not hold (`Label: <value>`, the
 * words the Board's Labels picker gives `label=` too, so one narrowing has one name), and one for
 * each other column whose picks are not the view's. A chip's press takes its narrowing back to
 * the view's own: taking a Status chip off goes back to the view's statuses, not to every closed
 * issue in the project, and on a saved view no press takes the saved view's own filter away.
 */
export function viewChips({
  columns,
  flags,
  narrowings,
  own,
}: {
  columns: readonly Column<Item, unknown>[];
  flags: Pick<IssueFiltersState, "needsYou" | "unread" | "setNeedsYou" | "setUnread">;
  narrowings: readonly FilterChip[];
  own: OwnNarrowings;
}): FilterChip[] {
  const flagged: FilterChip[] = [];
  if (flags.needsYou && !own.needsYou)
    flagged.push({ label: "Needs you", remove: () => flags.setNeedsYou(false) });
  if (flags.unread && !own.unread)
    flagged.push({ label: "Unread", remove: () => flags.setUnread(false) });
  const columned: FilterChip[] = [];
  for (const column of columns) {
    const picks = columnFilterPicks(column.getFilterValue());
    const ownValue = own.filters.find((filter) => filter.id === column.id)?.value;
    const kept = columnFilterPicks(ownValue);
    if (column.id === "labels") {
      for (const value of picks.filter((pick) => !kept.includes(pick)))
        columned.push({
          key: `label:${value}`,
          label: `Label: ${value}`,
          remove: () => {
            const rest = picks.filter((pick) => pick !== value);
            column.setFilterValue(rest.length === 0 ? undefined : rest);
          },
        });
    } else if (column.id === "claim") {
      // Claimed by's "Nobody or lapsed" is the Unclaimed flag, which has a chip of its own among
      // the flags; the column's chip names its other picks.
      const others = picks.filter((pick) => pick !== UNCLAIMED_FACET);
      const ownOthers = kept.filter((pick) => pick !== UNCLAIMED_FACET);
      if (picks.includes(UNCLAIMED_FACET) && !kept.includes(UNCLAIMED_FACET))
        flagged.push({
          label: "Unclaimed",
          remove: () => column.setFilterValue(others.length === 0 ? undefined : others),
        });
      if (others.length > 0 && !sameSet(others, ownOthers))
        columned.push(
          columnFilterChip(column, others, () => {
            const rest = [...ownOthers, ...picks.filter((pick) => pick === UNCLAIMED_FACET)];
            column.setFilterValue(rest.length === 0 ? undefined : rest);
          })
        );
    } else if (picks.length > 0 && !sameSet(picks, kept)) {
      columned.push(columnFilterChip(column, picks, () => column.setFilterValue(ownValue)));
    }
  }
  return [...flagged, ...narrowings, ...columned];
}
