import { CONTROL_LABELS } from "./page-words";

/** For unit tests only: how many entries each list of Details and its history draws at first,
 *  and how many more each press of Show more draws, the spec's 20. It is written here, never
 *  read from the page's own constants, on purpose: a page that draws any other number fails the
 *  cases that count by it. */
export const PAGE_SIZE = 20;

/** For unit tests only: Show more, under a list of Details or under its history, whatever count
 *  it says, found by the page's word. */
export const SHOW_MORE = new RegExp(`^${CONTROL_LABELS.show_more}`, "u");

/** For unit tests only: the accessible name of Show more with `left` entries of its list not
 *  drawn, in the page's words: "Show more (25 left)" (`details-leaves.tsx`'s `ShowMoreWords`). */
export function showMoreName(left: number): string {
  return `${CONTROL_LABELS.show_more} (${left} ${CONTROL_LABELS.show_more_left})`;
}
