import {
  borderDefault,
  card,
  focusVisibleRing,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textSecondaryOnSurface,
} from "../../theme/classes";

/**
 * The shape the issue page's menus wear (dispatch://INFRA-1196 R11, R16).
 *
 * The page opens three: the identity block's actions, its add control, and the node menu
 * beside the presets, which the preset row itself borrows below 1025 px. They are declared
 * here rather than inside any one of them, because none of the three owns the shape and a
 * copy inside each is how three menus come to look like three.
 */

/** The panel a menu opens, below the control that opened it. A menu that opens to the right of
 *  its control, or against the page's own edge, places itself and wears the rest of this. */
export const MENU_CLASS = `absolute left-0 top-full z-30 mt-1 flex w-56 flex-col gap-1 rounded-lg border p-2 shadow-lg ${borderDefault} ${card}`;

/** One item inside that panel. */
export const MENU_ITEM_CLASS = `min-h-11 rounded-lg px-2 py-2 text-left text-sm font-medium ${textSecondaryOnSurface}`;

/** The control that opens a menu of the identity block. */
export const MENU_TRIGGER_CLASS = `min-h-11 rounded-lg px-2 text-sm font-medium md:min-h-8 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${focusVisibleRing}`;
