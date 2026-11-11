import type {
  ColumnFiltersState,
  GroupingState,
  PaginationState,
  SortingState,
} from "@tanstack/react-table";
import { useMemo } from "react";
import { useSearchParams } from "react-router-dom";
import { capSearchQuery } from "../../lib/query-param";
import { readUrl, type View, writeUrl } from "./issues-table-model";

/**
 * The table's view — what it sorts, groups, filters, searches and pages by — read from and
 * written to the URL, so a shared link opens the same table. Every setter has the shape
 * TanStack's `on…Change` handlers expect, and every change but the page itself returns to the
 * first page: a filter that leaves fewer rows than the page the reader was on would otherwise
 * show nothing.
 */
export function useTableView(hasLegion = false) {
  const [params, setParams] = useSearchParams();
  const view = useMemo(() => readUrl(params, hasLegion), [params, hasLegion]);
  const updateView = (update: (current: View) => View) =>
    setParams((current) => writeUrl(current, update(readUrl(current, hasLegion)), hasLegion), {
      replace: true,
    });
  return {
    params,
    setParams,
    view,
    setSorting: (update: SortingState | ((current: SortingState) => SortingState)) =>
      updateView((current) => ({
        ...current,
        sorting: typeof update === "function" ? update(current.sorting) : update,
        page: 0,
      })),
    setGrouping: (update: GroupingState | ((current: GroupingState) => GroupingState)) =>
      updateView((current) => ({
        ...current,
        grouping: typeof update === "function" ? update(current.grouping) : update,
        page: 0,
      })),
    setFilters: (
      update: ColumnFiltersState | ((current: ColumnFiltersState) => ColumnFiltersState)
    ) =>
      updateView((current) => ({
        ...current,
        filters: typeof update === "function" ? update(current.filters) : update,
        page: 0,
      })),
    setSearch: (search: string) =>
      updateView((current) => ({ ...current, search: capSearchQuery(search), page: 0 })),
    setPagination: (update: PaginationState | ((current: PaginationState) => PaginationState)) =>
      updateView((current) => {
        const next =
          typeof update === "function"
            ? update({ pageIndex: current.page, pageSize: current.pageSize })
            : update;
        return { ...current, page: next.pageIndex, pageSize: next.pageSize };
      }),
  };
}
