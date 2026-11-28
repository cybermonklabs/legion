import { useLayoutEffect, useMemo, useRef, useState } from "react";
import { flushSync } from "react-dom";

import { abortOnReaderAct } from "./useNodeEditing";

/**
 * A view's rows drawn the first screen first, and the others in slices (the plan's state table
 * "The rows drawn in slices", `drawn-in-slices.test.tsx`).
 *
 * A view drew every row in one commit, and that commit was the longest task of the load: React's
 * render and commit of every row, and each row's markdown painted in its layout effect, up to
 * 3,161 ms at 390 px on Graph (`t6`'s profile). So the first commit draws the anchor's screen -
 * the anchor, the row the page's opening or the address names, else the first row, with the rows
 * that fill one screen above it and one below, or from the first row the rows that fill the window
 * below it alone - and every other row is drawn in slices after it.
 * Every word of every row is in the document once the slices have run, and no row is hidden or
 * taken out of the document again (R4: no row is windowed out; the architect's ruling of
 * dispatch://INFRA-5939/comment/42f48939-b821-43d5-b30a-2ac645a1cbf6 took out the
 * `content-visibility` rule that once skipped rows far from the screen).
 *
 * Each slice is a task of its own, asked of `scheduler.postTask` where the browser has it and of
 * `setTimeout(0)` where it does not, and it commits its rows with `flushSync` inside that task,
 * so React's render and commit of them, and the markdown each row paints in its layout effect,
 * are that task's and the time read around them is the task's own. A slice draws the rows after
 * the ones drawn, then the rows before them, nearest first, a batch at a time, until `SLICE_MS`
 * of wall time is spent, and starts no batch that would take it past `SLICE_LIMIT_MS` by what the
 * costliest row of the slice took, nor a batch that row puts over `COMMIT_MS`. The next slice
 * is queued at background priority, so input and painting take priority. With no input pending,
 * slices aim to spend 60 ms so `waitForLoadEnd` does not finish during a long run of slices.
 * If input is pending, the slice ends after its current commit rather than starting another.
 *
 * A row a key, an address or the Contents list is to take the reader to is drawn first, with every
 * row between the anchor and it: a render names it among `needed`, and a key asks `drawUpTo` from
 * its handler before it moves the focus. The row an address names takes the focus only once every
 * row up to it is drawn (`whenDrawnUpTo`): from the first drawing on it stands on screen alone
 * with the rows that fill a screen around it, and a reader who read the page from the focus would
 * find the rows above it still to come. Rows the given rows gain - a read that answers, an
 * unfold, a page's control pressed - are drawn as the rows around them are: where the row before
 * them is drawn, the first screen of them is drawn in the commit that brings them and the rest in
 * slices, and where it is not, they wait with it. A fold takes its rows out of the rows given, so
 * no slice draws them.
 *
 * A commit that draws rows above the row the reader reads leaves that row where it stood: the
 * focused row, or the first in the window where no row has the focus. The commit's layout effect
 * scrolls the window by how far that row moved, before the frame is painted, for the rows a slice,
 * a key or the first screen drew, and for the rows a read brought until the reader's first act
 * (`abortOnReaderAct`); after it, a read moves nothing, as at the base. That row is read once, and
 * again only where the reader scrolled, the focus moved, or the rows above it changed: a commit
 * that draws rows below it alone reads no layout, so the rows a slice draws are laid out once, as
 * the browser paints, and not in each commit.
 */

/** How long one slice draws rows for: it ends once this much wall time is spent, read on
 *  `performance.now()`, which a slowed processor slows too. */
export const SLICE_MS = 60;

/** How long no slice runs to: it starts no batch of rows that would take it past this, by what
 *  its costliest row took, so an act that lands during it waits at most this of R2's 200 ms. */
export const SLICE_LIMIT_MS = 100;

/** Target time for one batch, estimated from the costliest batch so far. Leave headroom for
 *  rows that take longer than the estimate. */
export const COMMIT_MS = 25;

/** How tall a row is taken to be before any is drawn, which tells the first commit how many rows
 *  fill a screen above the anchor and below it, and the fill from the first row how many fill the
 *  window below it while it stands alone. A screen of rows shorter than this is filled in the same
 *  task, before the browser paints (`fillScreen`). */
export const ROW_GUESS_PX = 96;

/** What the view's keys, its address and the Contents list ask of its rows. */
export interface RowSlices {
  /** Whether the row of `key` is drawn; a key the rows do not hold reads as drawn. */
  readonly isDrawn: (key: string) => boolean;
  /** Draws, in one commit made at once (`flushSync`), every row not drawn yet between the anchor
   *  and the row of `key`, that row included: what a key asks before it moves the focus there.
   *  It is for an event's handler; a render asks for a row by `needed`. */
  readonly drawUpTo: (key: string) => void;
  /** Calls `then` once every row from the first to the row of `key`, that row included, is drawn:
   *  at once where they are, else in the layout effect of the commit that draws the last of them,
   *  before its frame is painted. A key the rows do not hold reads as drawn. Answers how to call
   *  it off. */
  readonly whenDrawnUpTo: (key: string, then: () => void) => () => void;
}

/** One screen still to fill around a row the rows were drawn from: the anchor at the first
 *  drawing, both ways, and the first row of each run a later answer brought, downwards. A first
 *  drawing from the first row (`window`) fills the window from that row to its foot, rather than a
 *  screen's height from it. */
interface Fill {
  readonly from: string;
  readonly up: boolean;
  readonly window?: boolean;
}

/** What the hook holds across renders, read and written outside React's state since a render, a
 *  layout effect and a slice's task each read and write it. */
interface Ledger {
  /** The rows as last given, by their keys, and the place of each among them. */
  rows: readonly unknown[] | undefined;
  keys: readonly string[];
  at: ReadonlyMap<string, number>;
  /** The keys of the rows not drawn yet. */
  readonly held: Set<string>;
  /** The anchor's key, from the first drawing on. */
  anchor: string | undefined;
  /** The screens still to fill (`fillScreen`). */
  fills: Fill[];
  /** The last commit's drawn keys and reference row. Its `top` is the desired viewport top
   *  plus the scroll position, not the realized top after a rounded scroll (`keepReference`). */
  drawnBefore: ReadonlySet<string>;
  reference: { readonly above: number; readonly key: string; readonly top: number } | undefined;
  /** The window's scroll as the hook last read it or left it, the scroll right before a commit
   *  that will hold the reference in place (`scrollBefore`), and whether a scroll the hook did not
   *  make has come since (`useDrawnInSlices`' scroll listener): the reader scrolled, so the row
   *  they read may be another. */
  scrollBefore: number;
  scrollY: number | undefined;
  scrolled: boolean;
  /** The keys the hook itself has drawn since the last commit was laid out: a slice's, a key's,
   *  a needed row's and a first screen's. */
  readonly drew: Set<string>;
  /** Who waits for every row up to a row to be drawn (`RowSlices.whenDrawnUpTo`), by that row's
   *  key, each told once. */
  readonly waiting: Set<{ readonly key: string; readonly then: () => void }>;
  /** How to call off the slice asked for, while one is. */
  cancel: (() => void) | undefined;
  /** Whether a slice's task is running now. */
  slicing: boolean;
  /** The key of the focused row and the measure of a row's top, as the last render gave them. */
  focused: string | undefined;
  measure: (key: string) => number | undefined;
}

/** A ledger that holds no row yet. */
function newLedger(): Ledger {
  return {
    anchor: undefined,
    at: new Map(),
    cancel: undefined,
    drawnBefore: new Set(),
    drew: new Set(),
    fills: [],
    focused: undefined,
    held: new Set(),
    keys: NO_KEYS,
    measure: () => undefined,
    reference: undefined,
    rows: undefined,
    scrollBefore: 0,
    scrolled: false,
    scrollY: undefined,
    slicing: false,
    waiting: new Set(),
  };
}

/** The rows `rows` to draw now, by the state table "The rows drawn in slices": `anchor` is the key
 *  of the row the page's opening or the address names, read at the first drawing, else the first
 *  row; `focused` the key of the row that holds the focus, which a commit that draws rows above
 *  it leaves where it stood; `measure` the top of a drawn row in the window, or undefined where
 *  it is not drawn; `needed` the keys of the rows a key, an address or the Contents list is taking
 *  the reader to in this render. */
export function useDrawnInSlices<Row>({
  anchor,
  focused,
  keyOf,
  measure,
  needed = NO_KEYS,
  rows,
}: {
  readonly anchor: string | undefined;
  readonly focused?: string | undefined;
  readonly keyOf: (row: Row) => string;
  readonly measure: (key: string) => number | undefined;
  readonly needed?: readonly string[];
  readonly rows: readonly Row[];
}): { readonly drawn: readonly Row[]; readonly slices: RowSlices } {
  const [, setDrawing] = useState(0);
  const [ledger] = useState(newLedger);
  // What the layout effect reads of this render.
  ledger.focused = focused;
  ledger.measure = measure;
  // The reader's first act, after which a read that brings rows above the row they read moves
  // nothing (the first drawing's row 5).
  const acts = useRef<AbortController | undefined>(undefined);
  // Whether the rows are still on the page, which a slice asked for before they left reads.
  const mounted = useRef(false);

  if (rows !== ledger.rows) {
    ledger.rows = rows;
    takeRows(ledger, rows.map(keyOf), anchor);
  }
  for (const key of needed) drawUpToKey(ledger, key);
  const drawn =
    ledger.held.size === 0
      ? rows
      : rows.filter((_row, at) => !ledger.held.has(ledger.keys[at] ?? ""));
  if (willKeepReference(ledger, acts.current?.signal.aborted === true)) {
    ledger.scrollBefore = window.scrollY;
  }
  const slices = useMemo<RowSlices>(
    () => ({
      drawUpTo: (key) => {
        if (!ledger.held.has(key)) return;
        drawUpToKey(ledger, key);
        flushSync(() => setDrawing((drawings) => drawings + 1));
      },
      isDrawn: (key) => !ledger.held.has(key),
      whenDrawnUpTo: (key, then) => {
        const waiter = { key, then };
        if (drawnUpTo(ledger, key)) then();
        else ledger.waiting.add(waiter);
        return () => ledger.waiting.delete(waiter);
      },
    }),
    [ledger]
  );

  // From the commit that mounts the rows, before the first slice can be asked for in the effect
  // below, to the commit that takes them off the page, which calls off the slice asked for.
  useLayoutEffect(() => {
    mounted.current = true;
    acts.current = abortOnReaderAct();
    // A scroll the hook did not make - the reader's, or one the browser made - may put another row
    // first in the window, so the row the reader reads is read again after the next commit. The
    // window's scroll is read here, in the frame the scroll is shown in, where its layout is
    // made anyway.
    const onScroll = (): void => {
      if (window.scrollY !== ledger.scrollY) ledger.scrolled = true;
    };
    window.addEventListener("scroll", onScroll, { passive: true });
    return () => {
      mounted.current = false;
      window.removeEventListener("scroll", onScroll);
      acts.current?.abort();
      ledger.cancel?.();
      ledger.cancel = undefined;
      noteHeld(ledger, false);
      ledger.waiting.clear();
    };
  }, [ledger]);

  // After every commit, before the frame is painted: the row the reader reads is put back where it
  // stood, those who wait for the rows up to a row are told where this commit drew the last of
  // them, the screens still to fill are filled, and a slice is asked for the rows left. The row
  // the reader reads is read again only where it may have changed (`referenceStale`), so a commit
  // that draws rows below it alone, the reader not having scrolled, reads no layout: the browser
  // lays the rows out once, as it paints.
  useLayoutEffect(() => {
    const { focused: holding, measure: topOf } = ledger;
    const acted = acts.current?.signal.aborted === true;
    noteHeld(ledger, mounted.current);
    keepReference(ledger, topOf, acted);
    ledger.drew.clear();
    ledger.drawnBefore = new Set(ledger.keys.filter((key) => !ledger.held.has(key)));
    for (const waiter of ledger.waiting) {
      if (!drawnUpTo(ledger, waiter.key)) continue;
      ledger.waiting.delete(waiter);
      waiter.then();
    }
    let grew = false;
    if (ledger.fills.length > 0) {
      ledger.fills = ledger.fills.filter((fill) => {
        const { done, drew } = fillScreen(ledger, fill, topOf);
        grew ||= drew;
        return !done;
      });
    }
    if (grew) {
      setDrawing((drawings) => drawings + 1);
      return;
    }
    if ((ledger.held.size > 0 || !acted) && referenceStale(ledger, holding)) {
      ledger.reference = referenceRow(ledger, holding, topOf);
    }
    if (ledger.held.size === 0 || ledger.cancel !== undefined || ledger.slicing) return;
    const slice = (): void => {
      ledger.cancel = undefined;
      if (!mounted.current) return;
      // While the slices are held (`holdSlices`), the slice waits to be let go, and is asked for
      // again then.
      if (slicesHeld) {
        const resume = (): void => {
          ledger.cancel = undefined;
          if (mounted.current && ledger.held.size > 0) {
            ledger.cancel = askForTask(slice, "background");
          }
        };
        heldSlices.add(resume);
        ledger.cancel = () => heldSlices.delete(resume);
        return;
      }
      ledger.slicing = true;
      try {
        drawSlice(ledger, () => flushSync(() => setDrawing((drawings) => drawings + 1)));
      } finally {
        ledger.slicing = false;
      }
      if (ledger.held.size > 0 && mounted.current) {
        ledger.cancel = askForTask(slice, "background");
      }
    };
    ledger.cancel = askForTask(slice, "background");
  });

  return { drawn, slices };
}

/** No key: one empty list for every render. */
const NO_KEYS: readonly string[] = [];

/** Whether the slices of every view are held (`holdSlices`), and the slices waiting to be let go. */
let slicesHeld = false;
const heldSlices = new Set<() => void>();

/**
 * Holds every view's slices where they stand, or lets them go: while a press on the view row waits
 * on the reads of the view it turned on (`view-press.tsx`), the view it left draws no row, so no
 * row the press did not bring is drawn after it, and the page's processor goes to those reads and
 * the view they draw. A slice asked for while they are held draws nothing and is asked for again
 * once they are let go.
 */
export function holdSlices(hold: boolean): void {
  slicesHeld = hold;
  if (hold) return;
  const waiting = [...heldSlices];
  heldSlices.clear();
  for (const resume of waiting) resume();
}
/** The ledgers of the mounted views that still hold a row back, and who to tell as that set
 *  empties or fills: the page's end of the load waits for every row of the drawn pages to be in
 *  the document (`load-end.ts`). */
const viewsHolding = new Set<Ledger>();
const viewsHoldingListeners = new Set<() => void>();

/** Whether every row of every mounted view is in the document: no view's slices are left. */
export function everyRowDrawn(): boolean {
  return viewsHolding.size === 0;
}

/** Calls `listener` each time `everyRowDrawn` may have changed; answers how to stop. */
export function onRowsDrawnChange(listener: () => void): () => void {
  viewsHoldingListeners.add(listener);
  return () => viewsHoldingListeners.delete(listener);
}

/** Takes `ledger` into the views that hold a row back, or out of them, as its rows stand after a
 *  commit (`mounted`) or as its view leaves the page, and tells the listeners where that changed
 *  the set. */
function noteHeld(ledger: Ledger, mounted: boolean): void {
  const holds = mounted && ledger.held.size > 0;
  if (holds === viewsHolding.has(ledger)) return;
  if (holds) viewsHolding.add(ledger);
  else viewsHolding.delete(ledger);
  for (const listener of viewsHoldingListeners) listener();
}

/**
 * Takes the rows' keys, in order, into `ledger`. Rows that share no key with the rows before are a
 * first drawing - the rows that place them have answered, or another view's rows stand in the
 * place of the last - and every row but the anchor's screen is held back; a slice asked for the
 * rows before is called off. From a row further down, which the page lands on, the screen is
 * guessed at `ROW_GUESS_PX` a row above it and below it, and filled after the commit
 * (`fillScreen`). From the first row, the screen is the anchor alone, and the rows that fill the
 * window below it are drawn once it is laid out, in the same task: where the rows stand below the
 * window's foot, as under the children's section at 390 px, none is drawn with it. Otherwise each
 * run of rows new to the ledger is drawn as the row before it is, or the row after it where it
 * comes first: held back where that row is, and else the first screen of it drawn and the rest
 * held back. A row that has left is forgotten.
 */
function takeRows(ledger: Ledger, keys: readonly string[], anchor: string | undefined): void {
  const before = ledger.at;
  const at = new Map(keys.map((key, index) => [key, index]));
  const screen = Math.ceil(window.innerHeight / ROW_GUESS_PX);
  ledger.keys = keys;
  ledger.at = at;
  if (!keys.some((key) => before.has(key))) {
    ledger.cancel?.();
    ledger.cancel = undefined;
    ledger.held.clear();
    ledger.fills = [];
    ledger.reference = undefined;
    ledger.drawnBefore = new Set();
    const from = Math.max(0, anchor === undefined ? 0 : (at.get(anchor) ?? 0));
    const around = from === 0 ? 0 : screen;
    ledger.anchor = keys[from];
    for (const [index, key] of keys.entries()) {
      if (Math.abs(index - from) > around) ledger.held.add(key);
    }
    if (ledger.anchor !== undefined) {
      ledger.fills.push({ from: ledger.anchor, up: true, window: from === 0 });
    }
    return;
  }
  for (const key of ledger.held) if (!at.has(key)) ledger.held.delete(key);
  for (let start = 0; start < keys.length; start += 1) {
    if (before.has(keys[start] ?? "")) continue;
    let end = start;
    while (end < keys.length && !before.has(keys[end] ?? "")) end += 1;
    const beside = keys[start - 1] ?? keys[end];
    const waits = beside !== undefined && ledger.held.has(beside);
    let heldAny = false;
    for (let index = start; index < end; index += 1) {
      if (waits || index - start > screen) {
        ledger.held.add(keys[index] ?? "");
        heldAny = true;
      }
    }
    const first = keys[start];
    if (!waits && heldAny && first !== undefined) ledger.fills.push({ from: first, up: false });
    start = end;
  }
}

/** Whether every row from the first to the row of `key`, that row included, is drawn: a key the
 *  rows do not hold reads as drawn. */
function drawnUpTo(ledger: Ledger, key: string): boolean {
  const to = ledger.at.get(key);
  if (to === undefined || ledger.held.size === 0) return true;
  for (let index = 0; index <= to; index += 1) {
    if (ledger.held.has(ledger.keys[index] ?? "")) return false;
  }
  return true;
}

/** Draws every row not drawn yet between the anchor and the row of `key`, that row included: from
 *  the nearest drawn row where the anchor has left the rows. Nothing where the row is drawn. */
function drawUpToKey(ledger: Ledger, key: string): void {
  const to = ledger.at.get(key);
  if (to === undefined || !ledger.held.has(key)) return;
  const anchored = ledger.anchor === undefined ? undefined : ledger.at.get(ledger.anchor);
  const from = anchored ?? nearestDrawn(ledger, to) ?? to;
  for (let index = Math.min(from, to); index <= Math.max(from, to); index += 1) {
    const one = ledger.keys[index] ?? "";
    if (ledger.held.delete(one)) ledger.drew.add(one);
  }
}

/** The place of the drawn row nearest the `to`th, or undefined where none is drawn. */
function nearestDrawn(ledger: Ledger, to: number): number | undefined {
  for (let by = 1; by < ledger.keys.length; by += 1) {
    for (const index of [to - by, to + by]) {
      const key = ledger.keys[index];
      if (key !== undefined && !ledger.held.has(key)) return index;
    }
  }
  return undefined;
}

/** Whether the pending commit draws rows above `ledger.reference` that `keepReference` will hold
 *  in place: pure JS with no layout read, so `window.scrollY` is read before commit only when
 *  `keepReference` will actually run. */
function willKeepReference(ledger: Ledger, acted: boolean): boolean {
  const reference = ledger.reference;
  const at = reference === undefined ? undefined : ledger.at.get(reference.key);
  if (reference === undefined || at === undefined || ledger.held.has(reference.key)) return false;
  for (let index = 0; index < at; index += 1) {
    const key = ledger.keys[index] ?? "";
    if (ledger.held.has(key) || ledger.drawnBefore.has(key)) continue;
    if (ledger.drew.has(key) || !acted) return true;
  }
  return false;
}

/** Restores the reference's viewport position after a commit adds rows above it. */
function keepReference(
  ledger: Ledger,
  measure: (key: string) => number | undefined,
  acted: boolean
): void {
  if (!willKeepReference(ledger, acted)) return;
  const reference = ledger.reference;
  const at = reference === undefined ? undefined : ledger.at.get(reference.key);
  if (reference === undefined || at === undefined) return;
  const top = measure(reference.key);
  if (top === undefined) return;
  // Measure in the viewport so any scroll anchoring the browser already did is taken into
  // account. Keep the same target across slices rather than adopting each rounded result
  // as the next target: a series of subpixel differences otherwise adds up to whole pixels.
  const wanted = reference.top - ledger.scrollBefore;
  const moved = Math.round(top) - Math.round(wanted);
  if (moved !== 0) window.scrollBy({ behavior: "instant", top: moved });
  const scrolled = window.scrollY;
  ledger.scrollY = scrolled;
  ledger.reference = { above: drawnAbove(ledger, at), key: reference.key, top: wanted + scrolled };
}

/** Picks a new reference before a slice only if the viewport has moved since it was recorded. */
function freshReference(ledger: Ledger): void {
  if (ledger.reference === undefined || ledger.scrolled || window.scrollY !== ledger.scrollY) {
    ledger.reference = referenceRow(ledger, ledger.focused, ledger.measure);
  }
}

/** Whether the row the reader reads must be read again after this commit: none is held; the
 *  reader scrolled since it was read; the focus is on another drawn row; it is no longer drawn;
 *  or the drawn rows above it are not those it was read with, and `keepReference` did not hold it
 *  where it stood. Where none of these holds, its top down the document is the one held. */
function referenceStale(ledger: Ledger, focused: string | undefined): boolean {
  const reference = ledger.reference;
  if (reference === undefined || ledger.scrolled) return true;
  if (focused !== undefined && focused !== reference.key && ledger.at.has(focused)) {
    if (!ledger.held.has(focused)) return true;
  }
  const at = ledger.at.get(reference.key);
  if (at === undefined || ledger.held.has(reference.key)) return true;
  return drawnAbove(ledger, at) !== reference.above;
}

/** How many of the rows before the `at`th are drawn. */
function drawnAbove(ledger: Ledger, at: number): number {
  let drawn = 0;
  for (let index = 0; index < at; index += 1) {
    if (!ledger.held.has(ledger.keys[index] ?? "")) drawn += 1;
  }
  return drawn;
}

/** The row the reader reads as this commit stands, with its top down the document and how many
 *  drawn rows stand above it: the focused row where it is drawn, which is the one top read, else
 *  the first drawn row whose top is in the window, else the last whose top is above it. The drawn
 *  rows stand one under another, so the first in the window is found by halves, without reading
 *  every drawn row's top. The window's scroll is read with it (`Ledger.scrollY`). */
function referenceRow(
  ledger: Ledger,
  focused: string | undefined,
  measure: (key: string) => number | undefined
): Ledger["reference"] {
  const scrolled = window.scrollY;
  ledger.scrollY = scrolled;
  ledger.scrolled = false;
  if (focused !== undefined && ledger.at.has(focused) && !ledger.held.has(focused)) {
    const top = measure(focused);
    const at = ledger.at.get(focused) ?? 0;
    if (top !== undefined)
      return { above: drawnAbove(ledger, at), key: focused, top: top + scrolled };
  }
  const drawn = ledger.keys.filter((key) => !ledger.held.has(key));
  // The top of the `index`th drawn row, or of the first after it that is measured: a row the hook
  // is given with no element of its own, as the outline's page control is, is passed over.
  const topAt = (index: number): { at: number; key: string; top: number } | undefined => {
    for (let at = index; at < drawn.length; at += 1) {
      const key = drawn[at] ?? "";
      const top = measure(key);
      if (top !== undefined) return { at, key, top };
    }
    return undefined;
  };
  let low = 0;
  let high = drawn.length;
  while (low < high) {
    const middle = (low + high) >>> 1;
    const read = topAt(middle);
    if (read !== undefined && read.top < 0) low = middle + 1;
    else high = middle;
  }
  const read =
    topAt(low) ??
    (low > 0 ? topAt(low - 1) : undefined) ??
    (drawn.length > 0 ? topAt(0) : undefined);
  return read === undefined
    ? undefined
    : { above: read.at, key: read.key, top: read.top + scrolled };
}

/**
 * Draws, for the screen `fill` names, the rows not drawn yet that the drawn rows next to its row do
 * not reach a screen with: below the row, and above it too for the anchor's own; for a first
 * drawing from the first row, the rows that reach from it to the window's foot and no further than
 * a screen, and none where it stands below the foot. How many is read from the drawn rows'
 * measured height, `ROW_GUESS_PX` a row where the row stands alone, and where they measure no
 * height, as where nothing is laid out, every row not drawn that way is drawn. Answers whether the
 * screen is filled, or can fill no more, and whether it drew a row.
 */
function fillScreen(
  ledger: Ledger,
  fill: Fill,
  measure: (key: string) => number | undefined
): { readonly done: boolean; readonly drew: boolean } {
  if (ledger.held.size === 0) return { done: true, drew: false };
  const from = ledger.at.get(fill.from);
  const fromTop = measure(fill.from);
  if (from === undefined || fromTop === undefined || ledger.held.has(fill.from)) {
    return { done: true, drew: false };
  }
  let drew = false;
  let done = true;
  for (const by of fill.up ? [1, -1] : [1]) {
    // How far from the row the drawn rows are to reach that way: a screen, or from the first row
    // no further than the window's foot.
    const screen =
      fill.window === true && by === 1
        ? Math.min(window.innerHeight, window.innerHeight - fromTop)
        : window.innerHeight;
    if (screen <= 0) continue;
    // The drawn rows next to the row, that way, up to the first that reaches a screen from it, and
    // how far the last of them that is measured stands from it.
    let edge = from;
    let reach = 0;
    let rows = 0;
    for (let index = from + by; index >= 0 && index < ledger.keys.length; index += by) {
      const key = ledger.keys[index] ?? "";
      if (ledger.held.has(key)) break;
      edge = index;
      const top = measure(key);
      if (top === undefined) continue;
      reach = Math.abs(top - fromTop);
      rows = Math.abs(index - from);
      if (reach >= screen) break;
    }
    if (reach >= screen) continue;
    const next = edge + by;
    if (next < 0 || next >= ledger.keys.length) continue;
    const tall = rows === 0 ? (fill.window === true ? ROW_GUESS_PX : 0) : reach / rows;
    const wanted = tall === 0 ? ledger.keys.length : Math.ceil((screen - reach) / tall);
    let taken = 0;
    for (
      let index = next;
      index >= 0 && index < ledger.keys.length && taken < wanted;
      index += by
    ) {
      const key = ledger.keys[index] ?? "";
      if (!ledger.held.delete(key)) break;
      ledger.drew.add(key);
      taken += 1;
    }
    if (taken > 0) {
      drew = true;
      done = false;
    }
  }
  return { done, drew };
}

/**
 * One slice: the rows not drawn yet after the anchor, in order, then those before it, nearest
 * first, a batch at a time, each committed by `commit` (`flushSync`), until `SLICE_MS` is spent;
 * a batch is as many rows as the costliest row of the slice so far leaves room for before
 * `SLICE_MS`, and no more than it puts in `COMMIT_MS`, and none is started that would pass
 * `SLICE_LIMIT_MS` by that row's cost. The first batch is one row. Before a batch that draws rows
 * above the row the reader reads, that row's top is read as it stands (`freshReference`), so the
 * commit holds it where the reader last saw it, whatever moved it since it was read.
 */
function drawSlice(ledger: Ledger, commit: () => void): void {
  const started = performance.now();
  let costliest = 0;
  let batch = 1;
  while (ledger.held.size > 0) {
    const keys = nextHeld(ledger, batch);
    if (keys.length === 0) return;
    const reference =
      ledger.reference === undefined ? undefined : ledger.at.get(ledger.reference.key);
    if (reference === undefined || keys.some((key) => (ledger.at.get(key) ?? 0) < reference)) {
      freshReference(ledger);
    }
    const began = performance.now();
    for (const key of keys) {
      ledger.held.delete(key);
      ledger.drew.add(key);
    }
    commit();
    const now = performance.now();
    costliest = Math.max(costliest, (now - began) / keys.length);
    const spent = now - started;
    if (spent >= SLICE_MS || inputPending()) return;
    if (costliest === 0) {
      batch = ledger.held.size;
      continue;
    }
    const room = Math.floor((SLICE_LIMIT_MS - spent) / costliest);
    if (room < 1) return;
    batch = Math.min(
      room,
      Math.max(1, Math.floor(Math.min(SLICE_MS - spent, COMMIT_MS) / costliest))
    );
  }
}

/** The next `count` rows not drawn yet, in the order the slices draw them: after the anchor, then
 *  before it, nearest first. */
function nextHeld(ledger: Ledger, count: number): string[] {
  const anchored = ledger.anchor === undefined ? undefined : ledger.at.get(ledger.anchor);
  const from = anchored ?? 0;
  const next: string[] = [];
  for (let index = from + 1; index < ledger.keys.length && next.length < count; index += 1) {
    const key = ledger.keys[index] ?? "";
    if (ledger.held.has(key)) next.push(key);
  }
  for (let index = from; index >= 0 && next.length < count; index -= 1) {
    const key = ledger.keys[index] ?? "";
    if (ledger.held.has(key)) next.push(key);
  }
  return next;
}

/** The priority `askForTask` asks `scheduler.postTask` for: `background` yields to user input,
 *  the browser's paint and `MessageChannel` tasks, and runs at once while none of those waits. */
type TaskPriority = "background" | "user-visible";

/** The browser's `scheduler`, where it has one, by the one call the slices make of it. */
interface TaskScheduler {
  readonly postTask: (
    callback: () => void,
    options: { priority?: TaskPriority; signal: AbortSignal }
  ) => Promise<unknown>;
}

/** Whether `value` is the browser's `scheduler`, which holds `postTask`: Chromium's and
 *  Firefox's do, and WebKit holds none, nor do the DOMs the unit tests run in. */
function isTaskScheduler(value: unknown): value is TaskScheduler {
  return (
    typeof value === "object" &&
    value !== null &&
    "postTask" in value &&
    typeof value.postTask === "function"
  );
}

/** Whether a reader's input waits on the browser's queue (`navigator.scheduling.isInputPending`,
 *  which Chromium holds): a slice ends after its current commit (`COMMIT_MS`) so the input runs
 *  next rather than waiting out the rest of the slice. */
function inputPending(): boolean {
  const nav: unknown = Reflect.get(globalThis, "navigator");
  if (typeof nav !== "object" || nav === null) return false;
  const scheduling: unknown = Reflect.get(nav, "scheduling");
  if (typeof scheduling !== "object" || scheduling === null) return false;
  const check: unknown = Reflect.get(scheduling, "isInputPending");
  return typeof check === "function" && check.call(scheduling) === true;
}

/** Asks for `run` as a task of its own: of `scheduler.postTask` where the browser has it, at
 *  `priority`, else of `setTimeout(0)`. Answers how to call it off. */
export function askForTask(run: () => void, priority: TaskPriority = "user-visible"): () => void {
  const scheduler: unknown = Reflect.get(globalThis, "scheduler");
  if (isTaskScheduler(scheduler)) {
    const calledOff = new AbortController();
    // A task called off rejects its promise, which is the call itself.
    scheduler.postTask(run, { priority, signal: calledOff.signal }).catch(() => undefined);
    return () => calledOff.abort();
  }
  const timer = setTimeout(run, 0);
  return () => clearTimeout(timer);
}

/** Calls `then` once the browser has painted the frame it draws next: in the task that frame's
 *  animation callback queues, as the page reads a frame painted, and ahead of the page's work
 *  asked for at background priority (`askForTask`). Answers how to call it off. */
export function afterPaint(then: () => void): () => void {
  let painted: number | undefined;
  const frame = requestAnimationFrame(() => {
    painted = window.setTimeout(then, 0);
  });
  return () => {
    cancelAnimationFrame(frame);
    window.clearTimeout(painted);
  };
}
