import { useLayoutEffect } from "react";

/**
 * Marks on the page's performance timeline (`performance.mark(name)`) the commit in which a part
 * of the Evals tab draws once it is `drawn`: from its layout effect, which runs after every
 * child's own and before the browser paints, so the mark is the moment that drawing committed.
 * A browser measurement of the tab reads it (packages/envoy's evalbench/tab/paint.ts observes
 * every `dispatch:` mark); nothing else does. The mark goes once the part stops drawing or
 * unmounts, so the timeline holds at most one mark of each part drawn and none of a part gone.
 */
export function useDrawnMark(name: string, drawn: boolean): void {
  useLayoutEffect(() => {
    if (!drawn) return;
    performance.mark(name);
    return () => performance.clearMarks(name);
  }, [name, drawn]);
}
