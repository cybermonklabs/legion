import { lazy, type ReactNode, Suspense } from "react";
import { useLocation } from "react-router-dom";

import { routeHasMargin } from "../refs/routes";
import { useContentDrawn } from "../shell/content-drawn";
import { renderChunk } from "../shell/DeploymentResilience";
import { COMPACT_VIEWPORT_QUERY, useMediaQuery } from "../shell/useDialog";

import type { MarginProps } from "./Margin";
import { clampMarginWidth, DEFAULT_MARGIN_WIDTH } from "./margin-width";

/**
 * Where the shell places the margin, and where the margin's own code is fetched: after the page
 * the reader landed on has drawn its content, not before it.
 *
 * The margin is mounted on every route, and draws nothing on most of them - it returns null on a
 * desktop route that has no margin (`Margin.tsx`) - but it carries the comment composer, the
 * thread cards and the Markdown surfaces under them, a tenth of what the entry chunk used to
 * make every first page load parse. Nothing of it is on any route's first frame, so the shell
 * loads it the way `features/doc/editor.ts` loads the editor: behind `import()`, beside the
 * component it loads rather than in the shell, since the margin's rule for what it draws is the
 * margin's own.
 *
 * On a landing whose page draws a live document, "after the first paint" is still before the
 * content, so the margin waits for the document's first block as well (`useContentDrawn`,
 * `features/shell/content-drawn.ts`): the landing's own measure of an issue page or a document
 * page is that block (`e2e/landing-reads.e2e.ts`), and the margin is beside it, not part of it.
 * While it waits, and while its chunk comes down, the page holds the margin's empty column
 * (`MarginFrame`), so the document is drawn at the width it keeps once the margin is in.
 *
 * It stays mounted once it arrives, as it always was: the tab, the open thread and the composer
 * the reader left behind on an issue are still there when they come back to it
 * (`useMarginSheet.ts`).
 */
// Under `renderChunk`, which tells the chunk-failure recovery that a reader is waiting on this
// download: the margin is mounted by the route, not prefetched by the entry, so a failure here
// must spend the tab's reload rather than be read as the entry's prefetch
// (`features/shell/DeploymentResilience.tsx`). The empty frame makes the wait invisible, not
// harmless: a margin that never arrives is a page missing its comments, its threads and its
// composer.
const Margin = lazy(() =>
  renderChunk(() => import("./Margin").then((module) => ({ default: module.Margin })))
);

/**
 * The margin's desktop column, empty. It takes the width `Margin.tsx` gives its column, on the
 * routes and viewports where that column takes any: a phone's margin is a sheet over the page,
 * and a collapsed margin is a fixed rail whose gutter the shell reserves itself (`app.tsx`).
 * Without it, a document drawn before the margin arrives would be laid out again, narrower, when
 * the margin does.
 */
function MarginFrame({ collapsed = false, width }: MarginProps): ReactNode {
  const isCompactViewport = useMediaQuery(COMPACT_VIEWPORT_QUERY);
  const { pathname, search } = useLocation();
  if (isCompactViewport || collapsed || !routeHasMargin(pathname, search)) {
    return null;
  }
  return (
    <div
      aria-hidden="true"
      className="relative order-3 shrink-0"
      style={{ width: `${clampMarginWidth(width ?? DEFAULT_MARGIN_WIDTH)}px` }}
    />
  );
}

export function MarginColumn(props: MarginProps): ReactNode {
  const drawn = useContentDrawn();
  if (!drawn) {
    return <MarginFrame {...props} />;
  }
  return (
    <Suspense fallback={<MarginFrame {...props} />}>
      <Margin {...props} />
    </Suspense>
  );
}
