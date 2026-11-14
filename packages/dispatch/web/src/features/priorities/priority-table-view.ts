import type {
  ColumnFiltersState,
  GroupingState,
  PaginationState,
  SortingState,
} from "@tanstack/react-table";
import { useMemo } from "react";
import { useSearchParams } from "react-router-dom";
import { capSearchQuery } from "../../lib/query-param";
import { PAGE_SIZES } from "../project/issues-table-model";
import type { TableColumn } from "./priority-table-model";

/** What the reader is viewing in one of these tables, as the address carries it. */
export interface TableView {
  readonly sorting: SortingState;
  readonly grouping: GroupingState;
  readonly filters: ColumnFiltersState;
  readonly search: string;
  readonly page: number;
  readonly pageSize: number;
}

const DEFAULT_PAGE_SIZE = 50;

/** The address key one control of the table with this prefix is written under: `p.sort` for
 *  the priorities table, `f.` before a column's own filter so no column can take a control's
 *  name. Every table of this page carries a prefix, since the page draws three at once. */
const viewKey = (prefix: string, name: string) => `${prefix}.${name}`;

/** The view `params` carries for the table with this prefix, over these columns. A filter of a
 *  column the table does not have is left where it is: it is another table's, or an older
 *  link's. */
export function readTableView<Row>(
  params: URLSearchParams,
  prefix: string,
  columns: readonly TableColumn<Row>[],
  defaultSorting: SortingState
): TableView {
  const key = (name: string) => viewKey(prefix, name);
  const ids = columns.map((column) => column.id);
  const sort = params.get(key("sort"));
  const sorting = (sort ?? "")
    .split(",")
    .map((part) => ({ id: part.replace(/^-/, ""), desc: part.startsWith("-") }))
    .filter(({ id }) => ids.includes(id));
  const filters = columns.flatMap((column) => {
    const values = params.getAll(key(`f.${column.id}`));
    if (values.length === 0) return [];
    return [{ id: column.id, value: column.text === undefined ? values : (values[0] ?? "") }];
  });
  const group = params.get(key("group")) ?? "";
  const page = Number(params.get(key("page")));
  const size = Number(params.get(key("size")));
  return {
    sorting: sort === "" ? [] : sorting.length > 0 ? sorting : defaultSorting,
    grouping: columns.some((column) => column.id === group && column.group !== undefined)
      ? [group]
      : [],
    filters,
    search: capSearchQuery(params.get(key("q")) ?? ""),
    page: Number.isSafeInteger(page) && page > 0 ? page - 1 : 0,
    pageSize: PAGE_SIZES.includes(size) ? size : DEFAULT_PAGE_SIZE,
  };
}

/** `params` with this table's controls rewritten to `view`, leaving every other table's and
 *  every other parameter alone. A control at its default writes nothing, so a shared link
 *  carries what the reader changed and nothing else. */
export function writeTableView<Row>(
  params: URLSearchParams,
  prefix: string,
  columns: readonly TableColumn<Row>[],
  view: TableView,
  defaultSorting: SortingState
): URLSearchParams {
  const key = (name: string) => viewKey(prefix, name);
  const next = new URLSearchParams(params);
  for (const name of ["sort", "group", "q", "page", "size"]) next.delete(key(name));
  for (const column of columns) next.delete(key(`f.${column.id}`));
  const sort = view.sorting.map(({ id, desc }) => `${desc ? "-" : ""}${id}`).join(",");
  const fallback = defaultSorting.map(({ id, desc }) => `${desc ? "-" : ""}${id}`).join(",");
  if (sort !== fallback) next.set(key("sort"), sort);
  if (view.grouping[0] !== undefined) next.set(key("group"), view.grouping[0]);
  for (const filter of view.filters) {
    const values = Array.isArray(filter.value) ? filter.value.map(String) : [String(filter.value)];
    for (const value of values) if (value !== "") next.append(key(`f.${filter.id}`), value);
  }
  if (view.search !== "") next.set(key("q"), view.search);
  if (view.page > 0) next.set(key("page"), String(view.page + 1));
  if (view.pageSize !== DEFAULT_PAGE_SIZE) next.set(key("size"), String(view.pageSize));
  return next;
}

/**
 * One table's view, read from and written to the address under its own prefix, so a shared link
 * opens all three tables of these pages as the reader left them. Every setter has the shape
 * TanStack's `on…Change` handlers expect, and every change but the page itself returns to the
 * first page, as the issues table's own view does: a filter leaving fewer rows than the page
 * the reader stood on would otherwise show nothing.
 */
export function useTableView<Row>(
  prefix: string,
  columns: readonly TableColumn<Row>[],
  defaultSorting: SortingState
) {
  const [params, setParams] = useSearchParams();
  const view = useMemo(
    () => readTableView(params, prefix, columns, defaultSorting),
    [params, prefix, columns, defaultSorting]
  );
  const update = (change: (current: TableView) => TableView) =>
    setParams(
      (current) =>
        writeTableView(
          current,
          prefix,
          columns,
          change(readTableView(current, prefix, columns, defaultSorting)),
          defaultSorting
        ),
      { replace: true }
    );
  return {
    params,
    setParams,
    view,
    setSorting: (update_: SortingState | ((current: SortingState) => SortingState)) =>
      update((current) => ({
        ...current,
        sorting: typeof update_ === "function" ? update_(current.sorting) : update_,
        page: 0,
      })),
    setGrouping: (update_: GroupingState | ((current: GroupingState) => GroupingState)) =>
      update((current) => ({
        ...current,
        grouping: typeof update_ === "function" ? update_(current.grouping) : update_,
        page: 0,
      })),
    setFilters: (
      update_: ColumnFiltersState | ((current: ColumnFiltersState) => ColumnFiltersState)
    ) =>
      update((current) => ({
        ...current,
        filters: typeof update_ === "function" ? update_(current.filters) : update_,
        page: 0,
      })),
    setSearch: (search: string) =>
      update((current) => ({ ...current, search: capSearchQuery(search), page: 0 })),
    setPagination: (update_: PaginationState | ((current: PaginationState) => PaginationState)) =>
      update((current) => {
        const next =
          typeof update_ === "function"
            ? update_({ pageIndex: current.page, pageSize: current.pageSize })
            : update_;
        return { ...current, page: next.pageIndex, pageSize: next.pageSize };
      }),
  };
}
