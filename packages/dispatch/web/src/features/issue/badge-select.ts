import type { SyntheticEvent } from "react";

import { hasFocusVisibleRing } from "../../theme/classes";

const badgeSelectFrame = "relative inline-flex min-h-11 shrink-0 items-center rounded-sm";

/** The "tinted badge whose press opens a list" wrappers: the wrapper is the 44 px tap target
 *  (32 px from `md`) and draws the keyboard focus ring of the shared list's single pick inside
 *  it, whose trigger shows the badge (the priority, the issue header's assignee, an ask card's
 *  hand-on, an Inbox Snooze: `badgeSelectTrigger`). */
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
/** `badgeSelectBadge` for a value that can be longer than its row - today's Repository list in
 *  its row of the issue header's Details: a block of one line, as wide as its text up to its
 *  row's width, that ends what does not fit in an ellipsis. A flex box's own text takes no
 *  ellipsis (`TruncatedText`), so this badge is a block, and the caller passes its whole text as
 *  its `title`. */
export const badgeSelectTruncatedBadge =
  "inline-block max-w-full truncate rounded-sm px-1.5 py-0.5 align-middle text-xs font-semibold";
/** Inside `NoTooltips` (the new issue page) every value a shared control draws wraps: it keeps
 *  `max-w-full` and breaks anywhere, and nothing around it keeps its width, so a long login with
 *  no space stays inside Details' row rather than running past it. These are
 *  `badgeSelectWrapper` in that shape, with `min-w-0 max-w-full` in place of `shrink-0`, and
 *  `badgeSelectBadge` with `max-w-full [overflow-wrap:anywhere]`. The list's trigger over the
 *  badge stays usable however many lines the badge takes: it stretches to the wrapper's height
 *  and narrows with it (`badgeSelectPageTrigger`), so it covers every line of a badge that
 *  wraps. The wrapper and the trigger also hold today's Repository list in its row of the issue
 *  header's Details (`RepoControl.tsx`, `RepositoryList`), which narrows to the row the same way;
 *  there the badge ends a long name in an ellipsis (`badgeSelectTruncatedBadge`), with the whole
 *  name as its title, rather than wrap, since a `title` can give back what it cut. */
const badgeSelectPageFrame =
  "relative inline-flex min-h-11 min-w-0 max-w-full items-center rounded-sm";
export const badgeSelectPageWrapper = `${badgeSelectPageFrame} md:min-h-8 has-focus-visible:ring-2 ${hasFocusVisibleRing}`;
export const badgeSelectPageBadge = `${badgeSelectBadge} max-w-full [overflow-wrap:anywhere]`;
/** The trigger of a list laid over the badge, filling its wrapper's tap target, leaving the
 *  focus ring to the wrapper and showing a held trigger's press as refused; each wrapper's
 *  overhang is added below. */
const badgeSelectTriggerFrame =
  "flex cursor-pointer items-center self-stretch rounded-sm outline-none refused:cursor-not-allowed";
/** The trigger of a list laid over the badge in `badgeSelectWrapper` (the priority, the issue
 *  header's assignee, the ask card's hand-on, an Inbox Snooze): it shows the badge. Below
 *  1280 px every button is at least 44 px tall (`styles.css`), and from `md` the wrapper is
 *  32 px: there the trigger overhangs it by 6 px above and below rather than make its line 12 px
 *  taller, so the line is the height it was when a native select lay over the badge, and the
 *  trigger is still the 44 px target that select was. */
export const badgeSelectTrigger = `${badgeSelectTriggerFrame} md:max-xl:-my-1.5`;
/** The same trigger in `badgeSelectPageWrapper`, which is `badgeSelectWrapper`'s height: it
 *  narrows with its wrapper, so the badge inside it wraps within the line. */
export const badgeSelectPageTrigger = `${badgeSelectTrigger} min-w-0 max-w-full`;
/** The same trigger in `badgeSelectHeaderWrapper`, which is 24 px at every width: below 1280 px
 *  the 44 px trigger overhangs it by 10 px above and below, so the header's line stays 24 px and
 *  the trigger is the 44 px target the select laid over it was; from 1280 px it is the wrapper's
 *  height. It narrows with its wrapper, so the value inside it wraps within the line. */
export const badgeSelectHeaderTrigger = `${badgeSelectTriggerFrame} min-w-0 max-w-full max-xl:-my-2.5`;

/** The handlers a badge-select wrapper spreads. A press on the trigger, or in the list it opens
 *  (portalled, but a React child of the wrapper), is kept out of the row, card or header under
 *  it. Where the list reads its options only once the reader reaches for it (the assignee, the
 *  repository, an ask card's hand-on), the pointer over the wrapper, a touch on it or the focus
 *  in it calls `want`, which starts that read; the priority's options are fixed, and it passes
 *  none. Nothing here makes the wrapper interactive, so it needs no role of its own; and every
 *  open panel reads a press in the capture phase (`AnchoredPanel`), so stopping it here hides it
 *  from none of them. */
export function badgeReachHandlers(want?: () => void) {
  return {
    onFocus: want,
    onMouseDown: (event: SyntheticEvent) => event.stopPropagation(),
    onPointerDown: (event: SyntheticEvent) => event.stopPropagation(),
    onPointerEnter: want,
    onTouchStart: (event: SyntheticEvent) => {
      event.stopPropagation();
      want?.();
    },
  };
}
