import { SHELL_BAR_BOTTOM_PROPERTY } from "../shell/useShellBarBottom";

/** Where the widened side panel starts (R1, A6): the top left corner of the room the shell's
 *  navigation leaves free, as CSS lengths. The panel runs from there to the window's right and
 *  bottom edges, so the navigation stays in view and reachable beside it or above it. */
export interface WideInset {
  readonly left: string;
  readonly top: string;
}

/** What the inset follows of the shell's layout. */
export interface ShellLayout {
  /** Below Tailwind's `xl` (`COMPACT_VIEWPORT_QUERY`): the navigation is not a column beside the
   *  page. */
  readonly compact: boolean;
  /** The new issue page's icon rail stands beside the page (`iconRailShown`). */
  readonly iconRail: boolean;
  /** The reader has folded the sidebar to its rail. */
  readonly sidebarHidden: boolean;
  /** From Tailwind's `sm` up (`SM_VIEWPORT_QUERY`): below it the panel is the whole width of the
   *  window already (`w-full sm:w-[420px]`). */
  readonly sm: boolean;
}

/** Beside the sidebar's 20 rem. */
const BESIDE_SIDEBAR: WideInset = { left: "20rem", top: "0px" };

/** Beside the 3.5 rem of the rail the sidebar folds to and of the new issue page's icon rail. */
const BESIDE_RAIL: WideInset = { left: "3.5rem", top: "0px" };

/** Under the compact navigation bar, across the whole width. The shell header or the issue's
 *  own navigation row publishes its bottom edge through `useShellBarBottom`. */
const BELOW_BAR: WideInset = { left: "0px", top: `var(${SHELL_BAR_BOTTOM_PROPERTY}, 0px)` };

/**
 * Where the side panel starts once the person widens it (A6), or undefined where it cannot widen
 * because it fills the window already. The panel always leaves the navigation reachable:
 *
 * - at the edge of a column of navigation beside the page: the icon rail where the frame draws it
 *   at any width from `md`, else, from `xl`, the sidebar, or the rail it folds to;
 * - below `xl` with no rail, where the navigation is a header and a drawer above the page, under
 *   the shell's top bar and across the whole width;
 * - nowhere below `sm`.
 */
export function wideInsetOf({
  compact,
  iconRail,
  sidebarHidden,
  sm,
}: ShellLayout): WideInset | undefined {
  if (iconRail) return BESIDE_RAIL;
  if (!compact) return sidebarHidden ? BESIDE_RAIL : BESIDE_SIDEBAR;
  return sm ? BELOW_BAR : undefined;
}
