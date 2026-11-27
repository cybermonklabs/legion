import type { Page } from "@playwright/test";

import { gotoIssue } from "./issue-page";

/** Waits two rendering frames in `page`. Whatever the browser has just been given - a key press,
 *  a click, a viewport change - its own handler and the render React queued for it are both done
 *  within them, so a row that reads the page after this reads what that event produced rather
 *  than what was on screen before it. */
export function nextFrames(page: Page): Promise<void> {
  return page.evaluate(() => {
    const frames = Promise.withResolvers<void>();
    requestAnimationFrame(() => requestAnimationFrame(() => frames.resolve()));
    return frames.promise;
  });
}

/**
 * Waits out what a resize of `page` to `width` starts, so a row that reaches a width by a resize
 * reads what a load at that width draws. The caller has set the viewport. The wait runs to the
 * fourth frame after the first frame that draws the window at the new width (R), whose rendering
 * fires the resize event and each width query's change (`useMediaQuery`) before its frame
 * callbacks:
 *
 * 1. before R+1, React has drawn what those events asked for - the rail, the Contents pane, the
 *    view row's controls - and the observers of the band's and the view row's heights
 *    (`identity-block.ts`, `slots/view-row.tsx`) have published what R's layout measured; the
 *    header's labels, on an issue that has them, saw R's layout and read the line on R+1
 *    (`IssuePropertiesRow.tsx`);
 * 2. before R+2, that reading is drawn, and on R+1 an observer saw the layout of any render
 *    that landed after R;
 * 3. before R+3, the reading that observer asked for on R+2 is drawn;
 * 4. before R+4, a second reading is drawn where the first changed the labels' box's own width
 *    (seen on R+2, read on R+3).
 *
 * On a phone the window can stay wider than the viewport where the page overflows it, so the
 * wait for the first frame at the new width is bounded.
 */
export async function resizeSettled(page: Page, width: number): Promise<void> {
  await page.evaluate(async (width) => {
    const frame = () => {
      const drawn = Promise.withResolvers<void>();
      requestAnimationFrame(() => drawn.resolve());
      return drawn.promise;
    };
    for (let frames = 0; frames < 30; frames += 1) {
      await frame();
      if (window.innerWidth === width) break;
    }
    for (let frames = 0; frames < 4; frames += 1) await frame();
  }, width);
}

/** A viewport, as `page.setViewportSize` takes it. Each family of rows picks the height it reads
 *  a width at with a `viewportAt` of its own (`issue-page.ts`, `issue-page-p.ts`). */
export interface Viewport {
  readonly height: number;
  readonly width: number;
}

/**
 * The widths the new issue page's layout switches at, each the first width of its new side:
 *
 * - 761 px: the header stops scrolling away, its `min-[761px]:` classes and the view row's
 *   `max-[761px]:` ones switch, and Details stops shading the page (`HEADER_SCROLLS_QUERY`,
 *   `identity-block.ts`; `IssueHeader.tsx`, `IssuePropertiesRow.tsx`, `IssueTitleRow.tsx`);
 * - 768 px: the rail and the page's room for it (`MD_VIEWPORT_QUERY`, `shell/useDialog.ts`, read
 *   as `railFits` in `frame-shell.tsx`, and its `md:pl-14`);
 * - 1024 px: the Contents pane and, below it, the Outline control (`CONTENTS_PANE_QUERY`,
 *   `contents-frame.tsx`);
 * - 1241 px: the pane's 260 px width (`min-[1241px]:w-[260px]`, `contents-frame.tsx`);
 * - 1280 px: the view row's five controls, the actions menu's desktop rows and Details as a
 *   drawer (`ONE_CONTROL_QUERY`, `IssuePresets.tsx`; `IssueActionsMenu.tsx`;
 *   `COMPACT_VIEWPORT_QUERY`, `shell/useDialog.ts`).
 *
 * A walk of widths through `openAt` loads at each width across one of them from the width before
 * it (`acrossABreakpoint`), and resizes (`resizeTo`) only between widths on the same side of
 * every one: a chunk the page loads decides part of the layout, and what a load at a width draws
 * is not always what a resize to that width draws. While the margin's chunk was on its way after
 * a load, the shell held a column for it that narrowed the page (381b29ad), which a resize, with
 * the chunk long in, never showed. The rule is the walks' alone: rows that set the viewport
 * themselves (`page.setViewportSize`) resize across a breakpoint too, in `issue-page-d.e2e.ts`,
 * `issue-page-g.e2e.ts`, `issue-page-i.e2e.ts`, `issue-page-k.e2e.ts` and `issue-page-p.e2e.ts`
 * itself, and are not listed here.
 *
 * Three walks, all in `issue-page-p.e2e.ts`, cross one by a resize, each where nothing it reads
 * after the resize turns on how the page reached the width:
 *
 * - "the header holds its list and its limits at every width", whose first pass loads at 390 px
 *   and resizes to 1440 px on a desktop project. What that pass reads at 1440 px - the header's
 *   limits with no approval request waiting - is read on a load at 1440 px too, by row (3) of
 *   that file, and the row's second pass loads at 1440 px itself with a request waiting.
 * - "the press that ends a title edit reaches the control under it", which on chromium loads at
 *   1440 px and resizes to 390 px only to measure which titles take two lines and three. Each
 *   press it asserts is made on a load of its own at that width, after the title is written; and
 *   a resize that drew the heading at another width would hand those loads titles of the wrong
 *   length, which the row's read of the editor's lines on each load then fails rather than passes.
 * - "the labels' count names every label the line leaves out, at every width of the line", which
 *   loads at 1440 px and resizes to 1024 px. Its walk sets the line's width itself, pixel by pixel
 *   from the width the page drew, and at each one reads the chips drawn whole against the count:
 *   a page that drew the line at another width would move where the walk starts, not what holds
 *   at each width, and the labels read the line again on every change of its width.
 */
export const ISSUE_PAGE_BREAKPOINTS = [761, 768, 1024, 1241, 1280] as const;

/** Whether a page at `from` px brought to `to` px meets one of `ISSUE_PAGE_BREAKPOINTS` on the
 *  way, so a row brings it there by a load. */
export function acrossABreakpoint(from: number, to: number): boolean {
  return ISSUE_PAGE_BREAKPOINTS.some(
    (breakpoint) => Math.min(from, to) < breakpoint && breakpoint <= Math.max(from, to)
  );
}

/** Sets the viewport to `size` and waits out what the resize starts (`resizeSettled`), so a row
 *  that reaches a width by a resize reads what the width queries draw there. That wait ends on
 *  R+4, the fourth frame after the first that draws the new width, which is the latest frame the
 *  labels' walk (`issue-page-p.e2e.ts`, the labels' count row) reads a width at: so a resize reads
 *  the line no sooner than the walk does. */
export async function resizeTo(page: Page, size: Viewport): Promise<void> {
  await page.setViewportSize(size);
  await resizeSettled(page, size.width);
}

/**
 * Brings the issue page at `path` to `size`: a load at that size (`gotoIssue`) where `load` says
 * so, else a resize of the page already open (`resizeTo`). `ISSUE_PAGE_BREAKPOINTS` says which.
 *
 * A load from a page of another size sends that page to `about:blank` before the resize, so the
 * page it leaves is not laid out and drawn again at the new size just before the load throws it
 * away: on an issue of some ninety rows that cost 0.2 to 0.5 s a resize, and the load reads
 * nothing of the page it replaces. A page already at `size` loads at once.
 */
export async function openAt(
  page: Page,
  size: Viewport,
  path: string,
  load: boolean
): Promise<void> {
  if (!load) {
    await resizeTo(page, size);
    return;
  }
  const drawn = page.viewportSize();
  if (drawn?.height !== size.height || drawn.width !== size.width) {
    await page.goto("about:blank");
    await page.setViewportSize(size);
  }
  await gotoIssue(page, path);
}
