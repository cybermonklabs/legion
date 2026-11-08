import type { ReactNode } from "react";

import { tabClassName, tabListClassName, tabStripClassName } from "../../components/tab-classes";
import {
  borderDefault,
  borderStrong,
  borderTransparent,
  linkText,
  skeletonBg,
  textTransparent,
} from "../../theme/classes";
import { ISSUE_TAB_LABELS } from "./issue-tab-list";

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
 * Here rather than in `app.tsx` because both draw them: `app.tsx` is the entry chunk, which may
 * hold nothing a route draws, and these two boxes are all of the page that belongs there.
 *
 * `state` is the one thing in the header's box that is not a placeholder: what the header
 * would tell the reader about this issue without being asked - that it is closed, where its
 * work stands, and what a review of its spec has decided (`IssuePage.tsx`). A reader presses
 * the box for a control and has the header then, which is no use to one who was never told
 * there was anything to press for, so the notices stand in it and the controls wait.
 */
export function IssueHeaderSkeleton({ state }: { state?: ReactNode }): ReactNode {
  return (
    <header className={`mb-6 border-b pb-6 ${borderDefault}`} data-testid="issue-header-box">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className={`text-sm font-semibold ${linkText}`}>
          <span
            className={`inline-block h-4 w-16 animate-pulse rounded align-middle ${skeletonBg}`}
          />
        </p>
        <button
          className={`rounded-lg border px-3 py-2 text-sm font-medium ${textTransparent} ${borderStrong}`}
          disabled
          type="button"
        >
          Pin issue
        </button>
      </div>
      <h1
        className={`mt-2 w-full rounded-lg border px-2 py-1 text-2xl font-semibold ${borderTransparent}`}
      >
        <span
          className={`inline-block h-7 w-2/3 animate-pulse rounded align-middle ${skeletonBg}`}
        />
      </h1>
      {state === undefined ? null : (
        <div className="mt-2 flex flex-wrap items-center gap-2">{state}</div>
      )}
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
 * Each tab is a pulsing box the width of its own name rather than a tab that reads as one: the
 * strip is what the reader presses, and nothing there answers a press or a screen reader.
 *
 * `trailing` is the one thing in this box that is not a placeholder: the open document's
 * connection dot, which the strip itself carries in the same place (`Tabs`'s own `trailing`,
 * inside `SpecToolbar`). It is the document's own state rather than the page's chrome, and the
 * moment a reader most needs it - the document is not connected and their edits are in this
 * browser alone - is exactly the moment no first batch comes to draw the strip
 * (`features/doc/first-batch.ts`, dispatch://INFRA-341 R11).
 */
export function IssueTabsSkeleton({ trailing }: { trailing?: ReactNode }): ReactNode {
  return (
    <div className={tabStripClassName(true)} data-testid="issue-tabs-box">
      <div aria-hidden="true" className={tabListClassName(true)}>
        {ISSUE_TAB_LABELS.map((label) => (
          <span className={tabClassName(true, false)} key={label}>
            <span className={`animate-pulse rounded ${textTransparent} ${skeletonBg}`}>
              {label}
            </span>
          </span>
        ))}
      </div>
      {trailing}
    </div>
  );
}
