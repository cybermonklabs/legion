import { mermaidDrawsSettled, mermaidDrawsWaiting } from "../refs/mermaid-diagrams";
import {
  scrollportTop,
  thePageLeftIt,
  thePageLeftItThere,
  whatScrolls,
  whereTheReaderIs,
} from "./readers-place";
import { followThePress, movesTheReader, THE_READERS_EVENTS } from "./readers-scroll";

/** How long a correction is made at all. A drawing that never settles - a card that re-measures
 *  itself forever - must not leave the listeners armed for the life of the page. It is the length
 *  of a large document's build rather than a second or two, because such a document goes quiet
 *  between its diagrams and the next one moves the target just as far; a reader who moves on in
 *  the meantime stops the watch themselves. */
export const LIMIT_MS = 30_000;

/** How long a drawing must hold still to be read as settled: long enough that a run of cards
 *  landing one after another is waited out once rather than chased, short enough that a reader
 *  who followed a link sees the target without reading somewhere else first. This watch waits
 *  out no quiet of its own - it corrects in the frame after each movement, which is what a
 *  large document's diagrams need - so the one correction reading it is the Inbox column's own
 *  hold on the document it opened (`holdSection`, `features/inbox/pane-placement.ts`). That
 *  hold reads this rather than a number of its own, so the two cannot drift apart. */
export const QUIET_MS = 200;

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

/** How far the document may have moved on the reader's screen and still be standing where this
 *  watch last read it: under one CSS pixel, which is no movement a reader can make and none
 *  they could see. A browser compensating a growth above the document scrolls by the layout's
 *  own figure, so the document lands back on the very line it was on and the difference is
 *  zero; the tolerance is for a page whose scroll offset the browser keeps in fractions of a
 *  pixel, where that figure and the offset it is written to round apart. */
const STILL_PX = 1;

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
 * (`e2e/large-documents-reader.e2e.ts`, "a link to the decision block at the end of the larger
 * document lands on that block"). Every move inside one frame is one correction, and the
 * correction is made in a frame of its own rather than inside the observer, so a reader who
 * moves on in the same turn as the growth is never pulled back. The focus itself does not
 * animate (`FOCUS_SCROLL`, `@legion/proof-editor`): over a document of 400,000 characters an
 * animated scroll was cut short and restarted by every correction, and the link never landed.
 *
 * With nothing that can move the target - no diagram waiting, and a `drawn` whose height never
 * changes - it does not run again and the target is pulsed once. A caller with no drawn root to
 * name passes none and waits on the diagrams alone. A reading column's scroll container keeps its
 * own size whatever its content does, so it names the document drawn inside it
 * (`scrollBlockToTop`).
 *
 * It runs only while nobody but the page itself has moved the page since this was called, so a
 * reader who moved on is never pulled back. Five things end it, and the first four are all "the
 * reader has the page now": an input that moves them, by the table of the reader's inputs the
 * page's keepers read too (`THE_READERS_INPUTS`, `readers-scroll.ts`) - a wheel, a finger that
 * moves, a key whose own action scrolls or moves the focus, a shortcut of the page's that moves
 * them, such as one that focuses a header control or switches a tab; a scroll of the box
 * carrying the document that neither this watch's own corrections nor the page's own
 * place-keeping left there, which is how a scrollbar drag, a find-in-page jump, an assistive
 * tool and a scroll made over the automation protocol end it though they are none of those
 * inputs; a selection change inside the document, a reader working in the page rather than
 * waiting for a link to land; and the focus moving anywhere the document is not once the reader
 * has pressed, touched or typed - a click on a control outside it, a tap into a field outside
 * it, a dialog their press or key opened.
 * A focus the page moves by itself, with no input of the reader's since this was called, is the
 * page and ends nothing: a comment link opens its card in the margin, whose reply field takes the
 * focus (`MentionComposer`), and React commits that before or after the landing's own focus as
 * it schedules the two. Read as the reader, it ended the landing 22 to 83 ms after it began - in
 * 3 runs of 20 of a comment link in Firefox, and 11 of 20 of a past version's in WebKit - before
 * the document's web font grew each paragraph above the quote from 24 to 25.6 px; nothing put
 * the reader back, and in Firefox the quote was left 21.6 px from where the link put it in each
 * of the 3 (`e2e/deep-links.e2e.ts`, "a composer opened from the document is in the margin's
 * view after a comment link").
 * The fifth is the limit. An input that moves nothing is not among them, and nor is a press
 * that has moved neither the place nor the focus - a click on empty space, a tap that lands on
 * nothing: this watch is also what puts a reader back on their own place once the editor is
 * swapped in over the draw (`ProofDocument`), and one that ended on any key left a reader who
 * had pressed Shift during the build 924.6 px from where they were, in 5 runs of 8
 * (`e2e/large-documents-build.e2e.ts`). A shortcut that moved them and did not end it is the
 * other way wrong: the page's own `s` focuses the issue's Status, which scrolls the page to it,
 * and that scroll, made while a correction was still due, was let through as the correction's
 * own, so the next growth put the reader back on the target with the control they asked for off
 * the screen.
 *
 * What is not the reader is the page holding the reader still while it draws, in either of the
 * two ways it does that. The page's own place-keeping - `keepTheReadersPlace` and
 * `keepWhatStandsUnder`, which scroll through `moveTheScroll` and are known by what it records
 * there (`thePageLeftItThere`, `readers-place.ts`) - and the browser's own scroll anchoring,
 * which answers a growth above the document by scrolling the box by exactly what grew and so
 * leaves the document standing on the line of the box it was already on, moving nothing on the
 * reader's screen. A growth either of them answers moves this watch's target too, so it goes on
 * watching from where they left the page; a reader who moves the page moves what it draws with
 * them, so neither reading ever excuses one of their moves.
 *
 * A press held down changes none of that. A scroll of the box is read by where it left the box
 * before the table is asked, so this watch's own correction, a keeper's write and the browser's
 * scroll anchoring stay what they are while a mouse button or a finger rests on the page, and
 * only a scroll none of them made is the press's own movement - a drag on the scroll bar, which
 * ends this at once. Each correction's own scroll is recorded as the page's own, where the
 * keepers read it too (`thePageLeftIt`): the editor's focus scrolls the box through no call of
 * `readers-place.ts`, and read as the press's movement, a press held across a correction ended
 * this at that correction and opened the keepers' window on it, which took whatever grew
 * meanwhile as the reader's place.
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
  // ends this is one inside it. A caller with no drawn root names neither, and keeps the
  // reader's own actions and the limit as its only stops.
  const root = drawn instanceof HTMLElement ? drawn : undefined;
  /** The events this watch reads by the table of the reader's inputs. A watch with a drawn root
   *  reads a scroll for itself (`scrolled`, below): where its own correction, a keeper's write
   *  or the browser's scroll anchoring left the box is no press's movement, and the table is
   *  asked of a scroll only once none of those is what it is. One with no root has no box of
   *  its own to read, and reads a scroll as the table does: a scroll while a press is held is
   *  the press's first movement. */
  const heard =
    root === undefined
      ? THE_READERS_EVENTS
      : THE_READERS_EVENTS.filter((type) => type !== "scroll");
  /** The box carrying the document - a column in a reading pane, the page itself (`null`)
   *  where no column carries it. How far down it the reader is, and where its top stands, are
   *  the readers' own readings of a reader's place (`whereTheReaderIs` and `scrollportTop`,
   *  `readers-place.ts`): a scroll of any other box, such as the margin landing on its own
   *  card, leaves that number where it was and is none of this watch's business. */
  const carryingBox = (): HTMLElement | null => (root === undefined ? null : whatScrolls(root));
  /** Which line of the box the drawn document stands on: the top of the document in the box's
   *  own coordinates, which are the viewport's where the page is the box. A reader scrolling
   *  moves it. A box scrolled by exactly what grew above the document inside it does not,
   *  which is what the browser's own scroll anchoring does, and is what tells the two apart.
   *  Zero with no drawn root, which is a caller that watches no scroll at all. */
  const whereTheDocumentStands = (): number => {
    if (root === undefined) return 0;
    return root.getBoundingClientRect().top - scrollportTop(carryingBox());
  };
  /** Where a scroll this watch owns left that box and the line the document stood on when it
   *  did, so a scroll it does not own is told from one it does, and the timer that stands
   *  while the movement one started is still arriving. */
  let left = whereTheReaderIs(carryingBox());
  let stood = whereTheDocumentStands();
  let correcting: number | undefined;
  /** Reads the box as standing where this watch's own doing left it: `at` is the offset it
   *  stands at, and the line the document stands on now is the line the next scroll is read
   *  against. */
  const remember = (at: number) => {
    left = at;
    stood = whereTheDocumentStands();
  };
  const stop = () => {
    if (done) return;
    done = true;
    if (frame !== undefined) window.cancelAnimationFrame(frame);
    window.clearTimeout(limit);
    window.clearTimeout(correcting);
    sizes?.disconnect();
    for (const type of heard) window.removeEventListener(type, acted, true);
    window.removeEventListener("focusin", focused, true);
    window.removeEventListener("scroll", scrolled, true);
    document.removeEventListener("selectionchange", selected);
    releaseThePress();
  };
  /** Whether the reader has pressed, touched, turned the wheel or pressed a key since this was
   *  called, which a focus that leaves the document is read by (`focused`). A scroll is no
   *  input of theirs on its own, and nor is a `keydown` no keyboard made. */
  let theReaderActed = false;
  /** The reader did something: where it moves them, by the table of their inputs, this ends. */
  const acted = (event: Event) => {
    if (event.type !== "scroll" && (event.type !== "keydown" || event instanceof KeyboardEvent)) {
      theReaderActed = true;
    }
    if (movesTheReader(event)) stop();
  };
  /** The focus moved: into the document is the caret, which `selected` reads; anywhere else,
   *  after an input of the reader's, is the reader taking it there - a press on a control, a
   *  tap into a field, a dialog their key opened - and ends this. With no input of theirs since
   *  this was called it is the page's own focus, such as the margin's reply field taking it as a
   *  comment link opens its card, and moves nobody. A caller with no drawn root has no document
   *  of its own, and any focus the reader moves is that. */
  const focused = (event: Event) => {
    if (root !== undefined && event.target instanceof Node && root.contains(event.target)) return;
    if (!theReaderActed) return;
    stop();
  };
  /** A scroll this watch owns has just been made - its own correction, or the page's own
   *  place-keeping - and `at` is where that left the box, where it is already read. Where it
   *  leaves the box is where the box is meant to stand, and every position it reports on the
   *  way there is the same scroll's (`CORRECTION_SCROLL_MS`). Nothing to record with no box. */
  const corrected = (at?: number) => {
    if (root === undefined) return;
    remember(at ?? whereTheReaderIs(carryingBox()));
    window.clearTimeout(correcting);
    correcting = window.setTimeout(() => {
      correcting = undefined;
      remember(whereTheReaderIs(carryingBox()));
    }, CORRECTION_SCROLL_MS);
  };
  const scrolled = (event: Event) => {
    const box = carryingBox();
    const at = whereTheReaderIs(box);
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
    // The page re-laid itself under the reader: what stands above the document grew, and the
    // browser's own scroll anchoring moved the box by exactly that growth, so the document is
    // on the line of the box it was already on and nothing moved on the reader's screen. That
    // is the page moving what it draws, as much as a keeper's correction is, and the growth it
    // answers is one that moves this watch's target too - the version view's own heading and
    // chrome land above the document while the diagrams below are still drawing
    // (`e2e/mermaid.e2e.ts`, "a past version's highlighted mark lands on its block once a
    // diagram above it draws"). A reader who moves the page moves what it draws with it, so no
    // move of theirs is excused here.
    if (Math.abs(whereTheDocumentStands() - stood) < STILL_PX) {
      remember(at);
      return;
    }
    // A scroll none of those made, while a press is held: the press's first movement, a drag on
    // the box's scroll bar, by the table of the reader's inputs (`THE_READERS_INPUTS`). It ends
    // this even inside the windows below, which read the positions of a movement nobody's
    // hand is on: a drag read as a correction's own would be pulled back to the target at the
    // next growth, and every correction it fought would open the window again.
    if (movesTheReader(event)) {
      stop();
      return;
    }
    // The movement this watch's own scroll started is still arriving (above), or a correction
    // is already due this frame and the page is answering the very growth that correction is
    // for - the browser's own scroll anchoring, which moves the page for that growth wherever
    // it finds an anchor above it. Neither is somebody taking the page over, and reading
    // either as that would leave the growth uncorrected, which is the whole of what this
    // module is.
    if (correcting !== undefined || frame !== undefined) {
      remember(at);
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
      const box = carryingBox();
      const from = whereTheReaderIs(box);
      refocus();
      const at = whereTheReaderIs(box);
      // The editor's own focus scroll (`focusBlock`, `focusMark`) moves the box through no call
      // of `readers-place.ts`, so where it left the box is recorded there as the page's own:
      // the table of the reader's inputs reads the scroll by that record, and read as a held
      // press's movement it opened the keepers' window on this very correction.
      if (root !== undefined && at !== from) thePageLeftIt(box);
      corrected(at);
    });
  };
  // `capture`, so an input is read before the page answers it: the registry's own listener
  // takes a shortcut's key and moves its chord on, and the table reads the key as the registry
  // stands before that (`effectOf`, `features/shell/keymap.ts`).
  for (const type of heard) {
    window.addEventListener(type, acted, { capture: true, passive: true });
  }
  // `focusin`, which bubbles from wherever the focus went, on the capture phase like the rest.
  window.addEventListener("focusin", focused, { capture: true, passive: true });
  // Whether a press is held, which the table reads a scroll by: a scroll while one is held is
  // the press's first movement.
  const releaseThePress = followThePress();
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
