import { SHELL_BAR_BOTTOM_PROPERTY } from "../shell/useShellBarBottom";

/** Where the widened side panel starts (R1, A6): the top left corner of the room the shell's
 *  navigation leaves free, as CSS lengths. The panel runs from there to the window's right and
 *  bottom edges. A column of navigation beside it stays in view. A top bar above it scrolls with
 *  the page, and the panel starts where that bar ends in the window, so once the page has
 *  scrolled past the bar the panel starts at the window's top and covers the window, and the
 *  bar's Menu is out of view until the person scrolls back or docks the panel. */
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
 * because it fills the window already:
 *
 * - at the edge of a column of navigation beside the page: the icon rail where the frame draws it
 *   at any width from `md`, else, from `xl`, the sidebar, or the rail it folds to;
 * - below `xl` with no rail, where the navigation is a header and a drawer above the page, under
 *   the shell's top bar and across the whole width, which is the window's top once the page has
 *   scrolled past that bar (`WideInset`);
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

/**
 * Whether the panel at the side is wide (A6): the person has widened it (`widened`, the going
 * store's `wide`, which only the side holds) where the shell leaves it room to widen (`inset`,
 * from `wideInsetOf`). The panel draws itself from this, and so does the shell (`app.tsx`): while
 * the panel is docked, the shell keeps its 420 px free at the page's right, from `sm` up, and the
 * page and a top bar stand beside it; while it is wide, the panel covers the page from `inset` and
 * the shell keeps no room for it, so a top bar above it spans the whole window, with no empty band
 * between the bar and the panel.
 */
export function panelIsWide(widened: boolean, inset: WideInset | undefined): boolean {
  return widened && inset !== undefined;
}
