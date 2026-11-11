import type { ColumnFiltersState } from "@tanstack/react-table";
import { type Item, matchesFilters, readUrl } from "./issues-table-model";
import type { BuiltInView } from "./issues-table-view-spec";

export type { BuiltInView, BuiltInViewId } from "./issues-table-view-spec";
export {
  BUILT_IN_VIEWS,
  DEFAULT_VIEW,
  expandView,
  issuesViewHref,
  viewOf,
} from "./issues-table-view-spec";

/** Counts the items a built-in view selects before any reader override or search. */
export function viewCount(
  view: BuiltInView,
  items: readonly Item[],
  hasLegion: boolean,
  nowMs: number
): number {
  if (view.legionOnly && !hasLegion) return 0;
  const filters: ColumnFiltersState = readUrl(view.params(hasLegion), hasLegion).filters;
  return items.reduce(
    (count, item) => count + Number(matchesFilters(item, filters, undefined, true, nowMs)),
    0
  );
}
