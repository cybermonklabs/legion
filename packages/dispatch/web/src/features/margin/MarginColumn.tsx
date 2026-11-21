import { type ReactNode, Suspense } from "react";
import { useLocation } from "react-router-dom";

import { routeHasMargin } from "../refs/routes";
import { lazyChunk } from "../shell/chunk-loads";
import { useContentDrawn } from "../shell/content-drawn";
import { COMPACT_VIEWPORT_QUERY, useMediaQuery } from "../shell/useDialog";

import type { MarginProps } from "./Margin";
import { MARGIN_COLUMN_CLASS, marginColumnWidth } from "./margin-width";

/**
 * Where the shell places the margin, and where the margin's own code is fetched: after the page
 * the reader landed on has drawn its content, not before it.
 *
 * The margin is mounted on every route, and draws nothing on most of them - it returns null on a
 * desktop route that has no margin (`Margin.tsx`) - but it carries the comment composer, the
 * thread cards and the Markdown surfaces under them, a tenth of what the entry chunk used to
 * make every first page load parse. Nothing of it is on any route's first frame, so its code
 * sits behind `import()`, as the editor's library does (`features/doc/document-chunks.ts`), and
 * the `import()` is beside the component it loads rather than in the shell, since the margin's
 * rule for what it draws is the margin's own. Unlike the editor's library, which the entry starts
 * beside the landing read of a page that draws a document, nothing asks for it before the page
 * has drawn.
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
// Through `lazyChunk`, whose `renderChunk` tells the chunk-failure recovery that a reader is
// waiting on this download: the margin is mounted by the route, not prefetched by the entry, so a
// failure here must spend the tab's reload rather than be read as the entry's prefetch
// (`features/shell/DeploymentResilience.tsx`). The empty frame makes the wait invisible, not
// harmless: a margin that never arrives is a page missing its comments, its threads and its
// composer.
const Margin = lazyChunk(() => import("./Margin").then((module) => ({ default: module.Margin })));

/** What the shell tells the margin beside its own props: `drawer` is whether the margin is the
 *  new issue page's Details drawer, which the shell's frame makes it on an issue's address for a
 *  reader who chose that page (`app.tsx`'s `issueFrame`; `frame-shell.tsx` gives the margin its
 *  drawer exactly where it gives the shell a frame). */
interface MarginColumnProps extends MarginProps {
  drawer: boolean;
}

/**
 * The margin's desktop column, empty: the class and the width `Margin.tsx` gives its column
 * (`margin-width.ts`), on the routes and viewports where that column takes any. A phone's margin
 * is a sheet over the page, a collapsed margin is a fixed rail whose gutter the shell reserves
 * itself (`app.tsx`), and the frame's Details is a drawer over the page, which takes no column
 * open or closed (`Margin.tsx`'s `drawer`), so none of the three holds one here.
 * Without it, a document drawn before the margin arrives would be laid out again, narrower, when
 * the margin does. With one where the margin takes none, the page would be drawn narrower until
 * the margin arrives and then wider.
 */
function MarginFrame({ collapsed = false, drawer, width }: MarginColumnProps): ReactNode {
  const isCompactViewport = useMediaQuery(COMPACT_VIEWPORT_QUERY);
  const { pathname, search } = useLocation();
  if (drawer || isCompactViewport || collapsed || !routeHasMargin(pathname, search)) {
    return null;
  }
  return (
    <div
      aria-hidden="true"
      className={MARGIN_COLUMN_CLASS}
      style={{ width: `${marginColumnWidth(width)}px` }}
    />
  );
}

export function MarginColumn({ drawer, ...props }: MarginColumnProps): ReactNode {
  const drawn = useContentDrawn();
  if (!drawn) {
    return <MarginFrame drawer={drawer} {...props} />;
  }
  return (
    <Suspense fallback={<MarginFrame drawer={drawer} {...props} />}>
      <Margin {...props} />
    </Suspense>
  );
}
