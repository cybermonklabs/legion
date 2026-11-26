/** A reader's own acts, any of which ends the page's own placing of the focus - the issue tree's
 *  (`IssueTree.tsx`), the caret's in the row being edited (`useNodeEditing.ts`), and the caret a
 *  mention field puts after a pick (`MentionField.tsx`): a key, a press, a wheel and a touch. */
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
