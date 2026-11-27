/**
 * A reader's own acts: a key, a press, a wheel and a touch. Any of them ends the page's own
 * placing of the focus, a caret or a selection after a wait (`abortOnReaderAct`): a reader who
 * acts in the wait has put the focus where they want it, and a placing that came after would
 * take it back from there.
 */
export const READER_ACTS = ["keydown", "pointerdown", "wheel", "touchstart"] as const;

/**
 * A controller the reader's first act (`READER_ACTS`, read on the window in the capture phase)
 * aborts, calling `then` after it. Aborted any other way, it calls nothing.
 *
 * Only an act that comes after the call aborts it: the press or the key that asked for the wait
 * has already passed the window's capture phase when the watch is made, so it does not abort it.
 */
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
