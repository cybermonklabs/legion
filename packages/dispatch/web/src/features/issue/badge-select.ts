import type { SyntheticEvent } from "react";

import { focusVisibleRing, hasFocusVisibleRing } from "../../theme/classes";

const badgeSelectFrame = "relative inline-flex min-h-11 shrink-0 items-center rounded-sm";

/** The "tinted badge whose press opens a list" wrappers: the wrapper is the 44 px tap target
 *  (32 px from `md`) and draws the keyboard focus ring of the shared list's single pick inside
 *  it, whose trigger shows the badge (the issue header's assignee, an ask card's hand-on:
 *  `badgeSelectTrigger`). */
export const badgeSelectWrapper = `${badgeSelectFrame} md:min-h-8 has-focus-visible:ring-2 ${hasFocusVisibleRing}`;
/** The same wrapper in the issue header's details rail (the repository), 28 px from `md` like
 *  the rail's other controls, so it never makes the rail's row taller. */
export const badgeSelectRailWrapper = `${badgeSelectFrame} md:min-h-7 has-focus-visible:ring-2 ${hasFocusVisibleRing}`;
/** The trigger of a menu drawn as the same badge (`ChoiceMenu`: the snooze, the priority): the
 *  button is the 44 px tap target (32 px from `md`) and draws its own keyboard focus ring. It
 *  looks the same disabled or held (`refused:`, `styles.css`). */
export const badgeMenuTrigger = `${badgeSelectFrame} cursor-pointer outline-none md:min-h-8 focus-visible:ring-2 ${focusVisibleRing} refused:cursor-not-allowed refused:opacity-50`;
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
 *  `badgeSelectBadge` with `max-w-full [overflow-wrap:anywhere]`. The trigger over the badge
 *  stays usable however many lines the badge takes: the wrapper is `relative` and as tall as the
 *  badge it holds, and the trigger in it stretches to the wrapper's height and narrows to its
 *  width (`badgeSelectPageTrigger`, `badgeMenuPageTrigger`), so it covers every line of a badge
 *  that wraps. */
const badgeSelectPageFrame =
  "relative inline-flex min-h-11 min-w-0 max-w-full items-center rounded-sm";
export const badgeSelectPageWrapper = `${badgeSelectPageFrame} md:min-h-8 has-focus-visible:ring-2 ${hasFocusVisibleRing}`;
export const badgeSelectPageBadge = `${badgeSelectBadge} max-w-full [overflow-wrap:anywhere]`;
/** `badgeMenuTrigger` inside `NoTooltips` (the new issue page), in `badgeSelectPageWrapper`'s
 *  shape: `min-w-0 max-w-full` in place of `shrink-0`, so the trigger narrows to its line and the
 *  value it shows (`badgeSelectPageBadge`) breaks anywhere. */
export const badgeMenuPageTrigger = `${badgeSelectPageFrame} cursor-pointer outline-none md:min-h-8 focus-visible:ring-2 ${focusVisibleRing} refused:cursor-not-allowed refused:opacity-50`;
/** `badgeMenuTrigger` in the new issue page's header properties line, in
 *  `badgeSelectHeaderWrapper`'s shape: 24 px at every width, narrowing to its line. Below 1280 px
 *  every button is at least 44 px tall (`styles.css`): there the trigger overhangs the line by
 *  10 px above and below rather than make it 20 px taller, as `badgeSelectTrigger` overhangs its
 *  wrapper. */
export const badgeMenuHeaderTrigger = `relative inline-flex min-h-6 min-w-0 max-w-full items-center rounded-sm cursor-pointer outline-none max-xl:-my-2.5 focus-visible:ring-2 ${focusVisibleRing} refused:cursor-not-allowed refused:opacity-50`;
/** The trigger of a list laid over the badge, filling its wrapper's tap target, leaving the
 *  focus ring to the wrapper and showing a held trigger's press as refused; each wrapper's
 *  overhang is added below. */
const badgeSelectTriggerFrame =
  "flex cursor-pointer items-center self-stretch rounded-sm outline-none refused:cursor-not-allowed";
/** The trigger of a list laid over the badge in `badgeSelectWrapper` (the issue header's
 *  assignee, the ask card's hand-on): it shows the badge. Below 1280 px every button is at least
 *  44 px tall (`styles.css`), and from `md` the wrapper is 32 px: there the trigger overhangs it
 *  by 6 px above and below rather than make its line 12 px taller, so the line is the height it
 *  was when a native select lay over the badge, and the trigger is still the 44 px target that
 *  select was. */
export const badgeSelectTrigger = `${badgeSelectTriggerFrame} md:max-xl:-my-1.5`;
/** The same trigger in `badgeSelectPageWrapper`, which is `badgeSelectWrapper`'s height: it
 *  narrows with its wrapper, so the badge inside it wraps within the line. */
export const badgeSelectPageTrigger = `${badgeSelectTrigger} min-w-0 max-w-full`;
/** The same trigger in `badgeSelectHeaderWrapper`, which is 24 px at every width: below 1280 px
 *  the 44 px trigger overhangs it by 10 px above and below, so the header's line stays 24 px and
 *  the trigger is the 44 px target the select laid over it was; from 1280 px it is the wrapper's
 *  height. It narrows with its wrapper, so the value inside it wraps within the line. */
export const badgeSelectHeaderTrigger = `${badgeSelectTriggerFrame} min-w-0 max-w-full max-xl:-my-2.5`;
/** The same trigger in `badgeSelectRailWrapper` (the issue header's repository), which is 28 px
 *  from `md`: there the 44 px trigger overhangs it by 8 px above and below, so the rail's line is
 *  28 px from `md` (`md:min-h-7`), the height of the rail's other controls. */
export const badgeSelectRailTrigger = `${badgeSelectTriggerFrame} md:max-xl:-my-2`;

/** The handlers a badge-select wrapper spreads when its list reads its options only once the
 *  reader reaches for it (the assignee, the repository, an ask card's hand-on): a press on the
 *  trigger, or in the list it opens (portalled, but a React child of the wrapper), is kept out
 *  of the row, card or header under it, and the pointer over the wrapper, a touch on it or the
 *  focus in it calls `want`, which starts that read. Nothing here makes the wrapper interactive,
 *  so it needs no role of its own; and every open panel reads a press in the capture phase
 *  (`AnchoredPanel`), so stopping it here hides it from none of them. */
export function badgeReachHandlers(want: () => void) {
  return {
    onFocus: want,
    onMouseDown: (event: SyntheticEvent) => event.stopPropagation(),
    onPointerDown: (event: SyntheticEvent) => event.stopPropagation(),
    onPointerEnter: want,
    onTouchStart: (event: SyntheticEvent) => {
      event.stopPropagation();
      want();
    },
  };
}
