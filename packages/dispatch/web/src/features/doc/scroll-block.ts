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
 * want this scroll alone, without the re-run after the diagrams draw and without cancelling a
 * pending one.
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
 * when the row it shows changes. A **Skip to the answer** in the head leaves a re-run that reads
 * the reader on the window and lives as long as a document's build does (`LIMIT_MS`), and a row
 * changed by no action of the reader's - the server withdrawing the question, the selection
 * handed on - fires none of the four events that watch reads, so it would otherwise be left to
 * write the next item's column. */
export function cancelBlockScroll(): void {
  disposeRerun?.();
  disposeRerun = undefined;
}

/**
 * Scrolls `container` so the block with `blockId` sits at its top, and answers whether the
 * container held that block. A container that already shows it there is not moved.
 *
 * It runs again once the page's mermaid diagrams have drawn (`refocusWhenSettled`, the module
 * the live document's focus re-runs through): a document's diagrams draw after the editor opened,
 * in Dispatch's Mermaid worker, and each one above the block pushes it down by the diagram's
 * height. The re-run reads the block afresh, so a node view redrawn in between is found again,
 * and it is dropped if the block has gone or the reader has pressed, scrolled, touched or typed.
 * This column names no drawn root of its own: its scroll container keeps its size whatever its
 * content does, so the diagrams are what it waits on.
 *
 * This is not `focusBlock` on the editor handle: that centres the block and pulses it, which is
 * what the document page wants of a link. A reading pane beside a list wants the block the reader
 * chose at the top of the column, with the document under it to read on into.
 */
export function scrollBlockToTop(blockId: string, container: HTMLElement): boolean {
  cancelBlockScroll();
  const selector = `[${BLOCK_ID_ATTRIBUTE}="${CSS.escape(blockId)}"]`;
  if (container.querySelector(selector) === null) {
    return false;
  }
  const scroll = () => {
    const block = container.querySelector(selector);
    if (block === null) {
      return;
    }
    scrollElementToTop(block, container);
  };
  scroll();
  disposeRerun = refocusWhenSettled(scroll);
  return true;
}
