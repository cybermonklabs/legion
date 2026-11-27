import { lazy, type ReactNode, Suspense } from "react";
import { useLocation } from "react-router-dom";

import { routeHasMargin } from "../refs/routes";
import { COMPACT_VIEWPORT_QUERY, useMediaQuery } from "../shell/useDialog";
import type { MarginProps } from "./Margin";
import { marginColumnWidth } from "./margin-width";

/**
 * Where the shell places the margin, and where the margin's own code is fetched: after the first
 * paint, not before it.
 *
 * The margin is mounted on every route, and draws nothing on most of them - it returns null on a
 * desktop route that has no margin (`Margin.tsx`) - but it carries the comment composer, the
 * thread cards and the Markdown surfaces under them, a tenth of what the entry chunk used to
 * make every first page load parse. Nothing of it is on any route's first frame, so the shell
 * loads it the way `features/doc/editor.ts` loads the editor: behind `import()`, beside the
 * component it loads rather than in the shell, since the margin's rule for what it draws is the
 * margin's own.
 *
 * It stays mounted once it arrives, as it always was: the tab, the open thread and the composer
 * the reader left behind on an issue are still there when they come back to it
 * (`useMarginSheet.ts`).
 */
const Margin = lazy(() => import("./Margin").then((module) => ({ default: module.Margin })));

/**
 * The column the margin will fill, held open at the margin's own width until its chunk arrives.
 *
 * Nothing of the margin is on the first frame, but its column is: a 384 px column on a 1,280 px
 * viewport is 30 % of the document's width, and a fallback that drew nothing left the document
 * laid out 912 px wide and then re-wrapped every line of it under the reader when the margin
 * landed beside it a third of a second later (dispatch://INFRA-341). So the frames before the
 * chunk arrives hold exactly the box the frames after it hold, and the document is drawn once,
 * at the width it keeps.
 *
 * It reserves the width the margin will take and nothing anywhere else, from the same three
 * answers `Margin` itself renders from: a compact viewport draws the margin as a bottom sheet
 * over the document rather than a column beside it, a desktop route with no margin draws no
 * column at all, and a margin collapsed to its rail hides its column and the shell pads the
 * gutter for the rail instead. The width is `marginColumnWidth`, the number `Margin` lays its
 * own column out at.
 *
 * `data-margin-pending` is the mark a page still settling wears: this is a column with no
 * margin in it yet, so the review and the accessibility layers wait for it to go before they
 * read the page, and read the real margin rather than an empty box
 * (`e2e/review-routes.ts`, beside `data-markdown-pending`).
 *
 * The shell renders it itself on the frames before the route is drawn, where `MarginColumn`
 * cannot be mounted at all: a suspended boundary beside the route's own costs the page a
 * 300 ms fallback throttle, which is why the margin's chunk waits for `TheRouteIsDrawn`
 * (`web/src/app.tsx`). This suspends on nothing, so the column is held from the shell's first
 * commit and the chunk arrives into a box that is already there. It is exported for
 * `web/src/__tests__/responsive-shell.test.tsx` as well, which renders it rather than the
 * shell: React's `lazy` resolves once per process, so a shell render shows this as the
 * `Suspense` fallback only when it is the first in its file to reach the margin.
 */
export function ReservedMarginColumn({ collapsed = false, width }: MarginProps): ReactNode {
  const isCompactViewport = useMediaQuery(COMPACT_VIEWPORT_QUERY);
  const { pathname, search } = useLocation();
  if (isCompactViewport || collapsed || !routeHasMargin(pathname, search)) {
    return null;
  }
  return (
    <div
      className="order-3 shrink-0"
      data-margin-pending=""
      data-testid="margin-reservation"
      style={{ width: `${marginColumnWidth(width)}px` }}
    />
  );
}

export function MarginColumn(props: MarginProps): ReactNode {
  return (
    <Suspense fallback={<ReservedMarginColumn {...props} />}>
      <Margin {...props} />
    </Suspense>
  );
}
