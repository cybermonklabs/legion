/**
 * Where a reader is in a document, and how the page keeps them there.
 *
 * Two surfaces need the same answer. The swap from the page's own draw to the editor reads the
 * reader's place before it and puts them back after it (`ProofDocument`'s `onBatch` and its
 * `handle.reveal()`), and the page keeps that place for as long as the editor is bound, because
 * what the page draws after the reader is in place still grows above them: a panel the page
 * held behind the editor's first batch (`first-batch.ts`), a decision card drawn into its
 * block's shell one task at a time (`useAskCardsDrawnInTasks`), a Mermaid diagram the worker
 * answers for, and a block the browser skipped painting at its own height in place of the one
 * `contain-intrinsic-size` estimated.
 *
 * A body whose Markdown formats after the commit that drew it is the same answer again, kept
 * for the paint rather than for a binding (`features/refs/useRenderedMarkdown.tsx`): it is one
 * keeper and one name, where a window-only copy of it under the same name held a reader in the
 * Inbox's own reading column by scrolling the page the column does not move
 * (dispatch://INFRA-341).
 *
 * A panel arriving must not shift what a reader is looking at. Where the panel's box is known
 * before its read answers, the panel reserves it and nothing moves; where its height is
 * whatever the answer turns out to be, this module is the answer, and
 * `packages/dispatch/AGENTS.md` ("Panels that arrive after the reader") says which is which.
 */

/** Where a reader is in a document drawn as a run of top-level blocks: the first block the top
 *  of the scrollport is in or above, and how far that block's own top sits from it. `undefined`
 *  where there is nothing to keep: no draw, or a reader still at the document's first block and
 *  above its top, whom any draw of it leaves where they are. */
export interface ReadersBlock {
  readonly index: number;
  readonly top: number;
}

/** The top of `scroller` - the box that scrolls a document, the page itself where `null` - in
 *  the page's own coordinates: the top edge of the Inbox's reading column, or the top of the
 *  page where the page itself scrolls the document. It is the first line of the document the
 *  reader can see, and every reading below is taken from it: read against the page in a
 *  column, the block a reader is on is one that scrolled out of the column above them, and a
 *  growth inside that block moves everything they are looking at while this module answers
 *  nothing. */
export function scrollportTop(scroller: HTMLElement | null): number {
  return scroller === null ? 0 : scroller.getBoundingClientRect().top;
}

/** How tall that scrollport is: the column's own height, or the window's where the page is the
 *  scrollport. */
export function scrollportHeight(scroller: HTMLElement | null): number {
  return scroller === null ? window.innerHeight : scroller.clientHeight;
}

/**
 * The first child of `drawn` the top of the scrollport is in or above, by a binary search over
 * the children rather than a scan of them: the run is in document order, so `bottom > edge` is
 * false for every block above the scrollport and true from the reader's own block on, and the
 * document this is asked about holds thousands of blocks. `undefined` where every block is
 * above the scrollport, which is a reader past the end of the document.
 */
function firstBlockOnScreen(drawn: HTMLElement, edge: number): number | undefined {
  const blocks = drawn.children;
  let low = 0;
  let high = blocks.length - 1;
  let found: number | undefined;
  while (low <= high) {
    const middle = (low + high) >> 1;
    if (blocks[middle].getBoundingClientRect().bottom > edge) {
      found = middle;
      high = middle - 1;
    } else {
      low = middle + 1;
    }
  }
  return found;
}

export function readersBlock(drawn: HTMLElement | null): ReadersBlock | undefined {
  if (drawn === null) {
    return undefined;
  }
  const edge = scrollportTop(whatScrolls(drawn));
  const index = firstBlockOnScreen(drawn, edge);
  if (index === undefined) {
    return undefined;
  }
  const top = drawn.children[index].getBoundingClientRect().top - edge;
  return index === 0 && top >= 0 ? undefined : { index, top };
}

/** What scrolls `element`: the nearest ancestor that scrolls its own content, or the page
 *  itself. A document is the page's own scroll on its page and a column's scroll in a reading
 *  pane, and the reader's place is kept against whichever is carrying them. */
export function whatScrolls(element: HTMLElement): HTMLElement | null {
  for (let box = element.parentElement; box !== null; box = box.parentElement) {
    const overflow = window.getComputedStyle(box).overflowY;
    if ((overflow === "auto" || overflow === "scroll") && box.scrollHeight > box.clientHeight) {
      return box;
    }
  }
  return null;
}

/**
 * How far down the document the reader is: the one reading of a reader's place, taken against
 * `scroller` - the box that scrolls what they are reading - with the page itself as its `null`
 * case rather than its default.
 *
 * Every surface asks it of the box `whatScrolls` found for what it draws. A surface that reads
 * `window.scrollY` instead is right only while the page is the box: in the Inbox's reading
 * column the window's offset is a number the reader never moves, so a place recorded from it
 * is the wrong place, a distance measured with it is the wrong distance, and a test of whether
 * the reader is at the top answers the page's top rather than the column's
 * (dispatch://INFRA-341).
 */
export function whereTheReaderIs(scroller: HTMLElement | null): number {
  return scroller === null ? window.scrollY : scroller.scrollTop;
}

/** How far down whatever carries `within` the reader is. */
function scrollOffset(within: HTMLElement): number {
  return whereTheReaderIs(whatScrolls(within));
}

/** Where the page's own place-keeping last left each box it scrolled, and where it last left
 *  the page itself. A keeper's correction is the page moving what it draws to hold the reader
 *  still, never the reader moving themselves, and the watch that reads a scroll as the reader
 *  taking the page over (`refocus-when-settled.ts`) has to tell the two apart. It is recorded
 *  in the calls the page scrolls through rather than by a flag each caller raises around its
 *  own write: a caller that moves a box through this module cannot forget to say that it did,
 *  and a caller that moves one any other way is not the page's place-keeping. */
const thePageLeftBoxesAt = new WeakMap<HTMLElement, number>();
let thePageLeftTheWindowAt: number | undefined;

/** Whether the page's own place-keeping is what left `scroller` - the page itself where null -
 *  standing where it stands now. A box the page moved and somebody else then moved stands
 *  somewhere else, and reads false. */
export function thePageLeftItThere(scroller: HTMLElement | null): boolean {
  const left = scroller === null ? thePageLeftTheWindowAt : thePageLeftBoxesAt.get(scroller);
  return left !== undefined && left === whereTheReaderIs(scroller);
}

/** Records that the page's own place-keeping has just left `scroller` - the page itself where
 *  null - standing where it stands now. The one place either record is written: every call
 *  below writes its scroll and then says so here, after the write, so what a box clamped the
 *  write to is what is recorded. */
function thePageLeftIt(scroller: HTMLElement | null): void {
  if (scroller === null) {
    thePageLeftTheWindowAt = whereTheReaderIs(null);
    return;
  }
  thePageLeftBoxesAt.set(scroller, whereTheReaderIs(scroller));
}

/** Scrolls `scroller` itself - the page where null - by `moved` pixels, recording where that
 *  left it as the page's own doing (above). It is `moveTheScroll` for a caller that names the
 *  box rather than what it is holding still in it: the Inbox's reading column is one box drawn
 *  in one place, and its placement writes it while the document in it is still drawing - the
 *  section it was opened at put back at the top on every pass, and the start of the column
 *  written for an approval request - so that writing is the page holding the reader's place,
 *  and every landing correction watching the same column reads it as such rather than as the
 *  reader taking the column over (`refocus-when-settled.ts`). */
export function moveTheBox(scroller: HTMLElement | null, moved: number): void {
  if (moved === 0) {
    return;
  }
  if (scroller === null) {
    window.scrollBy(0, moved);
  } else {
    scroller.scrollTop += moved;
  }
  thePageLeftIt(scroller);
}

/** Scrolls whatever carries `within` by `moved` pixels, so what the reader is looking at stays
 *  on the same line of their screen while the page changes above it. Where it leaves that box
 *  is recorded as the page's own doing (above). */
export function moveTheScroll(within: HTMLElement, moved: number): void {
  moveTheBox(whatScrolls(within), moved);
}

/** Puts `scroller` - the page itself where null - at `top`, recorded as the page's own doing
 *  (above). It is `moveTheBox` for the caller that knows where the box is to stand rather than
 *  how far it is to move: a list put back at its own start when its controls change, a live
 *  transcript written to its end, a panel put back where the reader left it. A `top` outside
 *  the box is the write the browser clamps - asking for `scrollHeight` is how a transcript
 *  asks for its own end - and what the box clamps it to is what is recorded. */
export function putTheBoxAt(scroller: HTMLElement | null, top: number): void {
  if (top === whereTheReaderIs(scroller)) {
    return;
  }
  if (scroller === null) {
    window.scrollTo({ top });
  } else {
    scroller.scrollTo({ top });
  }
  thePageLeftIt(scroller);
}

/** The band of the page's own coordinates `scroller` - the page where null - places an element
 *  in: its own edges less its `scroll-padding`, which is the part of the box a scroll is
 *  allowed to land content in (`html { scroll-padding-bottom: 5rem }` under 1280px,
 *  `styles.css`, which holds the fixed review sheet's height clear of whatever the page
 *  scrolls to). */
function placingPort(scroller: HTMLElement | null): { bottom: number; top: number } {
  const style = window.getComputedStyle(scroller ?? document.documentElement);
  const top = scrollportTop(scroller);
  const height = scrollportHeight(scroller);
  return {
    bottom: top + height - (Number.parseFloat(style.scrollPaddingBottom) || 0),
    top: top + (Number.parseFloat(style.scrollPaddingTop) || 0),
  };
}

/** Where `element` asks to be placed: its own box grown by its `scroll-margin`, which is what
 *  the browser aligns rather than the drawn box itself. */
function placingBox(element: HTMLElement): { bottom: number; top: number } {
  const style = window.getComputedStyle(element);
  const box = element.getBoundingClientRect();
  return {
    bottom: box.bottom + (Number.parseFloat(style.scrollMarginBottom) || 0),
    top: box.top - (Number.parseFloat(style.scrollMarginTop) || 0),
  };
}

/** How far the box that scrolls `element` must move to place it where `block` asks, by the rule
 *  the browser's own `scrollIntoView` follows (CSSOM View, "scroll a target into view"). */
function howFarIntoView(
  element: HTMLElement,
  scroller: HTMLElement | null,
  block: ScrollLogicalPosition
): number {
  const port = placingPort(scroller);
  const box = placingBox(element);
  const above = box.top < port.top;
  const below = box.bottom > port.bottom;
  switch (block) {
    case "start":
      return box.top - port.top;
    case "end":
      return box.bottom - port.bottom;
    case "center":
      return (box.top + box.bottom) / 2 - (port.top + port.bottom) / 2;
    default:
      // `nearest`: an element already in the port, and one so tall that it covers the port, are
      // left where they are; one off the top is aligned to the top and one off the bottom to the
      // bottom - except that one taller than the port is aligned to the top either way, since
      // bringing its end up would carry its first line out of sight.
      if (above === below) {
        return 0;
      }
      if (above || box.bottom - box.top > port.bottom - port.top) {
        return box.top - port.top;
      }
      return box.bottom - port.bottom;
  }
}

/**
 * Every box that scrolls `element`, nearest first and the page itself last: the chain the
 * browser's own `scrollIntoView` walks.
 *
 * It is a wider set than `whatScrolls` answers with, and deliberately. That one names the box
 * the READER scrolls, which is where their place is kept and which they can only be in if they
 * can drag it. This one names every box a scroll can be WRITTEN to: `hidden` is a scrolling box
 * the reader cannot drag and the page can still scroll - a dialog that clips its own column is
 * one, and an element drawn past the clip is reachable only by scrolling it - while `clip` is no
 * scrolling box at all. A box showing all of its own content scrolls nothing and is left out, so
 * no box is recorded as moved by a write that could not move it.
 */
function boxesThatScroll(element: HTMLElement): (HTMLElement | null)[] {
  const boxes: (HTMLElement | null)[] = [];
  for (let box = element.parentElement; box !== null; box = box.parentElement) {
    const overflow = window.getComputedStyle(box).overflowY;
    if (overflow !== "visible" && overflow !== "clip" && box.scrollHeight > box.clientHeight) {
      boxes.push(box);
    }
  }
  boxes.push(null);
  return boxes;
}

/** Puts `element` in view of the boxes that scroll it - the page itself last - where `block`
 *  asks it to stand, as `Element.scrollIntoView` would, recording where that left each box as
 *  the page's own doing (above). The browser's own call writes the scroll itself and
 *  tells nobody, so a page that moves itself through it is read as the reader taking the page
 *  over by every landing correction watching the same box (`refocus-when-settled.ts`); this one
 *  measures what that call would do and then writes it through `moveTheBox`, which says who
 *  did it.
 *
 *  It measures by the browser's own rule, `scroll-padding` and `scroll-margin` included, so
 *  `nearest` still moves nothing for an element already in the port and still brings the near
 *  edge to the near edge for one that is not - rather than a delta of this module's own
 *  invention. Each box is measured after the boxes inside it have moved, as the browser
 *  measures, so an outer box is asked only for what the inner ones left to do - which is
 *  nothing, for the usual element that one box brings into view. One thing the browser's call
 *  does it does not: it leaves the inline axis alone. Every caller of it names a block
 *  alignment only, of an element drawn in the box the reader is already looking at. A scroll it
 *  writes is instant, as the browser's is: nothing in this app asks for
 *  `scroll-behavior: smooth`. */
export function placeInView(
  element: HTMLElement,
  options?: { readonly block?: ScrollLogicalPosition }
): void {
  const block = options?.block ?? "start";
  for (const scroller of boxesThatScroll(element)) {
    moveTheBox(scroller, howFarIntoView(element, scroller, block));
  }
}

/** Puts the reader back where `place` left them, in a document `drawn` draws the same blocks of
 *  in the same order. A block the new draw does not have - a document edited between the two -
 *  leaves the page where it is, which is the browser's own scroll anchoring rather than a jump
 *  to the wrong words. */
export function putTheReaderBack(drawn: HTMLElement, place: ReadersBlock | undefined): void {
  const block = place === undefined ? undefined : drawn.children[place.index];
  if (block === undefined || place === undefined) {
    return;
  }
  const edge = scrollportTop(whatScrolls(drawn));
  moveTheScroll(drawn, block.getBoundingClientRect().top - edge - place.top);
}

/** Where the reader is held: an edge of an element, and where that edge sits under the top of
 *  the scrollport. The element rather than an index, since the run it is in grows under them. */
interface TheirPlace {
  readonly block: Element;
  /** Which edge of `block` is held: the top of the block the reader is on, or the bottom of a
   *  run that is wholly above them, which is the line the content under it starts on. */
  readonly edge: "bottom" | "top";
  readonly top: number;
}

/** Where that edge stands now, under the top of the scrollport. */
function whereItStands(drawn: HTMLElement, place: TheirPlace): number {
  const box = place.block.getBoundingClientRect();
  return (place.edge === "top" ? box.top : box.bottom) - scrollportTop(whatScrolls(drawn));
}

/**
 * The reader's place in `drawn`: the block they are on, or - where `drawn` is wholly above
 * them - its own bottom edge, which is where the content under it starts.
 *
 * The second is a run that is one item of a longer page rather than the page's own document: a
 * comment the reader has scrolled past, whose Markdown is still being formatted, or a body
 * holding nothing but its own text while the worker parses it, which has no block to hold at
 * all. Everything they are looking at sits under it, so what its formatting adds pushes all of
 * it down, and the line that has to stay put is the one its last block ends on
 * (`features/refs/useRenderedMarkdown.tsx`). A run the reader can see, or one still below
 * them, is theirs to watch land and is held by neither rule.
 */
function theirPlace(drawn: HTMLElement): TheirPlace | undefined {
  const where = readersBlock(drawn);
  if (where === undefined) {
    const under = drawn.getBoundingClientRect().bottom - scrollportTop(whatScrolls(drawn));
    return under <= 0 ? { block: drawn, edge: "bottom", top: under } : undefined;
  }
  const block = drawn.children[where.index];
  return block === undefined ? undefined : { block, edge: "top", top: where.top };
}

/**
 * Keeps the block the reader is on at the same line of their screen for as long as the document
 * `drawn` is on the page, whatever grows or shrinks above it. Returns the dispose.
 *
 * The correction is made where the growth is seen - a `MutationObserver`'s microtask, a
 * `ResizeObserver`'s callback - and never a frame later, so nothing between the growth and the
 * paint that shows it holds a page the reader did not ask to move. It is not the watch
 * `refocusWhenSettled` runs, which puts a focus back on its target in the frame after the
 * drawing around it moved: that is what a landing needs to end on its target, and a correction
 * a reader who is simply reading never asked for. The two live side by side on a block link -
 * the focus lands the reader, this keeps them there - and the triggers below say which growths
 * this answers and why.
 *
 * Nothing is read while the page is quiet: the page itself says when it has changed.
 *
 * It corrects only what nothing else has. The scroll it leaves behind is remembered, and a
 * scroll it did not make - the reader's own, a focus, the splice that takes the draw's blocks
 * off the page as the editor's batches replace them, or the browser's own scroll anchoring,
 * which moves the page for this same reason wherever it picks an anchor above the growth - is
 * read as the place being somebody else's now: the block the reader is on is taken afresh and
 * that frame corrects nothing. So two corrections never add up to twice the movement, and a
 * reader who scrolls is never pulled back to where they were.
 *
 * A reader still at the document's first block and above its top is not kept, as `readersBlock`
 * says: they are at the top of a page, where what arrives is theirs to see land. Nor is one
 * who can see the whole of a run that is an item of a longer page: `theirPlace` says which of
 * the two edges holds them.
 */
export function keepTheReadersPlace(drawn: HTMLElement): () => void {
  let place = theirPlace(drawn);
  /** Where this module left the scroll, so a scroll it did not make is told from its own. */
  let left = scrollOffset(drawn);
  const take = () => {
    place = theirPlace(drawn);
    left = scrollOffset(drawn);
  };
  const check = () => {
    const offset = scrollOffset(drawn);
    const kept = place;
    if (kept === undefined || !drawn.contains(kept.block) || offset !== left) {
      take();
      return;
    }
    const moved = whereItStands(drawn, kept) - kept.top;
    if (moved !== 0) {
      moveTheScroll(drawn, moved);
      left = scrollOffset(drawn);
    }
  };
  // A scroll is the reader, or somebody placing them - a block link's focus and its re-run, the
  // splice that takes the draw's blocks off the page as the editor's batches replace them, the
  // browser's own scroll anchoring - and none of those is this module's to undo: where they
  // left the reader is where the reader is now. It is read here rather than in a frame of its
  // own, at the cost of one box read per scrolled frame, because a growth landing between the
  // scroll and that frame would be measured against a place the reader had already left, and
  // answered by doing nothing: that is how the page kept a panel's 64 px for 844 ms.
  const scrolled = () => {
    const kept = place;
    if (kept === undefined || !drawn.contains(kept.block) || kept.edge === "bottom") {
      // A run held by its own bottom edge is held because the reader is past it, and a scroll
      // may have taken them off it or onto it: where they are now is taken afresh.
      take();
      return;
    }
    // The scrollport the reader reads the document in: the column in a reading pane, the page
    // on a document page. A block that has left it, above or below, is no longer theirs.
    const box = whatScrolls(drawn);
    const edge = scrollportTop(box);
    const { bottom, top } = kept.block.getBoundingClientRect();
    if (bottom <= edge || top >= edge + scrollportHeight(box)) {
      take();
      return;
    }
    place = { block: kept.block, edge: "top", top: top - edge };
    left = scrollOffset(drawn);
  };
  // Every growth is answered where it is seen, never a frame later. A mutation is answered in
  // the microtask after the task that made it, before anything can read the page's layout and
  // before it is painted, and the layout it reads is the one that task owed anyway. A resize is
  // answered in the `ResizeObserver` callback, which the browser runs after that layout and
  // before it paints. Asking for an animation frame instead is what failed: on this pod the
  // frame that answered the Subscribers panel's arrival came 73 ms after it, and 844 ms when
  // the thread was busy, and a page that has moved for that long has moved under its reader.
  const mutations = new MutationObserver(check);
  // The whole page, not the document alone, since a panel landing in the issue header moves the
  // document without touching it; and attributes and character data with it, because a panel
  // that arrives need not add an element - a count written into a span, a class that unhides a
  // row the page already held - and either changes the height above the reader just as much.
  mutations.observe(document.body, {
    attributes: true,
    characterData: true,
    childList: true,
    subtree: true,
  });
  // What changes height with no mutation of its own: a block the browser skipped painting at
  // its own height in place of the one `contain-intrinsic-size` estimated, a picture or a font
  // that loaded, a window resized. The document's own box, the page's, and the box carrying the
  // document where that is a column of its own rather than the page.
  const sizes = new ResizeObserver(check);
  sizes.observe(drawn);
  sizes.observe(document.body);
  const scroller = whatScrolls(drawn);
  if (scroller !== null) {
    sizes.observe(scroller);
  }
  // `capture`, because a column's scroll does not bubble; `passive`, because this reads the
  // offset and moves nothing.
  window.addEventListener("scroll", scrolled, { capture: true, passive: true });
  return () => {
    mutations.disconnect();
    sizes.disconnect();
    window.removeEventListener("scroll", scrolled, true);
  };
}

/** Every box `keepWhatStandsUnder` is watching, with how tall each last stood, and where the
 *  last correction left the scroll. One state for all of them, because two boxes that grow in
 *  one frame are one growth to answer: a guard per box would read the first box's own
 *  correction as somebody else's and leave the second box's growth on the reader. */
const boxesKept = new Map<HTMLElement, number>();
let keptTheScrollAt: number | undefined;

/** Answers every kept box's growth at once, and nothing another keeper has already answered. */
function keepWhatStandsUnderThem(): void {
  const [first] = boxesKept.keys();
  if (first === undefined) {
    return;
  }
  const offset = scrollOffset(first);
  if (offset !== keptTheScrollAt) {
    // The reader scrolled, or the document's own keeper answered this growth first: where
    // they are now is where they are, and nothing is answered for twice.
    for (const box of boxesKept.keys()) {
      boxesKept.set(box, box.getBoundingClientRect().height);
    }
    keptTheScrollAt = offset;
    return;
  }
  let grew = 0;
  for (const box of boxesKept.keys()) {
    const { height, top } = box.getBoundingClientRect();
    // Only where the box stands above the top of the reader's screen. A reader who can see it
    // is watching it land, and moving the page under them would take what they are looking at
    // off it; that is the line `readersBlock` draws for a reader still above the document's
    // first block.
    if (top < 0) {
      grew += height - (boxesKept.get(box) ?? height);
    }
    boxesKept.set(box, height);
  }
  if (grew !== 0) {
    moveTheScroll(first, grew);
  }
  keptTheScrollAt = scrollOffset(first);
}

/**
 * Keeps whatever stands under `box` where the reader has it, for as long as `box` is on the
 * page, and returns the dispose.
 *
 * The twin of `keepTheReadersPlace`, for a part of the page that is not a document: a box the
 * page keeps where its chrome will stand, which the chrome then lands in taller than the space
 * it replaced (`features/doc/AfterTheFirstBatch.tsx`). The document under it must not move,
 * and the chrome may land over several commits - its chunk's fallback first, the part itself
 * after it - so what is answered is every change of the box's own height rather than one
 * commit of it.
 *
 * It corrects only what nothing else has, by the same rule `keepTheReadersPlace` reads: a
 * scroll these keepers did not make - the reader's own, or the document's own keeper answering
 * the same growth - says the reader is placed where they are now. So the two run side by side
 * over one page.
 */
export function keepWhatStandsUnder(box: HTMLElement): () => void {
  boxesKept.set(box, box.getBoundingClientRect().height);
  keptTheScrollAt ??= scrollOffset(box);
  // A mutation is answered in the microtask after the task that made it, before anything can
  // read the page's layout and before it is painted; a resize - a font loaded, the window
  // resized, a row of the chrome wrapping - in the `ResizeObserver` callback, which the
  // browser runs after that layout and before it paints. Neither is a frame late, so nothing
  // between the growth and the paint that shows it holds a page the reader did not ask to move.
  const mutations = new MutationObserver(keepWhatStandsUnderThem);
  mutations.observe(box, {
    attributes: true,
    characterData: true,
    childList: true,
    subtree: true,
  });
  const sizes = new ResizeObserver(keepWhatStandsUnderThem);
  sizes.observe(box);
  // Where the reader has scrolled to is where they are, and a growth that lands in a later
  // frame is measured against that rather than against where they were.
  const scrolled = () => {
    keptTheScrollAt = scrollOffset(box);
  };
  window.addEventListener("scroll", scrolled, { capture: true, passive: true });
  return () => {
    mutations.disconnect();
    sizes.disconnect();
    window.removeEventListener("scroll", scrolled, true);
    boxesKept.delete(box);
    if (boxesKept.size === 0) {
      keptTheScrollAt = undefined;
    }
  };
}
