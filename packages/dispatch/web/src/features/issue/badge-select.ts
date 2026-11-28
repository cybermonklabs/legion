import { hasFocusVisibleRing } from "../../theme/classes";

const badgeSelectFrame = "relative inline-flex min-h-11 shrink-0 items-center rounded-sm";

/** The "tinted badge with an invisible native select over it" control (priority, assignee):
 *  the wrapper is the 44 px tap target (32 px from `md`) and draws the keyboard focus ring. */
export const badgeSelectWrapper = `${badgeSelectFrame} md:min-h-8 has-focus-visible:ring-2 ${hasFocusVisibleRing}`;
/** The same wrapper in the new issue page's header properties line, where the controls are the
 *  line's own height, as the pictures draw them: 24 px, the least a touch target may be (WCAG
 *  2.5.8), because the header at 390 px is held to eight lines and 250 px, nine and 280 px while
 *  an approval request waits on the reader, which leaves no room for more. It is 24 px at every
 *  width, from `md` too, since a 24 px target holds at every width and not only on a phone. It
 *  holds a value - a person, a priority - so it narrows to its line rather than keep its width,
 *  and the value inside breaks anywhere (`min-w-0 max-w-full`). */
export const badgeSelectHeaderWrapper = `relative inline-flex min-h-6 min-w-0 max-w-full items-center rounded-sm has-focus-visible:ring-2 ${hasFocusVisibleRing}`;
/** The visible badge; the caller appends its tone's `bg` and `text`. */
export const badgeSelectBadge =
  "inline-flex items-center rounded-sm px-1.5 py-0.5 text-xs font-semibold";
/** Inside `NoTooltips` (the new issue page) every value a shared control draws wraps: it keeps
 *  `max-w-full` and breaks anywhere, and nothing around it keeps its width, so a long login with
 *  no space stays inside Details' row rather than running past it. These are
 *  `badgeSelectWrapper` in that shape, with `min-w-0 max-w-full` in place of `shrink-0`, and
 *  `badgeSelectBadge` with `max-w-full [overflow-wrap:anywhere]`. The select over the badge
 *  stays usable however many lines the badge takes: the wrapper is `relative` and as tall as the
 *  badge it holds, and `badgeSelectOverlay` is `absolute inset-0 size-full` over the wrapper, so
 *  it covers every line of a badge that wraps. */
const badgeSelectPageFrame =
  "relative inline-flex min-h-11 min-w-0 max-w-full items-center rounded-sm";
export const badgeSelectPageWrapper = `${badgeSelectPageFrame} md:min-h-8 has-focus-visible:ring-2 ${hasFocusVisibleRing}`;
export const badgeSelectPageBadge = `${badgeSelectBadge} max-w-full [overflow-wrap:anywhere]`;
/** The native select laid over the badge, invisible but the thing that receives the pick. */
export const badgeSelectOverlay =
  "absolute inset-0 size-full cursor-pointer appearance-none opacity-0 disabled:cursor-not-allowed";
