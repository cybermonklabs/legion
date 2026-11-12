import {
  activeTabIndicatorBorder,
  activeTabIndicatorText,
  borderDefault,
  textSecondaryOnCanvas,
} from "../theme/classes";

/**
 * What a tab strip is made of, in one place, because two surfaces draw the same strip: `Tabs`
 * itself, and the box the issue page keeps for it while its tabs are still to come
 * (`features/issue/issue-page-skeleton.tsx`, dispatch://INFRA-341 R11). A placeholder drawn at
 * the strip's size is that size only while both read the same classes; a copy of them drifts
 * the first time one changes, and the strip then moves the document under the reader as it
 * lands.
 *
 * Compact tabs sit tighter on phones, which is what keeps the shorter tablists on one line.
 */

/** The strip itself: the row the tablist and anything trailing it sit in. */
export function tabStripClassName(compact: boolean): string {
  return `${compact ? "mb-0" : "mb-3"} flex flex-wrap items-center gap-1 border-b ${borderDefault}`;
}

/** The tablist inside it, which scrolls sideways where the tabs are wider than the page. */
export function tabListClassName(compact: boolean): string {
  return `flex min-w-0 ${compact ? "gap-0 md:gap-1" : "gap-1"} overflow-x-auto [scrollbar-width:thin]`;
}

/** The box whatever trails the tablist stands in: the Spec toolbar on the issue page
 *  (`features/issue/SpecToolbar.tsx`), and the space the issue page keeps for it
 *  (`features/issue/issue-page-skeleton.tsx`).
 *
 *  It is a row of its own below `md` - `w-full`, so the tablist keeps the row above it - and
 *  sits at the end of the tab row from `md` up. The space reads this class for that reason: a
 *  trailing box drawn inline in the space and on its own row in the strip is a space one row
 *  short on every phone, which is the viewport the space was written for (dispatch://INFRA-341).
 */
export const tabTrailingClassName =
  "flex w-full min-w-0 flex-wrap items-center gap-2 md:ml-auto md:w-auto";

/** One tab. The selected one carries a 2 px underline and a heavier weight, neither of which
 *  changes its height: the minimum is 44 px and the content under it stands at 38 px. */
export function tabClassName(compact: boolean, selected: boolean): string {
  const minHeight = compact ? "min-h-11 md:min-h-9" : "min-h-11";
  const padding = compact ? "px-1.5 md:px-2" : "px-3";
  const box = `${minHeight} ${padding} inline-flex shrink-0 items-center gap-1.5 py-2 text-sm whitespace-nowrap`;
  return selected
    ? `${box} border-b-2 font-semibold ${activeTabIndicatorBorder} ${activeTabIndicatorText}`
    : `${box} ${textSecondaryOnCanvas}`;
}

/** The box a tab's count pill stands in: 24 px, the `Pill`'s own `px-2 py-1 text-xs` height,
 *  pinned here rather than left to that component.
 *
 *  A count is the tallest thing a compact tab holds - its label is a `text-sm` line of 20 px -
 *  so it, not the label, is what a compact strip's 40 px is. The box the issue page keeps for
 *  its strip reads this same class (`features/issue/issue-page-skeleton.tsx`), so the two
 *  cannot disagree and the strip lands in a box of its own height. Without it the box stood at
 *  37 px against the strip's 41 px, and the whole page under it moved 4 px when the strip
 *  landed (dispatch://INFRA-341).
 */
export const tabCountClassName = "h-6";
