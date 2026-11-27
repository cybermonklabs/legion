import { BLOCK_ID_ATTRIBUTE } from "./block-id";
import { moveTheBox } from "./readers-place";
import { refocusWhenSettled } from "./refocus-when-settled";

/**
 * Scrolls `container` so `element` sits at its top. A container already showing it there is not
 * moved, the two rect tops being equal.
 *
 * The move goes through `moveTheBox` (`readers-place.ts`), which records where it left the
 * container: this is the page placing a document it is drawing, so a landing correction
 * watching the same box reads the scroll as the page's own place-keeping rather than as the
 * reader taking the box over (`refocus-when-settled.ts`), and the correction goes on.
 *
 * This is the whole of the placement `scrollBlockToTop` performs, and it is what the Inbox's
 * reading column scrolls with: its placement writes the column with this on every drawing pass
 * the hold still allows (`features/inbox/pane-placement.ts`), and `Next change` performs it for
 * a block found by its mark rather than by its id. Neither of them is `scrollBlockToTop`, whose
 * one caller is the column's **Skip to the answer**, a placement the reader asked for: they
 * want this scroll alone, without that function's re-run and without cancelling a pending one.
 */
export function scrollElementToTop(element: Element, container: HTMLElement): void {
  moveTheBox(
    container,
    element.getBoundingClientRect().top - container.getBoundingClientRect().top
  );
}

/** The dispose of the latest scroll's re-run after the diagrams draw. One waits at a time, as the
 * live document's focus does (`focusInEditor` in `ProofDocument.tsx`): a newer scroll drops an
 * older one's re-run, so a reader is never pulled back to the block they have left. */
let disposeRerun: (() => void) | undefined;

/** Drops the pending re-run, if one waits. It is the dispose `refocusWhenSettled` returned, held
 * here so the one surface that knows its row has changed can call it: the Inbox's reading pane,
 * when the row it shows changes. A **Skip to the answer** in the head leaves a re-run that
 * watches the column's drawn document and lives as long as a document's build does
 * (`LIMIT_MS`), and a row changed by no action of the reader's - the server withdrawing the
 * question, the selection handed on - is no input of the reader's, so it may leave that watch
 * armed to write the next item's column. */
export function cancelBlockScroll(): void {
  disposeRerun?.();
  disposeRerun = undefined;
}

/**
 * Scrolls `container` so the block with `blockId` sits at its top, and answers whether the
 * container held that block. A container that already shows it there is not moved.
 *
 * It runs again each time the drawing around the block moves it (`refocusWhenSettled`, the
 * module the live document's focus re-runs through): once the page's mermaid diagrams have
 * drawn, in Dispatch's Mermaid worker, each one above the block having pushed it down by the
 * diagram's height; and each time `drawn`, the document drawn in the container, changes height,
 * which a block the browser skipped does as it paints at its own height in place of its 24 px
 * estimate (`content-visibility: auto`, `styles.css`). The skip itself brings the blocks between
 * the reader's old place and the block into view, so they paint at once and push it down: with
 * no drawn root to watch, a column with no diagram ran no re-run at all, and the reader who
 * pressed Skip to the answer in a phone's reading view was left a screen above the answer
 * (CI run 45994800355, `inbox-reading-pane.e2e.ts`, "on a phone the reading view answers a
 * decision block…", flaky). The re-run reads the block afresh, so a node view redrawn in between
 * is found again, and it is dropped if the block has gone, if the focus moves, or if the reader
 * has done something that moves them, by the table of the reader's inputs (`movesTheReader`,
 * `readers-scroll.ts`): a wheel, a finger that moves, a press that has begun to scroll, a key
 * that scrolls or moves the focus. A letter typed into a field, a modifier, a copy, or a press
 * or a finger that has gone down and not moved leaves it running; so does a scroll of the
 * container that only the page's own place-keeping or the browser's scroll anchoring made.
 *
 * This is not `focusBlock` on the editor handle: that centres the block and pulses it, which is
 * what the document page wants of a link. A reading pane beside a list wants the block the reader
 * chose at the top of the column, with the document under it to read on into.
 */
export function scrollBlockToTop(
  blockId: string,
  container: HTMLElement,
  drawn?: Element
): boolean {
  cancelBlockScroll();
  const selector = `[${BLOCK_ID_ATTRIBUTE}="${CSS.escape(blockId)}"]`;
  if (container.querySelector(selector) === null) {
    return false;
  }
  const placeTheBlock = () => {
    const block = container.querySelector(selector);
    if (block === null) {
      return;
    }
    scrollElementToTop(block, container);
  };
  placeTheBlock();
  disposeRerun = refocusWhenSettled(placeTheBlock, drawn);
  return true;
}
