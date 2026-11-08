import { mermaidDrawsSettled, mermaidDrawsWaiting } from "../refs/mermaid-diagrams";
import { thePageLeftItThere, whatScrolls } from "./readers-place";

/** What a reader does to move on from where a link put them: press, scroll, touch or type. */
const READER_ACTIONS = ["pointerdown", "wheel", "touchstart", "keydown"] as const;

/** How long a correction is made at all. A drawing that never settles - a card that re-measures
 *  itself forever - must not leave the listeners armed for the life of the page. It is the length
 *  of a large document's build rather than a second or two, because such a document goes quiet
 *  between its diagrams and the next one moves the target just as far; a reader who moves on in
 *  the meantime stops the watch themselves. */
const LIMIT_MS = 30_000;

/** How long the movement a correction started is still that correction's own. A scroll this
 *  watch makes is instant (`FOCUS_SCROLL`, `@legion/proof-editor`, and no `scroll-behavior:
 *  smooth` in any of the page's styles), so the box stands where the correction left it and
 *  the one scroll event it fires reports that very position. A browser told to animate it
 *  instead reports a position every frame on the way, none of which any correction set, and
 *  reading the first of those as the reader's would end the watch before it had corrected
 *  anything - which is the whole of what it is for. So while that movement goes on, each of
 *  its events restarting this, where it has reached is taken as the correction's, and only a
 *  scroll arriving this long after the last of them is somebody else's. It is many frames, so
 *  a frame dropped on a busy thread does not split one animation into two, and far under the
 *  gap at which a scroll is a separate act: the row this rule was written for scrolls 1.8 s
 *  after the landing's correction (`e2e/deep-links.e2e.ts`, "a composer opened from the
 *  document is in the margin's view after a comment link"). */
const CORRECTION_SCROLL_MS = 250;

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
 * It runs only while nobody but the page itself has moved the page since this was called, so a
 * reader who moved on is never pulled back. Four things end it, and the first three are all
 * "the reader has the page now": a press, a wheel, a touch or a key; a scroll of the box
 * carrying the document that neither this watch's own corrections nor the page's own
 * place-keeping left there, which is how a scrollbar drag, a find-in-page jump, an assistive
 * tool and a scroll made over the automation protocol end it though they fire none of those
 * four events; and a selection change inside the document, a reader working in the page rather
 * than waiting for a link to land. The fourth is the limit. What is not the reader is the page
 * holding the reader still while it draws - `keepTheReadersPlace` and `keepWhatStandsUnder`,
 * which scroll through `moveTheScroll` and are known by what it records there
 * (`thePageLeftItThere`, `readers-place.ts`) - and a growth they answer moves this watch's
 * target too, so it goes on watching from where they left the page.
 *
 * Returns the dispose, which removes the listeners and drops any correction still to come;
 * they also go once the limit has passed.
 */
export function refocusWhenSettled(refocus: () => void, drawn?: Element): () => void {
  const diagrams = mermaidDrawsWaiting();
  if (!diagrams && drawn === undefined) return () => {};
  let done = false;
  let frame: number | undefined;
  let limit: number | undefined;
  let sizes: ResizeObserver | undefined;
  // The drawn document, which is also what says whose scroll a scroll is and whose selection a
  // selection is: the box carrying it is what every correction moves, and the selection that
  // ends this is one inside it. A caller with no drawn root (`scrollBlockToTop`, a reading
  // column whose own box keeps its size) names neither, and keeps the reader's own actions and
  // the limit as its only stops.
  const root = drawn instanceof HTMLElement ? drawn : undefined;
  /** The box carrying the document - a column in a reading pane, the page itself (`null`)
   *  where no column carries it - and how far down it the reader is, read as the reader's own
   *  keeper reads it (`scrollOffset`, `readers-place.ts`). A scroll of any other box, such as
   *  the margin landing on its own card, leaves that number where it was and is none of this
   *  watch's business. */
  const carryingBox = (): HTMLElement | null => (root === undefined ? null : whatScrolls(root));
  const offsetOf = (box: HTMLElement | null): number =>
    box === null ? window.scrollY : box.scrollTop;
  /** Where a scroll this watch owns left that box, so a scroll it does not own is told from
   *  one it does, and the timer that stands while the movement one started is still arriving. */
  let left = offsetOf(carryingBox());
  let correcting: number | undefined;
  const stop = () => {
    if (done) return;
    done = true;
    if (frame !== undefined) window.cancelAnimationFrame(frame);
    window.clearTimeout(limit);
    window.clearTimeout(correcting);
    sizes?.disconnect();
    for (const type of READER_ACTIONS) window.removeEventListener(type, stop, true);
    window.removeEventListener("scroll", scrolled, true);
    document.removeEventListener("selectionchange", selected);
  };
  /** A scroll this watch owns has just been made - its own correction, or the page's own
   *  place-keeping - and `at` is where that left the box, where it is already read. Where it
   *  leaves the box is where the box is meant to stand, and every position it reports on the
   *  way there is the same scroll's (`CORRECTION_SCROLL_MS`). Nothing to record with no box. */
  const corrected = (at?: number) => {
    if (root === undefined) return;
    left = at ?? offsetOf(carryingBox());
    window.clearTimeout(correcting);
    correcting = window.setTimeout(() => {
      correcting = undefined;
      left = offsetOf(carryingBox());
    }, CORRECTION_SCROLL_MS);
  };
  const scrolled = () => {
    const box = carryingBox();
    const at = offsetOf(box);
    // The box stands where this watch's last scroll left it: this is that scroll's own event,
    // or some other box's, which moved the document by nothing.
    if (at === left) return;
    // The page's own place-keeping put it there - the document's keeper, or the chrome's
    // (`moveTheScroll` and `thePageLeftItThere`, `readers-place.ts`), both of which move the
    // page to hold what the reader is looking at where it is. That is the page moving what it
    // draws, not the reader moving themselves, and the growth those keepers answer is one that
    // moves this watch's target as well, so it goes on watching from where they left it.
    if (thePageLeftItThere(box)) {
      corrected(at);
      return;
    }
    // The movement this watch's own scroll started is still arriving (above), or a correction
    // is already due this frame and the page is answering the very growth that correction is
    // for - the browser's own scroll anchoring, which moves the page for that growth wherever
    // it finds an anchor above it. Neither is somebody taking the page over, and reading
    // either as that would leave the growth uncorrected, which is the whole of what this
    // module is.
    if (correcting !== undefined || frame !== undefined) {
      left = at;
      return;
    }
    stop();
  };
  const selected = () => {
    if (root === undefined) return;
    const selection = document.getSelection();
    if (selection === null) return;
    const { anchorNode, focusNode } = selection;
    if (
      (anchorNode !== null && root.contains(anchorNode)) ||
      (focusNode !== null && root.contains(focusNode))
    ) {
      stop();
    }
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
      corrected();
    });
  };
  for (const type of READER_ACTIONS) {
    window.addEventListener(type, stop, { capture: true, passive: true });
  }
  if (root !== undefined) {
    // `capture`, because a column's scroll does not bubble; `passive`, because this reads the
    // offset and moves nothing. `selectionchange` on the document, because it is every change
    // of what is selected whoever made it - a drag, a shift-arrow after the focus, a
    // find-in-page match, an assistive tool, a range set over the automation protocol - where
    // `selectstart` is only the ones a pointer or a key begins and none of the rest.
    window.addEventListener("scroll", scrolled, { capture: true, passive: true });
    document.addEventListener("selectionchange", selected);
  }
  // The caller focused its target in the turn that called this (`focusInEditor`,
  // `scrollBlockToTop`), so that scroll is this watch's own as much as a correction is: its
  // event arrives after the listener above is armed.
  corrected();
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
