import type { ReactNode } from "react";

import {
  tabClassName,
  tabCountClassName,
  tabListClassName,
  tabStripClassName,
  tabTrailingClassName,
} from "../../components/tab-classes";
import {
  borderTransparent,
  calloutWarningBg,
  calloutWarningBorder,
  calloutWarningText,
  card,
  skeletonBg,
  textTransparent,
} from "../../theme/classes";
import {
  issueClosedCalloutClassName,
  issueHeaderFrameClassName,
  issueKeyBadgeClassName,
  issueTitleBoxClassName,
} from "./issue-header-classes";
import { issueTabs } from "./issue-tab-list";

/**
 * The two pieces of the issue page a reader has before the page itself: the header's box and the
 * tab strip's.
 *
 * They are drawn twice, and the same both times. `IssuePageFallback` (`app.tsx`) draws them while
 * the page's chunk is still arriving, and the page draws them again around its header and its
 * tabs, each of which arrives in a commit of its own now rather than in the page's one mount
 * commit (`IssuePage.tsx`, dispatch://INFRA-341 R11). On a document address that commit waits
 * for the editor's first batch, so the box each keeps is the box a reader reads for seconds
 * rather than for a frame (`features/doc/AfterTheFirstBatch.tsx`). So nothing the reader has not
 * already seen appears between the skeleton and the page.
 *
 * Each box stands at the height of the part that lands in it, row by row, from that part's own
 * classes. Measured on this pod at 1280x720 on an issue's Conversation, where a comment's
 * `dispatch://` reference sat 483 px down the screen: the header's space stood at 177 px against
 * the header's 146 px and the strip's at 37 px against the strip's 41 px, the document lost
 * 27 px as the two landed 117 ms after that reference was first painted, the browser clamped the
 * scroll by 7 px of it, and the reference went up 20 px - out from under a pointer resting on it
 * (`e2e/heavy-bodies.e2e.ts`'s hover card of a comment of 8,000 list items, dispatch://INFRA-341).
 *
 * Here rather than in `app.tsx` because both draw them: `app.tsx` is the entry chunk, which may
 * hold nothing a route draws, and these two boxes are all of the page that belongs there. That
 * is also why each box is built from the classes the part is built from rather than from the
 * part itself: `components/tab-classes.ts` and the sizes named below carry no component into the
 * shell's own task.
 *
 * `state` is the one thing in the header's box that is not a placeholder: what the header
 * would tell the reader about this issue without being asked - where its work stands, and what
 * a review of its spec has decided (`IssuePage.tsx`). A reader presses the box for a control
 * and has the header then, which is no use to one who was never told there was anything to
 * press for, so the notices stand in it and the controls wait.
 *
 * `isClosed` is the third of those notices, and it is a block of its own rather than a word in
 * `state`, because that is what the header draws for it: a full-width callout above the three
 * rows, holding the sentence and the Reopen button. Said as a pill inside the state row
 * instead, it stood in the height the status pick already kept and the box held no pixel for
 * the block - about 76 px of header that landed on a reader sitting at the top of a closed
 * issue, where `keepWhatStandsUnder` compensates nothing: it answers only a box already
 * scrolled above the top of the screen (`features/doc/readers-place.ts`, dispatch://INFRA-341).
 */
export function IssueHeaderSkeleton({
  isClosed = false,
  state,
}: {
  isClosed?: boolean;
  state?: ReactNode;
}): ReactNode {
  return (
    // The header's own frame, from the one place both are built from
    // (`issue-header-classes.ts`): 26 px of border and padding and 12 px of margin under it.
    <header className={`${issueHeaderFrameClassName} ${card}`} data-testid="issue-header-box">
      {/* The closed block, at the header's own size: the same callout box from the one place
          both are built from (`issue-header-classes.ts`), around the same sentence, and the
          Reopen button's own 44 px box kept beside it (`IssueHeader.tsx`). The button itself
          waits with the other controls - a press anywhere here hands the reader the header, and
          Reopen comes with it. */}
      {isClosed ? (
        <div
          className={`${issueClosedCalloutClassName} ${calloutWarningBorder} ${calloutWarningBg} ${calloutWarningText}`}
        >
          <span>This issue is closed.</span>
          <span
            className={`block min-h-11 w-24 animate-pulse rounded-lg ${skeletonBg}`}
            data-testid="issue-reopen-space"
          />
        </div>
      ) : null}
      {/* The header's three rows, in the header's own wrapper: below 2xl its title block is
          `basis-full`, so the title, the state controls and the details line each take a row of
          their own, and the box keeps all three. */}
      <div className="flex flex-wrap items-center gap-2">
        <div className="flex min-w-0 grow basis-full flex-col gap-2 sm:flex-row sm:items-start 2xl:basis-auto">
          {/* The key badge's box: the header's own `px-2.5 py-1.5` around a 20 px line, so
              32 px. The placeholder inside is a block, not an inline box, so the line it makes
              is exactly the line the key's own text makes. */}
          <div className="flex shrink-0 items-center gap-1 self-start">
            <p className={issueKeyBadgeClassName}>
              <span className={`block h-5 w-14 animate-pulse rounded ${skeletonBg}`} />
            </p>
          </div>
          <div className="flex min-w-0 flex-1 items-start gap-2">
            {/* The title's box: the heading's own border and padding around its 28 px line. */}
            <h1 className={`${issueTitleBoxClassName} ${borderTransparent}`}>
              <span className={`block h-7 w-2/3 animate-pulse rounded ${skeletonBg}`} />
            </h1>
            {/* The pin's box, at `components/PinButton.tsx`'s own size. It answers no press:
                a press anywhere in this box hands the reader the header itself
                (`ChromeAfterTheFirstBatch`), and the control they pressed for comes with it.

                `aria-disabled` rather than `disabled`, so it is the one thing in this box a Tab
                reaches. A `disabled` button is out of the tab order, and with the tab strip's
                placeholders `aria-hidden` the whole held box held nothing focusable: a reader
                with no pointer tabbed straight past the header and the strip and never fired
                the focus that hands them over, for as long as the hold lasted
                (`features/doc/AfterTheFirstBatch.tsx`, dispatch://INFRA-341). */}
            <button
              aria-disabled="true"
              aria-label="Pin issue"
              className="min-h-11 min-w-11 shrink-0 rounded-lg md:min-h-8 md:min-w-8"
              type="button"
            />
          </div>
        </div>
        {/* The state controls' row, at the size the header's own status pick stands at
            (`min-h-11 md:min-h-8`), so the row is there whether or not `state` fills it. */}
        <div className="flex min-h-11 w-full min-w-0 flex-wrap items-center gap-2 md:min-h-8 md:w-auto">
          {state}
        </div>
        {/* The details line's row. The header's own never wraps - it scrolls sideways instead
            (`flex-nowrap overflow-x-auto`) - so its height is its controls' and nothing else:
            44 px, and 28 px from md, where `md:[&_button]:min-h-7` takes over. */}
        <div className="flex h-11 w-full items-center gap-2 md:h-7">
          <span className={`h-4 w-28 animate-pulse rounded ${skeletonBg}`} />
          <span className={`h-4 w-40 animate-pulse rounded ${skeletonBg}`} />
        </div>
      </div>
    </header>
  );
}

/**
 * The tab strip's box, at the strip's own size: the same classes `Tabs` draws itself with
 * (`components/tab-classes.ts`) around a box per tab the issue has, so the strip landing in it
 * moves the document under it by no pixels.
 *
 * The tabs it names are `issue-tab-list.ts`, the list `IssueTabs` renders. A copy of them here
 * had already drifted by three tabs, and a box naming other tabs than the strip is a box of the
 * wrong width. That module is data and types alone, so reading it costs this box none of the
 * tablist's own weight: nothing of `Tabs` or the `Pill` it draws counts with follows the list
 * into the commit this box exists to keep them out of.
 *
 * `artifactCount` is read for one thing: whether a tab of this issue carries a count. A count
 * is the tallest thing a tab holds (`tabCountClassName`, 24 px against a label's 20 px), so a
 * strip with one stands at 40 px and a strip without one at 36, and the box has to be the
 * issue's own. `IssuePageFallback` has read no issue yet and passes none; it keeps the box for
 * a page whose only content below is this skeleton's own rows.
 *
 * Each tab is a pulsing box the width of its own name rather than a tab that reads as one: the
 * strip is what the reader presses, and nothing there answers a press or a screen reader.
 *
 * `trailing` is the one thing in this box that is not a placeholder: the open document's
 * connection dot, which the strip itself carries in the same place (`Tabs`'s own `trailing`,
 * inside `SpecToolbar`). It is the document's own state rather than the page's chrome, and the
 * moment a reader most needs it - the document is not connected and their edits are in this
 * browser alone - is exactly the moment no first batch comes to draw the strip
 * (`features/doc/first-batch.ts`, dispatch://INFRA-341 R11).
 *
 * It stands in the toolbar's own box (`tabTrailingClassName`) at the toolbar's own height,
 * because that is where the strip puts it: below `md` the toolbar is `w-full` and takes a row
 * of its own under the tabs, a `min-h-11` select and the row's `gap-1`, and a dot drawn inline
 * there left all of it unkept - 48 px of strip landing on the reader, on the very viewport
 * this box is written for.
 */
export function IssueTabsSkeleton({
  artifactCount = 0,
  trailing,
}: {
  artifactCount?: number;
  trailing?: ReactNode;
}): ReactNode {
  return (
    <div className={tabStripClassName(true)} data-testid="issue-tabs-box">
      <div aria-hidden="true" className={tabListClassName(true)}>
        {issueTabs(artifactCount).map((tab) => (
          <span className={tabClassName(true, false)} key={tab.id}>
            <span className={`animate-pulse rounded ${textTransparent} ${skeletonBg}`}>
              {tab.label}
            </span>
            {tab.count === undefined ? null : (
              <span
                className={`${tabCountClassName} w-6 shrink-0 animate-pulse rounded-full ${skeletonBg}`}
              />
            )}
          </span>
        ))}
      </div>
      {trailing === undefined ? null : (
        <div className={`${tabTrailingClassName} min-h-11 md:min-h-8`}>{trailing}</div>
      )}
    </div>
  );
}
