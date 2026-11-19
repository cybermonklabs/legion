import { flushSync } from "react-dom";

/** The prototype's `CUT_LINES`: how many lines of an agent's words or an answered question a row
 *  draws before it cuts them. */
const CUT_LINES = 6;

/** How many words a cut must hide before it is made: fewer are drawn whole. */
const CUT_WORDS = 20;

/** How far past six lines a text may run and still be drawn whole, as the prototype allows. */
const CUT_SLACK = 12;

/** How far past the viewport, above it and below it, a row is near enough to be measured: one
 *  screen each way. */
const NEAR_BAND = "100% 0px";

/** How long one frame's slice of a batch reads for at most. One batch that read every near row
 *  of a large issue at once held the main thread for 414 ms; a batch of more rows goes on in the
 *  frames after it, each slice reading before it writes. */
const SLICE_MS = 50;

/** A row of the outline, of any kind: the rows a slice holds in place stand one under another,
 *  siblings in document order, as the outline (`TreeRows.tsx`) draws them. */
const ROW = "[data-node-row]";

/** What a row draws of words it cuts: how tall it draws them, how many words it holds back,
 *  and whether the cut was moved to the foot of a row of pictures the sixth line crossed. */
export interface Clamp {
  readonly height: number;
  readonly hidden: number;
  readonly pictures: boolean;
}

/** What a row is told of its words: their cut, or `undefined` where they are drawn whole. */
type Report = (clamp: Clamp | undefined) => void;

/** One row a slice read: its words, their cut, and the top of their box as the slice read it. */
interface Measured {
  readonly clamp: Clamp | undefined;
  readonly element: HTMLElement;
  readonly top: number;
}

/** The row whose top a slice holds in place, and that top as the slice read it before writing. */
interface Anchor {
  readonly element: HTMLElement;
  readonly top: number;
}

/** One row's words, as the measurer holds them. */
interface Watched {
  /** The words' box and the children it held when the row was handed over: the boxes whose
   *  size the measurer watches for this row. */
  readonly boxes: readonly Element[];
  /** The cut the row was last told, or `undefined` while its words are drawn whole or before
   *  they are first measured. */
  clamp: Clamp | undefined;
  /** How many times the page's typeface had arrived when the row's cut was last measured, or
   *  `undefined` before it is first measured. A count below the measurer's own marks a cut
   *  measured on an earlier face: a stale cut. */
  face: number | undefined;
  /** Whether the words stand within `NEAR_BAND` of the viewport. */
  near: boolean;
  readonly report: Report;
  /** The words' box's width when the size observer last saw it. */
  width: number | undefined;
}

/**
 * The one measurer of every cut the new issue page draws. Each row that may be cut hands it its
 * words (`useClamp`), and it measures them in batches, one slice of at most `SLICE_MS` a frame,
 * every read of a slice before any write. Each row once measured its own words from its own
 * layout effect, after the writes the rows before it had made in the same commit, so the browser
 * laid the whole outline out again for each row: on a production-size issue that took 7 to 28 s
 * of every draw of the outline after a view change.
 *
 * Where the browser has an `IntersectionObserver`, it measures only the words within one screen
 * of the viewport: the observer says which, and the words' own box confirms it as the slice
 * reads it. Where it has none, every row's words are measured. It measures them again when a
 * block of them changes size, or their box changes width or grows while they are whole (one
 * `ResizeObserver`). Words that come near are measured in the next batch; words that go away
 * keep the cut they last had.
 *
 * The page's typeface arrives after the first frame (`font-display: swap`, one `loadingdone`
 * listener), and words laid out again in it can cross the sixth line where the fallback's did
 * not, or hold back another count of words. So its arrival marks stale every row whose cut was
 * measured before it, near or far, and no row keeps a cut measured on the fallback face: a
 * stale row outside the view is measured in the next batch, wherever it is laid out, and a
 * stale row in view once it lies wholly outside the view, as every row in view is.
 *
 * A row that stands in view as it mounts is measured once the commit's layout effects have run,
 * with every other such row of the commit, and its cut drawn before the page is painted, so the
 * reader never sees it whole: the rows in the first view of a page that opens, and a row a
 * branch's opening draws in view. Every other row is measured in the batches, a frame after it
 * comes near, never in the frame it arrives in.
 *
 * A long jump into the middle of the page, such as a fast drag of the scrollbar, lands on rows
 * not measured yet, and their cuts the frame after moved the content in view (-1,141 px once in
 * five jumps from 60,000 px to 15,000 px): the browser's own scroll anchoring holds the view only
 * while the rows that shrink are outside it. Holding one row in place was not enough either: a
 * jump into a row taller than the window lost the text the reader was on to that row's own cut,
 * and a row under the held one shrank in view (the middle of the view moved in 7 of 12 jumps, by
 * up to -1,340 px at 390 px). So a slice cuts only rows that lie wholly outside the view. A row in
 * view, wholly or in part, keeps its length, unmeasured, and every scroll puts it in the next
 * slice again, which cuts it once it lies wholly outside the view. A slice whose cuts change a
 * row above the view reads, before any write, the top of the first row of the outline wholly in
 * view, of any kind, or of the row that spans the view's top where none is wholly in view; writes
 * its cuts at once (`flushSync`) rather than in a later render; and then scrolls the page by
 * however far that row moved, in the same frame: that scroll is its last write. A slice that
 * changes only rows below the view does none of it.
 */
class ClampMeasurer {
  readonly #rows = new Map<Element, Watched>();
  /** The rows the next batch measures. */
  readonly #due = new Set<HTMLElement>();
  /** The rows a slice found in view, which keep their length until a scroll leaves them wholly
   *  outside the view. */
  readonly #held = new Set<HTMLElement>();
  /** The rows handed over in the commit now running, which `#measureMounted` reads once its
   *  layout effects have all run. */
  readonly #mounted: HTMLElement[] = [];
  /** The row each box the size observer watches belongs to. */
  readonly #owners = new Map<Element, HTMLElement>();
  readonly #fonts: FontFaceSet | undefined = document.fonts;
  /** How many times the page's typeface has arrived (`loadingdone`). */
  #faces = 0;
  readonly #near: IntersectionObserver | undefined;
  readonly #sizes: ResizeObserver | undefined;
  #frame: number | undefined;

  constructor() {
    this.#near =
      typeof IntersectionObserver === "undefined"
        ? undefined
        : new IntersectionObserver(
            (entries) => {
              for (const entry of entries) {
                const watched = this.#rows.get(entry.target);
                if (watched === undefined || !(entry.target instanceof HTMLElement)) continue;
                watched.near = entry.isIntersecting;
                this.#queue(entry.target);
              }
            },
            { rootMargin: NEAR_BAND }
          );
    this.#sizes =
      typeof ResizeObserver === "undefined"
        ? undefined
        : new ResizeObserver((entries) => {
            for (const entry of entries) {
              const element = this.#owners.get(entry.target);
              const watched = element === undefined ? undefined : this.#rows.get(element);
              if (element === undefined || watched === undefined) continue;
              // The cut sets the box's height itself, so a cut box measured again only for a new
              // width: its words' own growing is seen on their blocks.
              if (entry.target === element) {
                const { width } = entry.contentRect;
                const widened = width !== watched.width;
                watched.width = width;
                if (!widened && watched.clamp !== undefined) continue;
              }
              this.#queue(element);
            }
          });
    // The page's typeface arrives after the first frame, with no change of the box a resize
    // would see.
    this.#fonts?.addEventListener("loadingdone", this.#faceArrived);
    // A scroll of the page, or of any box the rows stand in, can leave a row held in view wholly
    // outside it.
    document.addEventListener("scroll", this.#viewMoved, { capture: true, passive: true });
  }

  /** Measures `element`'s words from the next batch on and tells `report` each new cut, until
   *  the answer is called. Called from the row's layout effect: where the row stands in view, its
   *  cut is measured and drawn before the page is painted (`#measureMounted`). */
  watch(element: HTMLElement, report: Report): () => void {
    // The words' own box is held to the cut while it is drawn, so their growing - a reading that
    // paints after the row, a picture that loads - is seen on the blocks they are drawn in.
    const boxes = [element, ...element.children];
    this.#rows.set(element, {
      boxes,
      clamp: undefined,
      face: undefined,
      near: this.#near === undefined,
      report,
      width: undefined,
    });
    this.#near?.observe(element);
    for (const box of boxes) {
      this.#owners.set(box, element);
      this.#sizes?.observe(box);
    }
    this.#mounted.push(element);
    if (this.#mounted.length === 1) queueMicrotask(this.#measureMounted);
    this.#queue(element);
    return () => this.#unwatch(element);
  }

  /** Whether no row is watched any more. */
  get idle(): boolean {
    return this.#rows.size === 0;
  }

  /** Stops every observer and the batch still to run. */
  stop(): void {
    this.#near?.disconnect();
    this.#sizes?.disconnect();
    this.#fonts?.removeEventListener("loadingdone", this.#faceArrived);
    document.removeEventListener("scroll", this.#viewMoved, { capture: true });
    if (this.#frame !== undefined) window.cancelAnimationFrame(this.#frame);
    this.#frame = undefined;
  }

  #unwatch(element: HTMLElement): void {
    const watched = this.#rows.get(element);
    if (watched === undefined) return;
    this.#rows.delete(element);
    this.#due.delete(element);
    this.#held.delete(element);
    this.#near?.unobserve(element);
    for (const box of watched.boxes) {
      this.#owners.delete(box);
      this.#sizes?.unobserve(box);
    }
  }

  /** Puts `element` in the next batch, where it is near. */
  #queue(element: HTMLElement): void {
    if (this.#rows.get(element)?.near !== true) return;
    this.#due.add(element);
    this.#frame ??= window.requestAnimationFrame(this.#measure);
  }

  /** Marks stale every row whose cut was measured before the typeface arrived, by counting the
   *  arrival, and puts each in the next batch, near or far: one outside the view is measured
   *  there, and one in view is held until it lies wholly outside the view. */
  readonly #faceArrived = (): void => {
    this.#faces += 1;
    for (const [element, watched] of this.#rows) {
      if (watched.face !== undefined && element instanceof HTMLElement) this.#due.add(element);
    }
    if (this.#due.size > 0) this.#frame ??= window.requestAnimationFrame(this.#measure);
  };

  /** The rows handed over since the commit that mounted them, measured where they stand in view
   *  once that commit's layout effects have all run, and before the page is painted: every read
   *  of them comes first, after the writes of every layout effect, so the page is laid out for
   *  them once; then their cuts are drawn at once (`flushSync`). A row outside the view is left
   *  to the batches. */
  readonly #measureMounted = (): void => {
    const screen = window.innerHeight;
    const read: Measured[] = [];
    for (const element of this.#mounted.splice(0)) {
      if (!this.#rows.has(element)) continue;
      const box = element.getBoundingClientRect();
      if ((box.width === 0 && box.height === 0) || !inView(element, box, screen)) continue;
      read.push({ clamp: measureClamp(element, box.top), element, top: box.top });
    }
    if (read.length > 0) flushSync(() => this.#write(read));
  };

  /** Puts every row held in view in the next batch, which cuts the ones a scroll left wholly
   *  outside the view. */
  readonly #viewMoved = (): void => {
    if (this.#held.size === 0) return;
    for (const element of this.#held) this.#due.add(element);
    this.#frame ??= window.requestAnimationFrame(this.#measure);
  };

  /** One slice of the batch: reads for at most `SLICE_MS`, then every write of what it read,
   *  so the page is laid out once for all of them; the rest of the batch goes on next frame.
   *  A row in view is held, unmeasured, for a later slice. Where the writes change a row above
   *  the view, a row of the outline near the view's top is read before them and held where it
   *  stood after them (`#anchorOf`). */
  readonly #measure = (): void => {
    this.#frame = undefined;
    const until = performance.now() + SLICE_MS;
    const screen = window.innerHeight;
    const read: Measured[] = [];
    let taken = 0;
    for (const element of this.#due) {
      if (taken > 0 && performance.now() >= until) break;
      taken += 1;
      this.#due.delete(element);
      this.#held.delete(element);
      const box = element.getBoundingClientRect();
      // With no observer to say which rows come near as the page scrolls, nothing would queue
      // a far row again, so every row is measured. A stale row is measured wherever it is laid
      // out; any other row only within `NEAR_BAND`.
      const face = this.#rows.get(element)?.face;
      const stale = face !== undefined && face < this.#faces;
      const laidOut = box.width > 0 || box.height > 0;
      if (this.#near !== undefined && !(stale ? laidOut : withinBand(box))) continue;
      if (inView(element, box, screen)) {
        this.#held.add(element);
        continue;
      }
      read.push({ clamp: measureClamp(element, box.top), element, top: box.top });
    }
    const anchor = this.#anchorOf(read, screen);
    if (anchor === undefined) this.#write(read);
    else {
      // The rows draw their cuts now, in this frame, so the anchor's new top can be read and the
      // page scrolled before the frame is painted.
      flushSync(() => this.#write(read));
      if (anchor.element.isConnected) {
        const moved = anchor.element.getBoundingClientRect().top - anchor.top;
        if (moved !== 0) window.scrollBy({ behavior: "instant", top: moved });
      }
    }
    if (this.#due.size > 0) this.#frame = window.requestAnimationFrame(this.#measure);
  };

  /** Tells each row `read` holds the cut read of it. */
  #write(read: readonly Measured[]): void {
    for (const { clamp, element } of read) {
      const watched = this.#rows.get(element);
      if (watched === undefined) continue;
      watched.clamp = clamp;
      watched.face = this.#faces;
      watched.report(clamp);
    }
  }

  /** The row a slice that read `read` holds in place, with its top read before the slice writes
   *  anything (`anchorRow`), where one of the slice's cuts changes a row above the view; the
   *  slice reads no row in view, and a change below the view moves nothing in it. */
  #anchorOf(read: readonly Measured[], screen: number): Anchor | undefined {
    const changed = read.find(
      ({ clamp, element, top }) => top < screen && !sameClamp(this.#rows.get(element)?.clamp, clamp)
    );
    return changed === undefined ? undefined : anchorRow(changed.element, screen);
  }
}

/** The measurer while a row is watched: made by the first row and stopped after the last. */
let measurer: ClampMeasurer | undefined;

/** Hands `element`'s words to the page's one measurer (`ClampMeasurer`), which tells `report`
 *  each new cut of them until the answer is called. */
export function watchClamp(element: HTMLElement, report: Report): () => void {
  const held = measurer ?? new ClampMeasurer();
  measurer = held;
  const unwatch = held.watch(element, report);
  return () => {
    unwatch();
    if (!held.idle) return;
    held.stop();
    if (measurer === held) measurer = undefined;
  };
}

/** Whether the words' box, as the slice read it, is laid out within `NEAR_BAND` of the viewport.
 *  The intersection observer's word is from the frame it was computed in: words drawn before the
 *  page laid them out where they stand, or not laid out at all, can be reported near there. A
 *  box with no size is measured once it has one, which the size observer reports. */
function withinBand(box: DOMRect): boolean {
  if (box.width === 0 && box.height === 0) return false;
  const screen = window.innerHeight;
  return box.bottom >= -screen && box.top <= 2 * screen;
}

/** Whether the row that holds the words `element` holds, whose box was read as `box`, stands in
 *  view, wholly or in part. The row's whole box decides, since it holds the words: words above
 *  the view whose row runs on into it stand in view too. */
function inView(element: HTMLElement, box: DOMRect, screen: number): boolean {
  const row = element.closest(ROW);
  const span = row === null ? box : row.getBoundingClientRect();
  return span.bottom > 0 && span.top < screen;
}

/**
 * The row of the outline a slice holds in place, among the rows `element`'s own row stands with,
 * with its top: the first row wholly in view, of any kind, or where none is, the row whose box
 * spans the view's top, which it does where its top is at or above that top and its foot below
 * it (top <= 0 < bottom): a row taller than the view whose top is exactly at the view's top
 * spans it. It reads only the rows near the view's top: a binary search over the rows' boxes
 * finds the first whose foot is below the top, and the walk from there ends at the first row
 * that starts in view. Words outside the outline's rows hold nothing in place.
 */
function anchorRow(element: HTMLElement, screen: number): Anchor | undefined {
  const rows = element.closest(ROW)?.parentElement?.children;
  if (rows === undefined) return undefined;
  let low = 0;
  let high = rows.length;
  while (low < high) {
    const middle = (low + high) >>> 1;
    if ((rows[middle]?.getBoundingClientRect().bottom ?? 0) > 0) high = middle;
    else low = middle + 1;
  }
  let spanning: Anchor | undefined;
  for (let at = low; at < rows.length; at += 1) {
    const row = rows[at];
    if (!(row instanceof HTMLElement) || !row.matches(ROW)) continue;
    const box = row.getBoundingClientRect();
    if (box.height === 0) continue;
    if (box.top >= screen) break;
    if (box.top >= 0 && box.bottom <= screen) return { element: row, top: box.top };
    // Every row after one that starts below the view's top starts lower still, so where that row
    // runs past the view's foot, no row is wholly in view.
    if (box.top > 0) break;
    spanning ??= { element: row, top: box.top };
  }
  return spanning;
}

/** Whether two cuts draw a row alike: the same height, the same words held back and the same
 *  place of the cut, or both whole. */
export function sameClamp(one: Clamp | undefined, other: Clamp | undefined): boolean {
  return (
    one?.height === other?.height &&
    one?.hidden === other?.hidden &&
    one?.pictures === other?.pictures
  );
}

/** The cut of the words `element` holds, whose box's top the slice read at `top`, as the
 *  prototype's `measureClamps` reads it: layout reads alone. */
function measureClamp(element: HTMLElement, top: number): Clamp | undefined {
  const pitch = Number.parseFloat(getComputedStyle(element).lineHeight);
  const limit = CUT_LINES * (Number.isFinite(pitch) ? pitch : 24);
  if (element.scrollHeight <= limit + CUT_SLACK) return undefined;
  // The words a cut would hide, block by block: a block below the line whole, and of a block the
  // line crosses the share of its words below it.
  let hidden = 0;
  for (const block of blocksOf(element)) {
    const box = block.getBoundingClientRect();
    const from = box.top - top;
    const to = box.bottom - top;
    const count = (block.textContent ?? "").split(/\s+/u).filter(Boolean).length;
    if (from >= limit) hidden += count;
    else if (to > limit && box.height > 0) hidden += count * ((to - limit) / box.height);
  }
  const held = Math.round(hidden);
  if (held <= CUT_WORDS) return undefined;
  // A row of pictures the line crosses is drawn whole, and the cut moves to its foot.
  let height = limit;
  let pictures = false;
  for (const picture of element.querySelectorAll("img")) {
    const box = picture.getBoundingClientRect();
    if (box.top - top < limit && box.bottom - top > limit) {
      height = Math.max(height, box.bottom - top + 6);
      pictures = true;
    }
  }
  return { height, hidden: held, pictures };
}

/** The blocks of the words `element` holds: the paragraphs, lists and pictures of a text drawn
 *  from its reading, and the one paragraph of a text drawn as stored. */
function blocksOf(element: HTMLElement): Element[] {
  const drawn = element.querySelector(".dispatch-markdown");
  if (drawn !== null && drawn.children.length > 0) return [...drawn.children];
  const first = element.firstElementChild;
  return first === null ? [] : [first];
}
