/**
 * The draft matrix's coverage lists (INFRA-1503). Each `draft-matrix.*.coverage.ts`, beside its
 * matrix file, holds every census item that file answers for to one of its tests, by its exact
 * title, or to one of the numbered reasons of its header, by that reason's text; the header's
 * tables are the prose of the list. The lists sit beside the matrix files rather than in them
 * because a matrix file is a test, which a list imported from it would run again inside the
 * census's own test.
 *
 * An entry is keyed by its item's key (`census.ts`). A reason for a move a host cannot make at
 * all, since its composer draws no Close, no issue picker or no Back, is about that composer as a
 * whole, which its send name names, so it is keyed by the mount's `mutationKey` address prop.
 */

/** One census item's place in a draft matrix file. */
export type CoverageEntry =
  | { readonly key: string; readonly row: string }
  | { readonly key: string; readonly reason: string };

/** What answers for an item in its matrix file: one of its tests, or one of its reasons. */
export type Answer = { readonly row: string } | { readonly reason: string };

/** The states each cell of a matrix table writes its draft in. */
const HELD = ["typed", "refused", "stopped"] as const;

/** The tests of one row of a matrix table, one per state: `<row> · typed` and the others. */
export function cells(row: string, states: readonly string[] = HELD): Answer[] {
  return states.map((state) => ({ row: `${row} · ${state}` }));
}

/** The entries that hold the item `key` to each of `found`. */
export function answers(
  key: string,
  ...found: readonly (Answer | readonly Answer[])[]
): CoverageEntry[] {
  return found.flat().map((answer) => ({ ...answer, key }));
}
