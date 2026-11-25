import { CONTROL_LABELS } from "./page-words";

/** For unit tests only: Show more, under a list of Details or under its history, whatever count
 *  it says, found by the page's word. */
export const SHOW_MORE = new RegExp(`^${CONTROL_LABELS.show_more}`, "u");

/** For unit tests only: the accessible name of Show more with `left` entries of its list not
 *  drawn, in the page's words: "Show more (25 left)" (`details-leaves.tsx`'s `ShowMoreWords`). */
export function showMoreName(left: number): string {
  return `${CONTROL_LABELS.show_more} (${left} ${CONTROL_LABELS.show_more_left})`;
}
