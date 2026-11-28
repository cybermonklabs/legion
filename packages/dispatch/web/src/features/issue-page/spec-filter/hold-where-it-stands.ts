import type { RefObject } from "react";

import { abortOnReaderAct } from "../useNodeEditing";

/** A decision cell's words (`SpecFilterBody`'s `DecisionCell`), which stay at the top of the
 *  screen while the cell, as tall as its whole run of statements, runs past it. */
const CELL_WORDS = "[data-spec-cell-words]";

/**
 * Holds `item`, the statement or the decision cell of the Spec view's tree that a key or a press
 * left the focus in, where it stands on screen while the page grows: each time the page's size
 * changes - a read drawing rows above it, a document mounting beside the tree - the page is
 * scrolled by as far as what the reader sees of the item has moved, so it stands where it stood.
 * What the reader sees of a decision cell is its words (`CELL_WORDS`), and of a statement the
 * statement itself.
 *
 * The hold reads the item's top after the act's placing. A key's hold starts at keyup, after
 * the browser has scrolled any focused link into view. For a press that takes focus in a
 * later task, the caller reads the top again once that focus has been drawn and placed.
 * Other scrolls do not change the target. If a correction is clamped at the end of the page,
 * the next resize still corrects toward the original top.
 *
 * It does not bring the item on screen, as the outline's hold does (`IssueTree.tsx`'s
 * `holdOnScreen`): a key that moves the focus to an item places it by the page's rule as it moves
 * (`useOnScreenUnderHeads`), and the reader pressed what they could see. That rule puts an item
 * taller than the screen at its top, and a cell is as tall as its run, so a hold that brought the
 * item on screen would take the page to the top of a long run at a press on its cell's words or
 * at `w` to its cell, and to the top of a long statement at `f` to a link deep in it: what the
 * reader was reading would leave the screen.
 *
 * A reader's own act (`READER_ACTS`) ends the hold, and so does the focus having left the item.
 * It ends whatever `held` held before and takes its place there, so one item is held at a time,
 * as the outline holds one row (`holdInPlaceOf`).
 */
export function holdWhereItStands(
  item: HTMLElement,
  held: RefObject<AbortController | undefined>
): (() => void) | undefined {
  held.current?.abort();
  const hold = abortOnReaderAct();
  held.current = hold;
  if (typeof ResizeObserver === "undefined") return undefined;
  const seen = item.querySelector<HTMLElement>(CELL_WORDS) ?? item;
  let stood = seen.getBoundingClientRect().top;
  // While the hold places the item, the browser must not anchor the viewport to another row.
  const page = document.documentElement.style;
  const anchoring = page.overflowAnchor;
  page.overflowAnchor = "none";
  const grown = new ResizeObserver(() => {
    if (!item.contains(document.activeElement)) {
      hold.abort();
      return;
    }
    const moved = seen.getBoundingClientRect().top - stood;
    if (moved === 0) return;
    window.scrollBy({ top: moved });
  });
  grown.observe(document.body);
  hold.signal.addEventListener("abort", () => {
    grown.disconnect();
    page.overflowAnchor = anchoring;
  });
  return () => {
    if (!hold.signal.aborted && item.contains(document.activeElement)) {
      stood = seen.getBoundingClientRect().top;
    }
  };
}
