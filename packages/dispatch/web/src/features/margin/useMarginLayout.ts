import { useLocation } from "react-router-dom";

import { type MarginDrawer, useMarginDrawer } from "../issue-page/margin-slot";
import { routeHasMargin } from "../refs/routes";
import { COMPACT_VIEWPORT_QUERY, useMediaQuery } from "../shell/useDialog";
import { clampMarginWidth, DEFAULT_MARGIN_WIDTH } from "./margin-width";

/** The desktop column's class: its place in the shell's row, after the page, at its own width. */
export const MARGIN_COLUMN_CLASS = "relative order-3 shrink-0";

/** What the margin draws on this route at this width, and how wide. */
export interface MarginLayout {
  /** Whether the margin draws its desktop column: no drawer, above the compact layout, on a
   *  route that has a margin, and not collapsed to its rail. */
  columnShown: boolean;
  /** The new issue page's Details drawer, where the shell draws that page's frame; undefined on
   *  every other page, where the margin is the column it has always been. */
  drawer: MarginDrawer | undefined;
  hasMargin: boolean;
  isCompactViewport: boolean;
  /** Whether the margin is collapsed to its rail. Collapsed, it stays mounted under the rail,
   *  hidden: an open composer's unsent draft is still there when the margin comes back. A drawer
   *  has no rail: closed, it is gone from the screen, and Details opens it. */
  railShown: boolean;
  /** The margin's width in CSS pixels, the column's or the drawer's: the reader's, or the
   *  default, clamped to the viewport. */
  width: number;
}

/**
 * The rule the margin draws by (`Margin.tsx`), which `MarginColumn.tsx`'s fallback holds the
 * column's place by on the frames before the margin's code arrives, so the page beside it has
 * its final width from the first paint. Its own module, as `margin-width.ts` is: the shell reads
 * it while `Margin.tsx` is still loading, so it imports nothing the shell's entry chunk does not
 * already carry.
 */
export function useMarginLayout({
  collapsed = false,
  width,
}: {
  collapsed?: boolean;
  width?: number;
}): MarginLayout {
  const isCompactViewport = useMediaQuery(COMPACT_VIEWPORT_QUERY);
  const { pathname, search } = useLocation();
  const drawer = useMarginDrawer();
  const hasMargin = routeHasMargin(pathname, search);
  const desktopMargin = drawer === undefined && !isCompactViewport && hasMargin;
  return {
    columnShown: desktopMargin && !collapsed,
    drawer,
    hasMargin,
    isCompactViewport,
    railShown: desktopMargin && collapsed,
    width: clampMarginWidth(width ?? DEFAULT_MARGIN_WIDTH),
  };
}
