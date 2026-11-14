import {
  type TableAddress,
  type TableAddressGrammar,
  useTableAddress,
} from "../../lib/page-address";
import { readUrl, writeUrl } from "./issues-table-model";

/** The project's own spelling of its issue table in the address: `?view=` names the view, and
 *  every other parameter is the difference from that view (`issues-views.ts`), so this table
 *  brings its own grammar rather than taking the ordinary one (`tableAddressGrammar`). One per
 *  answer to whether the project has a Legion daemon, since the views differ by it; module-level
 *  constants, since `useTableAddress` memoizes the view it reads on the grammar's identity. */
const ISSUES_TABLE_GRAMMAR: TableAddressGrammar = {
  read: (params) => readUrl(params, false),
  write: (params, view) => writeUrl(params, view, false),
};
const LEGION_ISSUES_TABLE_GRAMMAR: TableAddressGrammar = {
  read: (params) => readUrl(params, true),
  write: (params, view) => writeUrl(params, view, true),
};

/**
 * The issue table's view — what it sorts, groups, filters, searches and pages by — held in the
 * page address, so a shared link opens the same table. Everything about how that is read and
 * written, `replace` included, is `lib/page-address.ts`'s; this names the grammar.
 */
export function useTableView(hasLegion: boolean): TableAddress {
  return useTableAddress(hasLegion ? LEGION_ISSUES_TABLE_GRAMMAR : ISSUES_TABLE_GRAMMAR);
}
