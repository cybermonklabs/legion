/**
 * How long the Inbox's reading column keeps the document it opened at the section it was opened
 * for: until its drawing has been quiet for `quietMs` with no diagram still waiting, and never
 * past `limitMs` after the first drawing pass.
 *
 * One scroll is never enough: the editor draws its blocks in pieces, a diagram or a picture
 * draws later still, and every block landing above the heading pushes it down again, so the
 * placement is applied on every drawing pass rather than once. Both clocks start at the first of
 * those passes rather than at this call, because a column whose document has not drawn yet would
 * otherwise spend its bound waiting for the first block. The quiet is the document's own
 * re-run's (`QUIET_MS`, `features/doc/refocus-when-settled.ts`), so the two cannot drift apart;
 * the bound is the column's own, declared beside it and measured against the longest draw this
 * pane serves (`COLUMN_LIMIT_MS`).
 *
 * The reader's press, scroll, touch or key is read on the column and not on the window: the list
 * beside it moves the selection with `j` and `k`, and a window-wide reading would end the next
 * item's placement before its document had drawn. The scroll the placement itself writes is
 * nobody's press: it goes through the one call that records where the page's own place-keeping
 * left a box (`moveTheBox`, `features/doc/readers-place.ts`), so the document's own landing
 * correction, which watches the same column, reads it as the page holding the reader still
 * rather than as the reader taking the column over.
 */
import { COLUMN_LIMIT_MS, QUIET_MS } from "../doc/refocus-when-settled";
import { mermaidDrawsWaiting } from "../refs/mermaid-diagrams";

/** What a reader does to move on from where the column put them. */
const READER_ACTIONS = ["pointerdown", "wheel", "touchstart", "keydown"] as const;

export interface SectionHold {
  /** Told by the pane of each pass where a block of the opened document stands in the column.
   *  The first call starts both clocks; every call restarts the quiet one. */
  readonly drawing: () => void;
  /** Lets go and stops listening: the column is the reader's from here on. Called when the
   *  column closes, and by a placement of the reader's own - the head's **Skip to the answer** -
   *  which is not in the column and so is not seen as a press in it. */
  readonly release: () => void;
  /** Whether the column may still be written. */
  readonly writes: () => boolean;
}

/**
 * Holds `column` at its section until its drawing settles, the limit passes, the reader presses,
 * scrolls, touches or types in it, or the hold is released.
 *
 * `bounds` is there for a row that proves the limit without waiting the column's bound out, as
 * `resetMermaidDiagrams(limitMs)` is for the worker's own.
 */
export function holdSection(
  column: HTMLElement,
  bounds?: { limitMs?: number; quietMs?: number }
): SectionHold {
  const limitMs = bounds?.limitMs ?? COLUMN_LIMIT_MS;
  const quietMs = bounds?.quietMs ?? QUIET_MS;
  let writes = true;
  let quiet: number | undefined;
  let limit: number | undefined;
  const release = () => {
    if (!writes) {
      return;
    }
    writes = false;
    window.clearTimeout(quiet);
    window.clearTimeout(limit);
    for (const type of READER_ACTIONS) {
      column.removeEventListener(type, release, true);
    }
  };
  // The drawing has been quiet for `quietMs`. A diagram still waiting will move the text above
  // the reader and raise the next drawing pass itself, so the quiet is waited out again rather
  // than taken as settled.
  const settled = () => {
    if (!writes) {
      return;
    }
    if (mermaidDrawsWaiting()) {
      quiet = window.setTimeout(settled, quietMs);
      return;
    }
    release();
  };
  const drawing = () => {
    if (!writes) {
      return;
    }
    window.clearTimeout(quiet);
    quiet = window.setTimeout(settled, quietMs);
    limit ??= window.setTimeout(release, limitMs);
  };
  for (const type of READER_ACTIONS) {
    column.addEventListener(type, release, { capture: true, passive: true });
  }
  return { drawing, release, writes: () => writes };
}
