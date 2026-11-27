import { useSyncExternalStore } from "react";

/**
 * Where a phone's Review sheet takes in the document's own chrome (dispatch://INFRA-341, R11; the
 * architect's choice 1, dispatch://INFRA-341/comment/f326cd35-b653-4840-9b18-339af013e758).
 *
 * On a phone the tab strip wraps onto three lines (dispatch://INFRA-5200), and with the artifact
 * toolbar's two lines and the open-decisions bar above the document, the document's first block
 * stood under the collapsed sheet at 390 by 664 px. So below the phone width the sheet holds
 * them: the toolbar's controls (the Version picker, Correct title, Remove, Name version) and the
 * open-decisions bar in the sheet's own body, under its toggle, in the order the page draws them
 * at every other width; and, in the collapsed bar itself, the version the page shows and the
 * count of open decisions, so a reader is told both without opening the sheet.
 *
 * The sheet is drawn by the margin and the chrome by the document, in two trees, so the sheet
 * publishes its four boxes here while it is a phone's collapsed-or-open sheet, and each part
 * draws itself into its box through a portal (`ArtifactHeader`, `ProofDocument`), or in its own
 * place where there are none: from 768 px up, in the Inbox's reading column, on a page with no
 * margin, and on a project document's page, which has no issue header or tab strip above its
 * document and keeps its toolbar and bar (dispatch://INFRA-341/comment/6cf1eeaf-0ea5-4e81-a640-d28bed8fd376).
 */
export interface ReviewSheetSlots {
  /** The sheet's body under its toggle: the toolbar's controls. */
  readonly controls: HTMLElement;
  /** The collapsed bar: the count of open decisions. */
  readonly count: HTMLElement;
  /** The sheet's body under the toolbar's controls: the open-decisions bar. */
  readonly decisions: HTMLElement;
  /** The collapsed bar: the version the page shows. */
  readonly version: HTMLElement;
  /** Shuts the sheet and then runs `move`, for a control in it that takes the reader into the
   *  document: the open-decisions bar's link. `move` runs once the sheet is shut and has given the
   *  focus back, so the place and the focus it gives the reader are the ones they are left with,
   *  not a modal sheet's over them; at once where the sheet is shut already. */
  readonly leave: (move: () => void) => void;
}

let current: ReviewSheetSlots | null = null;
const listeners = new Set<() => void>();

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

/** The sheet's boxes while a phone's Review sheet stands on the page, else null. */
export function useReviewSheetSlots(): ReviewSheetSlots | null {
  return useSyncExternalStore(
    subscribe,
    () => current,
    () => null
  );
}

/** Publishes the sheet's boxes, and answers the call that withdraws them: it withdraws only the
 *  boxes it published, so a sheet that replaced them keeps its own. */
export function publishReviewSheetSlots(slots: ReviewSheetSlots): () => void {
  current = slots;
  for (const listener of listeners) listener();
  return () => {
    if (current !== slots) return;
    current = null;
    for (const listener of listeners) listener();
  };
}
