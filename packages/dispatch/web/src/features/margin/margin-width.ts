/** How wide the margin column may be, and the class of that column on a desktop. Their own
 *  module because the shell reads them while `features/margin/Margin.tsx` is still loading: the
 *  margin's code is fetched after the first paint, the width the shell restores from the
 *  reader's preference must not wait for it, and the empty column the shell holds in its place
 *  (`MarginFrame`, `MarginColumn.tsx`) must be the column the margin then draws. */

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

/** The class of the margin's column on a desktop, where it takes its own width in the row. */
export const MARGIN_COLUMN_CLASS = "relative order-3 shrink-0";

/** The width of the margin's desktop column: the reader's width, or the default, clamped. */
export function marginColumnWidth(width: number | undefined): number {
  return clampMarginWidth(width ?? DEFAULT_MARGIN_WIDTH);
}
