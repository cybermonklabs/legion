import { mermaidDrawsSettled, mermaidDrawsWaiting } from "../refs/mermaid-diagrams";

/** What a reader does to move on from where a link put them: press, scroll, touch or type. */
const READER_ACTIONS = ["pointerdown", "wheel", "touchstart", "keydown"] as const;

/** How long a correction is made at all. A drawing that never settles - a card that re-measures
 *  itself forever - must not leave the listeners armed for the life of the page. It is the length
 *  of a large document's build rather than a second or two, because such a document goes quiet
 *  between its diagrams and the next one moves the target just as far; a reader who moves on in
 *  the meantime stops the watch themselves. */
const LIMIT_MS = 30_000;

/**
 * Runs `refocus` (every focus in the live document, through `ProofDocument`'s `focusInEditor`,
 * and a past version's highlighted mark in `VersionView`) again each time the drawing around its
 * target moves, so the focus lands on its target rather than where the target stood when it
 * first ran.
 *
 * Two things draw after the editor has opened and scrolled to its target, and each one above the
 * target pushes it down: the page's mermaid diagrams, drawn in Dispatch's Mermaid worker
 * (`features/refs/mermaid-diagrams.ts`), and the document's own blocks - a decision block's
 * hosted card, a picture, a font, a block the browser skipped (`content-visibility: auto`,
 * `styles.css`) painting at its own height instead of its estimate - which grow `drawn`, the
 * editor's drawn root. Neither is settled when this is called: a block asks for its diagram only
 * once an `IntersectionObserver` reports it within one viewport of the reader, and the focus's
 * own scroll is what brings the blocks around the target that near.
 *
 * So each move puts the reader back on their target in the next animation frame, rather than
 * once the page has held still for a while: the diagram directly above the decision block at the
 * end of the larger document grew from 297 to 1,587 px, and the 300 ms a settle cost were 300 ms
 * with the target 1,290 px below where it belonged - long enough for a reader to see it
 * (`e2e/large-documents.e2e.ts`, "a link to the decision block at the end of the larger document
 * lands on that block"). Every move inside one frame is one correction, and the correction is
 * made in a frame of its own rather than inside the observer, so a reader who moves on in the
 * same turn as the growth is never pulled back. The focus itself does not animate
 * (`FOCUS_SCROLL`, `@legion/proof-editor`): over a document of 400,000 characters an animated
 * scroll was cut short and restarted by every correction, and the link never landed.
 *
 * With nothing that can move the target - no diagram waiting, and a `drawn` whose height never
 * changes - it does not run again and the target is pulsed once. A caller with no drawn root to
 * name (a reading column, whose scroll container keeps its own size whatever its content does)
 * passes none and waits on the diagrams alone.
 *
 * It runs only while the reader has not pressed, scrolled, touched or typed since this was
 * called, so a reader who moved on is never pulled back. Returns the dispose, which removes the
 * listeners and drops any correction still to come; they also go once the limit has passed.
 */
export function refocusWhenSettled(refocus: () => void, drawn?: Element): () => void {
  const diagrams = mermaidDrawsWaiting();
  if (!diagrams && drawn === undefined) return () => {};
  let done = false;
  let frame: number | undefined;
  let limit: number | undefined;
  let sizes: ResizeObserver | undefined;
  const stop = () => {
    if (done) return;
    done = true;
    if (frame !== undefined) window.cancelAnimationFrame(frame);
    window.clearTimeout(limit);
    sizes?.disconnect();
    for (const type of READER_ACTIONS) window.removeEventListener(type, stop, true);
  };
  // Something that can have moved the target has just happened: the next frame puts the reader
  // back on it, so the frame that first shows the grown page already shows the target where the
  // link left it.
  const moving = () => {
    if (done || frame !== undefined) return;
    frame = window.requestAnimationFrame(() => {
      frame = undefined;
      if (done) return;
      refocus();
    });
  };
  for (const type of READER_ACTIONS) {
    window.addEventListener(type, stop, { capture: true, passive: true });
  }
  limit = window.setTimeout(stop, LIMIT_MS);
  if (drawn !== undefined) {
    let height = drawn.getBoundingClientRect().height;
    sizes = new ResizeObserver(() => {
      const drawnHeight = drawn.getBoundingClientRect().height;
      if (drawnHeight === height) return;
      height = drawnHeight;
      moving();
    });
    sizes.observe(drawn);
  }
  if (diagrams) void mermaidDrawsSettled().then(moving);
  return stop;
}
