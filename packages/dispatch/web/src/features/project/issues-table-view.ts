import {
  type TableAddress,
  type TableAddressGrammar,
  useTableAddress,
} from "../../lib/page-address";
import { readUrl, writeUrl } from "./issues-table-model";

/** The project's own spelling of its issue table in the address: `?status=`, `?unclaimed=` and
 *  `?nofilter=` are the project's words, and the open statuses are a default a shared link
 *  leaves out, so this table brings its own grammar rather than taking the ordinary one
 *  (`tableAddressGrammar`). A module-level constant, since `useTableAddress` memoizes the view
 *  it reads on the grammar's identity. */
const ISSUES_TABLE_GRAMMAR: TableAddressGrammar = { read: readUrl, write: writeUrl };

/**
 * The issue table's view — what it sorts, groups, filters, searches and pages by — held in the
 * page address, so a shared link opens the same table. Everything about how that is read and
 * written, `replace` included, is `lib/page-address.ts`'s; this names the grammar.
 */
export function useTableView(): TableAddress {
  return useTableAddress(ISSUES_TABLE_GRAMMAR);
}
