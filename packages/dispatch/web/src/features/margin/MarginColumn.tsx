import { lazy, type ReactNode, Suspense } from "react";

import type { MarginProps } from "./Margin";
import { MARGIN_COLUMN_CLASS, useMarginLayout } from "./useMarginLayout";

/**
 * Where the shell places the margin, and where the margin's own code is fetched: after the first
 * paint, not before it.
 *
 * The margin is mounted on every route, and draws nothing on most of them - it returns null on a
 * desktop route that has no margin (`Margin.tsx`) - but it carries the comment composer, the
 * thread cards and the Markdown surfaces under them, a tenth of what the entry chunk used to
 * make every first page load parse. None of that is on any route's first frame, so the shell
 * loads it the way `features/doc/editor.ts` loads the editor: behind `import()`, beside the
 * component it loads rather than in the shell, since the margin's rule for what it draws is the
 * margin's own (`useMarginLayout.ts`, which this file reads without loading the margin).
 *
 * It stays mounted once it arrives, as it always was: the tab, the open thread and the composer
 * the reader left behind on an issue are still there when they come back to it
 * (`useMarginSheet.ts`). On the frames before it arrives the reader sees the page, and beside it
 * what `MarginColumnFallback` draws by the margin's own rule:
 *
 * - On a desktop route that has a margin, other than the new issue page, with the margin open,
 *   an empty column of the margin's own width in its place, so the page has its final width
 *   from the first paint rather than narrowing when the margin arrives, its tab list wrapping
 *   and its content moving down under the reader. The column fills in when the margin's code
 *   arrives.
 * - Everywhere else nothing, since there the margin takes no width from the page either: on a
 *   desktop route with no margin it draws nothing; collapsed, it is a fixed rail over
 *   the gutter the shell already keeps for it (`app.tsx`'s `marginRailShown`); on the new issue
 *   page it is Details, a drawer over the page; and below the compact layout it is a sheet fixed
 *   at the foot of the screen. The rail, Details and the sheet appear when the margin's code does.
 */
const Margin = lazy(() => import("./Margin").then((module) => ({ default: module.Margin })));

export function MarginColumn(props: MarginProps): ReactNode {
  return (
    <Suspense fallback={<MarginColumnFallback {...props} />}>
      <Margin {...props} />
    </Suspense>
  );
}

/** The desktop column's place while the margin's code loads: empty, and hidden from assistive
 *  technology, since there is nothing in it to read yet. Nothing where no column is drawn. */
export function MarginColumnFallback(props: MarginProps): ReactNode {
  const { columnShown, width } = useMarginLayout(props);
  return columnShown ? (
    <div aria-hidden="true" className={MARGIN_COLUMN_CLASS} style={{ width: `${width}px` }} />
  ) : null;
}
