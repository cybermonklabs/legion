/**
 * A reader's own acts, any of which ends the page's own placing of the focus, a caret or a
 * selection after a wait (`abortOnReaderAct`): a key, a press, a wheel and a touch. A reader who
 * acts in the wait has put the focus where they want it. The waits that end so:
 *
 * - the issue tree's opening, landing, zoom and hold of a row on screen (`IssueTree.tsx`), and the
 *   watch from an address's following that its landing and a line drawn in its place read
 *   (`slots/body.tsx`'s `ReaderActsWatch`, which the stage draws too);
 * - the caret in the row being edited, and the focus given back to the row once its edit ends
 *   (`useNodeEditing.ts`);
 * - the caret a mention field puts after a pick (`MentionField.tsx`'s `takePick`);
 * - the caret in the outline's line after a Reply, an Answer or an option (`outline-writer.tsx`'s
 *   `focus`);
 * - the control of an agent's row that Enter or `i` opens (`agents/keyboard.ts`'s `inOpenRow`);
 * - the row of Details an opener asks for (`details-rows.ts`'s `useOpenDetailsAt`);
 * - the control Add reaches once Details has opened or closed (`IssueAddMenu.tsx`'s
 *   `atControl().open`);
 * - the outline's tab stop a view's chord reaches (`useIssueKeymap.ts`'s `reachPart`);
 * - the row an address names in the Decisions view, once its reads answer
 *   (`DecisionsFilterBody.tsx`'s `revealActs`);
 * - the card that focus follows after a keyboard move on the board (`IssueBoard.tsx`'s
 *   `focusAfterMove`);
 * - the text box of the copy's fallback, which takes the selection and the focus
 *   (`lib/clipboard.ts`'s `copyText`).
 */
export const READER_ACTS = ["keydown", "pointerdown", "wheel", "touchstart"] as const;

/** A controller the reader's first act (`READER_ACTS`, read on the window in the capture phase)
 *  aborts, calling `then` after it. Aborted any other way, it calls nothing. */
export function abortOnReaderAct(then?: () => void): AbortController {
  const acts = new AbortController();
  for (const type of READER_ACTS) {
    window.addEventListener(
      type,
      () => {
        acts.abort();
        then?.();
      },
      { capture: true, signal: acts.signal }
    );
  }
  return acts;
}

/** True when the reader has not acted since `abortOnReaderAct` made `acted`. It stops the watch
 *  either way. */
export function claimUnacted(acted: AbortController): boolean {
  const unacted = !acted.signal.aborted;
  acted.abort();
  return unacted;
}
