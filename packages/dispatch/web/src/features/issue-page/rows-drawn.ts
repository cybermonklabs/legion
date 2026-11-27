import { createContext, useCallback, useContext, useSyncExternalStore } from "react";

/**
 * Whether the issue page has drawn a row of the issue yet: the page's first drawing, which the
 * reading of the pages the outline read has not loaded waits on (`tree-read/tree-read.ts`'
 * `useReadUnloadedPages`, decision 538: a load reads no page past its first before the page has
 * drawn).
 *
 * At the base the render that received the tree read's answer drew its rows, so a frame after
 * that render the page had drawn them. On this page the tree is built in a task of its own and
 * handed to React in a later one (`useIssueTree.ts`), and the rows are drawn once every read that
 * places them has answered (`IssueTree`'s `placesSettled`), the first screen first
 * (`drawn-in-slices.ts`): a frame after the answer the page may hold no row yet. So each view's
 * rows tell the page, in the layout effect of the commit that first draws one of them
 * (`useDrawnInSlices`), and the reading starts a frame after that commit.
 *
 * The page's owner of the tree holds one for each issue the page draws
 * (`IssueTreeOwnerProvider`). Where none stands above - a slot drawn alone, the header drawn
 * alone - the page reads as drawn, and the reading waits a frame after the answer, as at the base.
 */
export interface RowsDrawn {
  /** Whether a view of the page has drawn a row of the issue. */
  drawn: boolean;
  /** Who to tell once it has. */
  readonly listeners: Set<() => void>;
}

/** A page that has drawn no row yet. */
export function newRowsDrawn(): RowsDrawn {
  return { drawn: false, listeners: new Set() };
}

/** The page's own, from its owner of the tree; none where no owner stands above. */
export const RowsDrawnContext = createContext<RowsDrawn | undefined>(undefined);

/** Tells `page` that a view drew a row of the issue, from the layout effect of the commit that drew
 *  it; nothing once it has been told, or where no page holds one. */
export function noteRowDrawn(page: RowsDrawn | undefined): void {
  if (page === undefined || page.drawn) return;
  page.drawn = true;
  for (const listener of page.listeners) listener();
}

/** Whether the page has drawn a row of the issue (`RowsDrawn`), and true where no owner of the
 *  tree stands above. */
export function useRowsDrawn(): boolean {
  const page = useContext(RowsDrawnContext);
  const subscribe = useCallback(
    (listener: () => void) => {
      if (page === undefined) return () => undefined;
      page.listeners.add(listener);
      return () => {
        page.listeners.delete(listener);
      };
    },
    [page]
  );
  return useSyncExternalStore(subscribe, () => page === undefined || page.drawn);
}
