/**
 * The boxes the issue header and the space kept for it are both built from.
 *
 * The space has to stand at the header's own height, row by row, or the document under it moves
 * when the header lands - 27 px of it, and 20 px of slide under a resting pointer, measured on
 * this pod (`issue-page-skeleton.tsx`, dispatch://INFRA-341). It was held to that by class
 * strings copied from `IssueHeader.tsx` into two other files, which the next change to the
 * header would have parted silently. The tab strip already had the remedy
 * (`components/tab-classes.ts`); this is the header's.
 *
 * Each string is the box alone - the border, the padding, the line height and the breakpoints,
 * and the font of a line wherever the font decides the box: how wide the key badge is, so where
 * the title starts, and how many lines the title wraps to. It is never the paint: the header adds
 * its own colours and controls over these, and the space adds its pulsing placeholders. Data and
 * strings only, as the tab list is, so the shell's own chunk takes nothing of the header with
 * them.
 */

/** The header's frame: 26 px of border and padding, and 12 px of margin under it. */
export const issueHeaderFrameClassName = "mb-3 min-w-0 rounded-xl border p-3";

/** The key badge's box: `px-2.5 py-1.5` around a 20 px line, so 32 px. */
export const issueKeyBadgeClassName = "rounded-md px-2.5 py-1.5";

/** The key's own line inside that badge. Its glyphs are the badge's width, and the badge's width
 *  is where the title starts, so the space draws the key in this font too. */
export const issueKeyTextClassName = "font-mono text-sm font-semibold";

/** The title heading's box: its border and padding around its 28 px line. */
export const issueTitleBoxClassName = "min-w-0 flex-1 rounded-lg border px-2 py-1 md:py-0";

/** The title's own line inside that box. The title wraps, so its font is its height: a bar where
 *  the issue's own title stands was a space one line short at 1280 px and two at 390 px on this
 *  pod, and the document moved by those lines as the header landed (dispatch://INFRA-341). */
export const issueTitleTextClassName =
  "break-words text-[22px] leading-7 font-semibold tracking-tight";

/** The closed issue's callout: the block above the header's three rows holding the sentence and
 *  Reopen, which the space keeps at that height too (`issue-page-skeleton.tsx`). */
export const issueClosedCalloutClassName =
  "mb-2 flex flex-wrap items-center justify-between gap-2 rounded-lg p-3 text-sm";

/** The status pill's box, which the header's own status pick stands in and the space's pill
 *  repeats, so the pick landing in its place moves nothing beside it. */
export const issueStatusPillClassName =
  "flex min-h-11 shrink-0 items-center gap-2 rounded-full border px-2 text-sm font-semibold uppercase tracking-wide sm:px-3 md:min-h-8 xl:px-2";

/** The dot inside that pill, which only a screen wide enough for it draws. */
export const issueStatusPillDotClassName = "hidden h-2 w-2 rounded-full sm:block";

/** The status pick inside the pill. A `<select>` is as wide as its widest option, not as its
 *  value, so the space draws the same element with the same options to stand at that width. */
export const issueStatusSelectClassName =
  "min-w-0 appearance-none font-inherit outline-none disabled:cursor-not-allowed disabled:opacity-50";

/** Close and Release, the two buttons the state row ends with. Each is as wide as its word, and
 *  the row wraps at a phone width, so the space draws the word at this size too. */
export const issueStateButtonClassName =
  "min-h-11 shrink-0 rounded-lg px-2 py-2 text-sm font-medium sm:px-3 md:min-h-8 md:py-1 xl:px-2";
