import { useMemo } from "react";
import {
  type TableAddress,
  type TableAddressGrammar,
  useTableAddress,
} from "../../lib/page-address";
import { readUrl, type View, writeUrl } from "./issues-table-model";

/**
 * The issue table's view — what it sorts, groups, filters, searches and pages by, and the issues
 * a link names (`only=`) — held in the page address, so a shared link opens the same table.
 * Everything about how that is read and written, `replace` included, is `lib/page-address.ts`'s;
 * this names the grammar. The project's spelling of its issue table is its own: `?status=`,
 * `?unclaimed=` and `?nofilter=` are the project's words, and the open statuses are a default a
 * shared link leaves out, so this table brings its own grammar rather than taking the ordinary
 * one (`tableAddressGrammar`).
 *
 * `stateKey` is the prefix the table's own controls are written under (`<stateKey>.sort`), so a
 * page drawing two tables gives each one its own; a table with none keeps the bare `sort`, `q`,
 * `group`, `page` and `size` this page has always published. The grammar is memoized on it,
 * since `useTableAddress` memoizes the view it reads on the grammar's identity.
 */
export function useTableView(stateKey?: string): TableAddress<View> {
  const grammar = useMemo<TableAddressGrammar<View>>(
    () => ({
      read: (params) => readUrl(params, stateKey),
      write: (params, view) => writeUrl(params, view, stateKey),
    }),
    [stateKey]
  );
  return useTableAddress(grammar);
}
