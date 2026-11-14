import type { SortingState } from "@tanstack/react-table";
import {
  capSearchQuery,
  copyAddress,
  type TableAddressGrammar,
  type TableAddressView,
} from "../../lib/page-address";
import { PAGE_SIZES } from "../project/issues-table-model";
import type { TableColumn } from "./priority-table-model";

const DEFAULT_PAGE_SIZE = 50;

/**
 * How one table of these pages spells its view in the address, under its own prefix, so a
 * shared link opens every table of the page as the reader left it: `p.sort`, `p.group`, `p.q`,
 * `p.page` and `p.size` for the priorities table, and `p.f.<column>` for a column's own filter,
 * the `f.` before it so no column can take a control's name. Every table of these pages carries
 * a prefix, since the page draws several at once. The page reads and writes the address through
 * `lib/page-address.ts`'s `useTableAddress`, which this grammar is handed to.
 *
 * The view read is the table's own: a sort naming no column of the table falls back to
 * `defaultSorting`, and an empty one (`p.sort=`) is no sort at all; only a column that groups is
 * grouped by; a filter of a column the table does not have is left where it is, since it is
 * another table's or an older link's. A control at its default writes nothing, so a shared link
 * carries what the reader changed and nothing else, and every other table's parameters and
 * every other parameter are left alone.
 */
export function priorityTableGrammar<Row>(
  prefix: string,
  columns: readonly TableColumn<Row>[],
  defaultSorting: SortingState
): TableAddressGrammar {
  const key = (name: string) => `${prefix}.${name}`;
  const ids = columns.map((column) => column.id);
  const fallback = defaultSorting.map(({ id, desc }) => `${desc ? "-" : ""}${id}`).join(",");
  return {
    read: (params): TableAddressView => {
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
    },
    write: (params, view) => {
      const next = copyAddress(params);
      for (const name of ["sort", "group", "q", "page", "size"]) next.delete(key(name));
      for (const column of columns) next.delete(key(`f.${column.id}`));
      const sort = view.sorting.map(({ id, desc }) => `${desc ? "-" : ""}${id}`).join(",");
      if (sort !== fallback) next.set(key("sort"), sort);
      if (view.grouping[0] !== undefined) next.set(key("group"), view.grouping[0]);
      for (const filter of view.filters) {
        const values = Array.isArray(filter.value)
          ? filter.value.map(String)
          : [String(filter.value)];
        for (const value of values) if (value !== "") next.append(key(`f.${filter.id}`), value);
      }
      if (view.search !== "") next.set(key("q"), view.search);
      if (view.page > 0) next.set(key("page"), String(view.page + 1));
      if (view.pageSize !== DEFAULT_PAGE_SIZE) next.set(key("size"), String(view.pageSize));
      return next;
    },
  };
}
