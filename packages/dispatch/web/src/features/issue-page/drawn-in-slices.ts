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
 * that fill one screen above it and one below - and every other row is drawn in slices after it.
 * Every word of every row is in the document once the slices have run, and none of them is
 * hidden by anything but the browser's `content-visibility` (`styles.css`); no row is taken out
 * of the document again.
 *
 * Each slice is a task of its own, asked of `scheduler.postTask` where the browser has it and of
 * `setTimeout(0)` where it does not, and it commits its rows with `flushSync` inside that task,
 * so React's render and commit of them, and the markdown each row paints in its layout effect,
 * are that task's and the time read around them is the task's own. A slice draws the rows after
 * the ones drawn, then the rows before them, nearest first, a batch at a time, until `SLICE_MS`
 * of wall time is spent, and starts no batch that would take it past `SLICE_LIMIT_MS` by what the
 * costliest row of the slice took; the next slice is asked for at once. So each slice but the
 * last is a task longer than the 50 ms the load's end waits a second without (Verification v4's
 * `waitForLoadEnd`), and the load cannot end while rows are still to be drawn, and an act that
 * lands during a slice waits at most `SLICE_LIMIT_MS` of R2's 200.
 *
 * A row a key, an address or the Contents list is to take the reader to is drawn first, with every
 * row between the anchor and it: a render names it among `needed`, and a key asks `drawUpTo` from
 * its handler before it moves the focus. Rows the given rows gain - a read that answers, an
 * unfold, a page's control pressed - are drawn as the rows around them are: where the row before
 * them is drawn, the first screen of them is drawn in the commit that brings them and the rest in
 * slices, and where it is not, they wait with it. A fold takes its rows out of the rows given, so
 * no slice draws them.
 *
 * A commit that draws rows above the row the reader reads leaves that row where it stood: the
 * focused row, or the first in the window where no row has the focus. The commit's layout effect
 * scrolls the window by how far that row moved, before the frame is painted, for the rows a slice,
 * a key or the first screen drew, and for the rows a read brought until the reader's first act
 * (`abortOnReaderAct`); after it, a read moves nothing, as at the base.
 */

/** How long one slice draws rows for: it ends once this much wall time is spent, read on
 *  `performance.now()`, which a slowed processor slows too. */
export const SLICE_MS = 60;

/** How long no slice runs to: it starts no batch of rows that would take it past this, by what
 *  its costliest row took, so an act that lands during it waits at most this of R2's 200 ms. */
export const SLICE_LIMIT_MS = 100;

/** How tall a row is taken to be before any is drawn, which tells the first commit how many rows
 *  fill a screen above the anchor and below it. A screen of rows shorter than this is filled in
 *  the same task, before the browser paints (`fillScreen`). It is the height `styles.css` holds a
 *  row's place at until the row is first laid out (`contain-intrinsic-block-size`). */
export const ROW_GUESS_PX = 96;

/** What the view's keys, its address and the Contents list ask of its rows. */
export interface RowSlices {
  /** Whether the row of `key` is drawn; a key the rows do not hold reads as drawn. */
  readonly isDrawn: (key: string) => boolean;
  /** Draws, in one commit made at once (`flushSync`), every row not drawn yet between the anchor
   *  and the row of `key`, that row included: what a key asks before it moves the focus there.
   *  It is for an event's handler; a render asks for a row by `needed`. */
  readonly drawUpTo: (key: string) => void;
}

/** One screen still to fill around a row the rows were drawn from: the anchor at the first
 *  drawing, both ways, and the first row of each run a later answer brought, downwards. */
interface Fill {
  readonly from: string;
  readonly up: boolean;
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
  /** The keys drawn as the last commit was laid out, and the row the reader reads then, with its
   *  top down the document, which the next commit holds in place. */
  drawnBefore: ReadonlySet<string>;
  reference: { readonly key: string; readonly top: number } | undefined;
  /** The keys the hook itself has drawn since the last commit was laid out: a slice's, a key's,
   *  a needed row's and a first screen's. */
  readonly drew: Set<string>;
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
    slicing: false,
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

  const slices = useMemo<RowSlices>(
    () => ({
      drawUpTo: (key) => {
        if (!ledger.held.has(key)) return;
        drawUpToKey(ledger, key);
        flushSync(() => setDrawing((drawings) => drawings + 1));
      },
      isDrawn: (key) => !ledger.held.has(key),
    }),
    [ledger]
  );

  // From the commit that mounts the rows, before the first slice can be asked for in the effect
  // below, to the commit that takes them off the page, which calls off the slice asked for.
  useLayoutEffect(() => {
    mounted.current = true;
    acts.current = abortOnReaderAct();
    return () => {
      mounted.current = false;
      acts.current?.abort();
      ledger.cancel?.();
      ledger.cancel = undefined;
    };
  }, [ledger]);

  // After every commit, before the frame is painted: the row the reader reads is put back where it
  // stood, the screens still to fill are filled, and a slice is asked for the rows left.
  useLayoutEffect(() => {
    const { focused: holding, measure: topOf } = ledger;
    keepReference(ledger, topOf, acts.current?.signal.aborted === true);
    ledger.drew.clear();
    ledger.drawnBefore = new Set(ledger.keys.filter((key) => !ledger.held.has(key)));
    ledger.reference = referenceRow(ledger, holding, topOf);
    let grew = false;
    ledger.fills = ledger.fills.filter((fill) => {
      const { done, drew } = fillScreen(ledger, fill, topOf);
      grew ||= drew;
      return !done;
    });
    if (grew) {
      setDrawing((drawings) => drawings + 1);
      return;
    }
    if (ledger.held.size === 0 || ledger.cancel !== undefined || ledger.slicing) return;
    const slice = (): void => {
      ledger.cancel = undefined;
      if (!mounted.current) return;
      ledger.slicing = true;
      try {
        drawSlice(ledger, () => flushSync(() => setDrawing((drawings) => drawings + 1)));
      } finally {
        ledger.slicing = false;
      }
      if (ledger.held.size > 0 && mounted.current) ledger.cancel = askForTask(slice);
    };
    ledger.cancel = askForTask(slice);
  });

  return { drawn, slices };
}

/** No key: one empty list for every render. */
const NO_KEYS: readonly string[] = [];

/**
 * Takes the rows' keys, in order, into `ledger`. Rows that share no key with the rows before are a
 * first drawing - the rows that place them have answered, or another view's rows stand in the
 * place of the last - and every row but the anchor's screen is held back, the screen guessed at
 * `ROW_GUESS_PX` a row and filled after the commit (`fillScreen`); a slice asked for the rows
 * before is called off. Otherwise each run of rows new to the ledger is drawn as the row before it
 * is, or the row after it where it comes first: held back where that row is, and else the first
 * screen of it drawn and the rest held back. A row that has left is forgotten.
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
    ledger.anchor = keys[from];
    for (const [index, key] of keys.entries()) {
      if (Math.abs(index - from) > screen) ledger.held.add(key);
    }
    if (ledger.anchor !== undefined) ledger.fills.push({ from: ledger.anchor, up: true });
    return;
  }
  for (const key of ledger.held) if (!at.has(key)) ledger.held.delete(key);
  for (let start = 0; start < keys.length; start += 1) {
    if (before.has(keys[start] ?? "")) continue;
    let end = start;
    while (end < keys.length && !before.has(keys[end] ?? "")) end += 1;
    const beside = keys[start - 1] ?? keys[end];
    const waits = beside !== undefined && ledger.held.has(beside);
    for (let index = start; index < end; index += 1) {
      if (waits || index - start > screen) ledger.held.add(keys[index] ?? "");
    }
    const first = keys[start];
    if (!waits && first !== undefined) ledger.fills.push({ from: first, up: false });
    start = end;
  }
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

/**
 * Puts the row the last commit was read by (`Ledger.reference`) back where it stood down the
 * document, where this commit drew rows above it that were not drawn then: rows the hook drew,
 * always, and rows a read or a fold brought, while the reader has not acted (`acted`). The window
 * is scrolled by how far the row moved, before the frame is painted.
 */
function keepReference(
  ledger: Ledger,
  measure: (key: string) => number | undefined,
  acted: boolean
): void {
  const reference = ledger.reference;
  const at = reference === undefined ? undefined : ledger.at.get(reference.key);
  if (reference === undefined || at === undefined || ledger.held.has(reference.key)) return;
  let drewAbove = false;
  let broughtAbove = false;
  for (let index = 0; index < at; index += 1) {
    const key = ledger.keys[index] ?? "";
    if (ledger.held.has(key) || ledger.drawnBefore.has(key)) continue;
    if (ledger.drew.has(key)) drewAbove = true;
    else broughtAbove = true;
  }
  if (!drewAbove && !(broughtAbove && !acted)) return;
  const top = measure(reference.key);
  if (top === undefined) return;
  const moved = top + window.scrollY - reference.top;
  if (moved !== 0) window.scrollBy({ behavior: "instant", top: moved });
}

/** The row the reader reads as this commit stands, with its top down the document: the focused
 *  row where it is drawn, else the first drawn row whose top is in the window, else the last
 *  whose top is above it. The drawn rows stand one under another, so the first in the window is
 *  found by halves. */
function referenceRow(
  ledger: Ledger,
  focused: string | undefined,
  measure: (key: string) => number | undefined
): Ledger["reference"] {
  const scrolled = window.scrollY;
  if (focused !== undefined && ledger.at.has(focused) && !ledger.held.has(focused)) {
    const top = measure(focused);
    if (top !== undefined) return { key: focused, top: top + scrolled };
  }
  const drawn = ledger.keys.filter((key) => !ledger.held.has(key));
  // The top of the `index`th drawn row, or of the first after it that is measured: a row the hook
  // is given with no element of its own, as the outline's page control is, is passed over.
  const topAt = (index: number): { key: string; top: number } | undefined => {
    for (let at = index; at < drawn.length; at += 1) {
      const key = drawn[at] ?? "";
      const top = measure(key);
      if (top !== undefined) return { key, top };
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
  return read === undefined ? undefined : { key: read.key, top: read.top + scrolled };
}

/**
 * Draws, for the screen `fill` names, the rows not drawn yet that the drawn rows next to its row do
 * not reach a screen with: below the row, and above it too for the anchor's own. How many is read
 * from the drawn rows' measured height, and where they measure no height, as where nothing is
 * laid out, every row not drawn that way is drawn. Answers whether the screen is filled, or can
 * fill no more, and whether it drew a row.
 */
function fillScreen(
  ledger: Ledger,
  fill: Fill,
  measure: (key: string) => number | undefined
): { readonly done: boolean; readonly drew: boolean } {
  const from = ledger.at.get(fill.from);
  const fromTop = measure(fill.from);
  if (from === undefined || fromTop === undefined || ledger.held.has(fill.from)) {
    return { done: true, drew: false };
  }
  const screen = window.innerHeight;
  let drew = false;
  let done = true;
  for (const by of fill.up ? [1, -1] : [1]) {
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
    const tall = rows === 0 ? 0 : reach / rows;
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
 * `SLICE_MS`, and none is started that would pass `SLICE_LIMIT_MS` by that row's cost. The first
 * batch is one row.
 */
function drawSlice(ledger: Ledger, commit: () => void): void {
  const started = performance.now();
  let costliest = 0;
  let batch = 1;
  while (ledger.held.size > 0) {
    const keys = nextHeld(ledger, batch);
    if (keys.length === 0) return;
    const began = performance.now();
    for (const key of keys) {
      ledger.held.delete(key);
      ledger.drew.add(key);
    }
    commit();
    const now = performance.now();
    costliest = Math.max(costliest, (now - began) / keys.length);
    const spent = now - started;
    if (spent >= SLICE_MS) return;
    if (costliest === 0) {
      batch = ledger.held.size;
      continue;
    }
    const room = Math.floor((SLICE_LIMIT_MS - spent) / costliest);
    if (room < 1) return;
    batch = Math.min(room, Math.max(1, Math.floor((SLICE_MS - spent) / costliest)));
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

/** The browser's `scheduler`, where it has one, by the one call the slices make of it. */
interface TaskScheduler {
  readonly postTask: (callback: () => void, options: { signal: AbortSignal }) => Promise<unknown>;
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

/** Asks for `run` as a task of its own: of `scheduler.postTask` where the browser has it, else of
 *  `setTimeout(0)`. Answers how to call it off. */
export function askForTask(run: () => void): () => void {
  const scheduler: unknown = Reflect.get(globalThis, "scheduler");
  if (isTaskScheduler(scheduler)) {
    const calledOff = new AbortController();
    // A task called off rejects its promise, which is the call itself.
    scheduler.postTask(run, { signal: calledOff.signal }).catch(() => undefined);
    return () => calledOff.abort();
  }
  const timer = setTimeout(run, 0);
  return () => clearTimeout(timer);
}
