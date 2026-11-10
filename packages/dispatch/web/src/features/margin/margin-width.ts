/** How wide the margin column may be.
 *
 *  Its own module because the shell reads the default for the stored preference it hands
 *  `Margin` (`app.tsx`), while `Margin` itself is a chunk the shell loads only once it is up
 *  (`features/shell/deferred-shell.ts`): a handful of numbers must not pull the margin's whole
 *  column, its sheet and its composer into the entry chunk. */

export const DEFAULT_MARGIN_WIDTH = 384;
export const MIN_MARGIN_WIDTH = 280;
export const MARGIN_WIDTH_STEP = 24;

/** The widest the margin may be dragged: 60 % of the viewport, never below the minimum. */
export function maxMarginWidth(): number {
  return Math.max(MIN_MARGIN_WIDTH, Math.floor(window.innerWidth * 0.6));
}

export function clampMarginWidth(width: number): number {
  return Math.min(Math.max(width, MIN_MARGIN_WIDTH), maxMarginWidth());
}

/** The width the margin's desktop column lays itself out at: the reader's stored preference,
 *  clamped, and the default where they have none.
 *
 *  `Margin` draws its column at this and `MarginColumn` reserves this same number on the
 *  frames before the margin's chunk arrives, so the box the reader sees before the margin is
 *  there and the box they see after cannot be two different widths. Written once here rather
 *  than twice, because the whole point of the reservation is that the two agree. */
export function marginColumnWidth(width: number | undefined): number {
  return clampMarginWidth(width ?? DEFAULT_MARGIN_WIDTH);
}
