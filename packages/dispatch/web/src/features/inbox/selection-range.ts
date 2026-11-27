/**
 * The run of rows a shift-click on a row's checkbox marks in the Inbox, and `Shift+X`, `Shift+J`
 * and `Shift+K` from the keyboard: every row the list draws from the anchor to the row clicked.
 * Which row is the anchor, and which state the run takes, is the selection's
 * (`useInboxSelection`).
 */

/** The ids of `order` from `anchor` to `clicked`, both included, in `order`'s order, whichever
 *  of the two comes first. Where `order` does not hold both, the run is `clicked` alone: an
 *  anchor the list no longer draws starts no run. */
export function rangeOf(
  order: readonly string[],
  anchor: string,
  clicked: string
): readonly string[] {
  const from = order.indexOf(anchor);
  const to = order.indexOf(clicked);
  if (from === -1 || to === -1) return [clicked];
  return order.slice(Math.min(from, to), Math.max(from, to) + 1);
}
