import type { Agent } from "../../api/types";

export type MatchingSelectionState = "none" | "some" | "all";

export interface MatchingSelection {
  /** Rows the filters match, folded sections included. */
  readonly matchingCount: number;
  /** Selected sessions among the rows the filters match, folded sections included. */
  readonly selectedMatching: number;
  /** Selected sessions still listed that an active filter hides. */
  readonly selectedHidden: number;
  /** Selected sessions a current list no longer holds: the registry no longer has them. */
  readonly selectedGone: number;
  /** Selected sessions missing from a list that is not current, which cannot say they are gone:
   *  whether they still run is unknown. */
  readonly selectedUnknown: number;
  /** The header checkbox's state, read from the matching rows alone. */
  readonly state: MatchingSelectionState;
}

function countSelected(rows: readonly Agent[], selected: ReadonlySet<string>): number {
  let count = 0;
  for (const agent of rows) if (selected.has(agent.session_id)) count += 1;
  return count;
}

/** How much of what the filters match is selected, out of every listed session. A selected
 *  session the filters hide, or one missing from the list, never makes the header mixed; each is
 *  counted apart, so nothing passes part of the selection off as all of it. One missing from the
 *  list is gone only while the list is `current`; otherwise its state is unknown. */
export function matchingSelection(
  listed: readonly Agent[],
  matching: readonly Agent[],
  selected: ReadonlySet<string>,
  current: boolean
): MatchingSelection {
  const selectedMatching = countSelected(matching, selected);
  const selectedListed = countSelected(listed, selected);
  const selectedMissing = selected.size - selectedListed;
  const state =
    selectedMatching === 0 ? "none" : selectedMatching === matching.length ? "all" : "some";
  return {
    matchingCount: matching.length,
    selectedGone: current ? selectedMissing : 0,
    selectedHidden: selectedListed - selectedMatching,
    selectedMatching,
    selectedUnknown: current ? 0 : selectedMissing,
    state,
  };
}

/** The header's count: how many rows the filters match and how many of them are selected,
 *  then every other selected session, since the composer names and sends to those too - the
 *  ones an active filter hides apart from the ones missing from the list, which a current list
 *  calls no longer listed and one that is not current of unknown state. */
export function selectionSummary(selection: MatchingSelection): string {
  const { matchingCount, selectedGone, selectedHidden, selectedMatching, selectedUnknown } =
    selection;
  const more = selectedMatching === 0 ? "" : "more ";
  const parts = [
    selectedMatching === 0
      ? `${matchingCount} matching`
      : `${selectedMatching} of ${matchingCount} matching selected`,
  ];
  if (selectedHidden > 0) parts.push(`${selectedHidden} ${more}selected outside the filter`);
  if (selectedGone > 0) parts.push(`${selectedGone} ${more}selected, no longer listed`);
  if (selectedUnknown > 0) parts.push(`${selectedUnknown} ${more}selected, state unknown`);
  return parts.join(" · ");
}

/** A group's count, as its heading wears it: `(N)`, and `(N, K selected)` once any of its rows
 *  is selected, so the header checkbox never selects a row out of sight silently. */
export function groupCountLabel(rows: readonly Agent[], selected: ReadonlySet<string>): string {
  const count = countSelected(rows, selected);
  return `(${rows.length}${count === 0 ? "" : `, ${count} selected`})`;
}

/** The header checkbox's click: select every matching session unless all of them already
 *  are, and otherwise unselect them. Every other selected session stays selected either way. */
export function toggleMatching(
  matching: readonly Agent[],
  selected: ReadonlySet<string>
): Set<string> {
  const next = new Set(selected);
  const selectAll = countSelected(matching, selected) !== matching.length;
  for (const agent of matching) {
    if (selectAll) next.add(agent.session_id);
    else next.delete(agent.session_id);
  }
  return next;
}
