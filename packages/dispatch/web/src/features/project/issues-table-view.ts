import {
  copyAddress,
  type TableAddress,
  type TableAddressGrammar,
  useTableAddress,
} from "../../lib/page-address";
import { readTableAddress, readUrl, writeTableAddress, writeUrl } from "./issues-table-model";
import { compressAgainst, embeddedDefaults, expandOver } from "./issues-views";

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

/** One grammar per prefix, made once: `useTableAddress` memoizes on the grammar's identity. */
const embeddedGrammars = new Map<string, TableAddressGrammar>();

/**
 * The spelling of an issue table another page draws (the priority pages): no view, so no
 * `view=`, and every parameter of its own under `<stateKey>.`, so a page drawing two tables
 * keeps a sort, a search, a filter set and a page for each. What it opens with is the open
 * statuses sorted by Place (`embeddedDefaults`), which a link leaves out as the project's views
 * leave out their own parameters.
 */
export function embeddedGrammar(stateKey: string): TableAddressGrammar {
  const known = embeddedGrammars.get(stateKey);
  if (known !== undefined) return known;
  const mark = `${stateKey}.`;
  const own = embeddedDefaults();
  const made: TableAddressGrammar = {
    read: (params) =>
      readTableAddress(
        expandOver(
          own,
          copyAddress(
            [...params.entries()].flatMap(([key, value]) =>
              key.startsWith(mark) ? [[key.slice(mark.length), value] as const] : []
            )
          )
        )
      ),
    write: (params, view) => {
      const next = copyAddress(params);
      for (const key of [...next.keys()]) if (key.startsWith(mark)) next.delete(key);
      for (const [key, value] of compressAgainst(own, writeTableAddress(copyAddress(), view)))
        next.append(`${mark}${key}`, value);
      return next;
    },
  };
  embeddedGrammars.set(stateKey, made);
  return made;
}

/**
 * The issue table's view — what it sorts, groups, filters, searches and pages by — held in the
 * page address, so a shared link opens the same table. Everything about how that is read and
 * written, `replace` included, is `lib/page-address.ts`'s; this names the grammar: the project's
 * views on the Issues tab, or, given a `stateKey`, the prefixed spelling of a table another page
 * draws.
 */
export function useTableView(
  table: { readonly hasLegion: boolean } | { readonly stateKey: string }
): TableAddress {
  return useTableAddress(
    "stateKey" in table
      ? embeddedGrammar(table.stateKey)
      : table.hasLegion
        ? LEGION_ISSUES_TABLE_GRAMMAR
        : ISSUES_TABLE_GRAMMAR
  );
}
