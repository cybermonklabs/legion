import { TMUX_LAYOUT_MAX } from "@legion/contracts";

/**
 * tmux's `window_layout` string, read into the boxes it names. tmux writes it in one of two forms,
 * which its source names version 1 and version 2 (`layout-custom.c`), and a session reports
 * whichever its tmux wrote: the listener and Dispatch pass the string on unchanged.
 *
 * Version 1, written by tmux 3.7c and every tmux before it, measured from tmux 3.5a and 3.7c: four
 * hex digits of checksum, a comma, then the root cell as `<width>x<height>,<x>,<y>`. A cell
 * followed by `[` is split top to bottom and one followed by `{` is split left to right, each
 * holding its children as a comma-separated list inside the bracket; a cell with no bracket is a
 * pane, and the number after it is the pane's id without its leading `%`. tmux 3.7 to 3.7c, which
 * have floating panes (panes over the tiled ones, like a popup), keep each floating pane's cell
 * among the tiled cells and then name the floating panes again after the root cell, as
 * `<cell,cell,…>` in stacking order, the front first; a floating pane's offset may be negative.
 *
 * Version 2, written by tmux 3.8 and later ("Layout strings now use a JSON subset format which
 * includes floating panes", tmux's CHANGES for 3.8): the JSON object `{"V":2,"L":<root cell>}`.
 * Each cell is an object holding its type as `t` (`"v"` split top to bottom, `"h"` split left to
 * right, `"p"` a pane), its box as `w`, `h`, `x` and `y`, a split's children as the array `c`, and a
 * pane's id, with its `%`, as `I`, and its index in the window as `i`. A floating pane is a pane
 * cell with `z`, its place in the stack of the window's floating panes (0 the front), among its
 * split's children right after the pane it was made from, so the string's order is neither the
 * order tmux paints the panes in nor their index order. The members a drawing has no use for (`a`
 * and `l`, the active and last panes) are read past.
 *
 * In both forms tmux keeps a floating pane's box where it was put, so the box may reach past any
 * edge of the window, or lie wholly outside it; this parser reports the box as tmux wrote it, and
 * `drawWindow` clips it to the window. tmux writes a window whose only pane floats as that pane's
 * cell alone, so that layout states no size of the window at all.
 *
 * Every number is in terminal cells, as tmux states them. tmux spends one cell on the divider
 * between two panes and gives it to neither, so two panes beside each other in an 80-cell window
 * are 40 and 39 wide, and a drawing adds that cell back itself: this parser reports what tmux
 * wrote and invents no gap.
 *
 * Version 1's checksum is read for its shape and never recomputed. tmux's own checksum guards a
 * layout a person hands to `select-layout`; here the string came from tmux through a session's
 * registration, so a mismatch would say nothing a reader could act on, while a re-implementation
 * of it that was wrong in any corner would hide windows that draw perfectly well.
 */

/** A box in terminal cells, as the layout string states it. */
export interface TmuxBox {
  readonly width: number;
  readonly height: number;
  readonly x: number;
  readonly y: number;
}

/** A window's size in terminal cells. */
export interface TmuxSize {
  readonly width: number;
  readonly height: number;
}

/**
 * One pane of the window. `paneId` carries the leading `%` that version 1's string drops (`,5` is
 * `%5`) and version 2's keeps (`"I":"%5"`), because every other side of the hop writes it that way
 * — `TmuxPlace.pane_id`, the pane's own `$TMUX_PANE`, `tmux list-panes` — so a reader matching a
 * session to a pane never has to know which side stripped it.
 */
export interface TmuxLayoutPane extends TmuxBox {
  readonly kind: "pane";
  readonly paneId: string;
  /** The pane's index in its window (`pane_index`), where the form states it: version 2's `i`.
   *  Version 1 states none. */
  readonly index?: number;
  /** Where a floating pane sits in the stack of the window's floating panes, 0 the front:
   *  version 2's `z`, or the pane's place in version 1's `<…>` group. Absent on a tiled pane. */
  readonly z?: number;
}

export interface TmuxLayoutSplit extends TmuxBox {
  readonly kind: "split";
  /**
   * `rows` for version 1's `[` and version 2's `"t":"v"`, whose children sit one above the next;
   * `columns` for `{` and `"t":"h"`, whose children sit side by side. tmux's own words for these
   * ("vertical", "horizontal") name opposite things in `split-window` and in its documentation of
   * the layout, so neither is used here.
   */
  readonly into: "rows" | "columns";
  readonly children: readonly TmuxLayoutCell[];
}

export type TmuxLayoutCell = TmuxLayoutPane | TmuxLayoutSplit;

export interface TmuxLayout {
  readonly root: TmuxLayoutCell;
  /** The window's size: the root's box, wherever the root is not itself a floating pane. Absent
   *  where it is one, since that box is the pane's own and the window's size is in no cell. */
  readonly window?: TmuxSize;
  /** Every pane in the window, tiled and floating, in the order the layout string names them. */
  readonly panes: readonly TmuxLayoutPane[];
}

/** One pane as the window draws it: its cell, and the part of its box inside the window. */
export interface TmuxDrawnPane {
  readonly cell: TmuxLayoutPane;
  readonly box: TmuxBox;
}

/** A window as it is drawn: its size, and what of each pane is on its screen. */
export interface TmuxWindowDrawing {
  readonly window: TmuxSize;
  /**
   * The panes with any part inside the window, from the back to the front as tmux paints them:
   * the tiled ones in the layout's order, then the floating ones from the back of their stack to
   * its front (`z` 0 last). Each box is clipped to the window.
   */
  readonly shown: readonly TmuxDrawnPane[];
  /** The panes with no part inside the window: floating panes tmux let be moved off it. */
  readonly outside: readonly TmuxLayoutPane[];
}

/** Where the read has reached in the string; the only mutable state of the parse. */
interface Cursor {
  readonly text: string;
  at: number;
}

const CHECKSUM = /^[0-9a-f]{4},/;
/** The one version of tmux's JSON form there is (`layout-custom.c` at tmux 3.8: "currently 2"). */
const JSON_FORM_VERSION = 2;
/** A pane id as the JSON form writes it, with its `%`. */
const PANE_ID = /^%\d+$/;

function isDigit(code: number): boolean {
  // `charCodeAt` past the end is NaN, which fails both bounds, so this also ends the string.
  return code >= 48 && code <= 57;
}

function readDigits(cursor: Cursor): number | undefined {
  const start = cursor.at;
  while (isDigit(cursor.text.charCodeAt(cursor.at))) {
    cursor.at += 1;
  }
  return cursor.at === start ? undefined : Number(cursor.text.slice(start, cursor.at));
}

/** An offset, which tmux writes with `%d`: a floating pane left of or above the window has a
 *  negative one. */
function readOffset(cursor: Cursor): number | undefined {
  const negative = readLiteral(cursor, "-");
  const value = readDigits(cursor);
  return value === undefined ? undefined : negative ? -value : value;
}

function readLiteral(cursor: Cursor, literal: string): boolean {
  if (cursor.text[cursor.at] !== literal) {
    return false;
  }
  cursor.at += 1;
  return true;
}

function readChildren(cursor: Cursor, closer: "]" | "}"): TmuxLayoutCell[] | undefined {
  cursor.at += 1; // the opening bracket, which the caller has already read
  const children: TmuxLayoutCell[] = [];
  for (;;) {
    const child = readCell(cursor);
    if (child === undefined) {
      return undefined;
    }
    children.push(child);
    if (readLiteral(cursor, closer)) {
      return children;
    }
    if (!readLiteral(cursor, ",")) {
      return undefined;
    }
  }
}

function readCell(cursor: Cursor): TmuxLayoutCell | undefined {
  const width = readDigits(cursor);
  if (width === undefined || !readLiteral(cursor, "x")) {
    return undefined;
  }
  const height = readDigits(cursor);
  if (height === undefined || !readLiteral(cursor, ",")) {
    return undefined;
  }
  const x = readOffset(cursor);
  if (x === undefined || !readLiteral(cursor, ",")) {
    return undefined;
  }
  const y = readOffset(cursor);
  if (y === undefined) {
    return undefined;
  }
  // What follows the box says what the cell is: a bracket opens a split, a comma introduces the
  // pane id, and a cell that is neither is a cell tmux did not write.
  const opener = cursor.text[cursor.at];
  if (opener === "[" || opener === "{") {
    const children = readChildren(cursor, opener === "[" ? "]" : "}");
    return children === undefined
      ? undefined
      : {
          children,
          height,
          into: opener === "[" ? "rows" : "columns",
          kind: "split",
          width,
          x,
          y,
        };
  }
  if (!readLiteral(cursor, ",")) {
    return undefined;
  }
  const paneNumber = readDigits(cursor);
  return paneNumber === undefined
    ? undefined
    : { height, kind: "pane", paneId: `%${paneNumber}`, width, x, y };
}

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function isWhole(value: unknown): value is number {
  return typeof value === "number" && Number.isSafeInteger(value);
}

function isSize(value: unknown): value is number {
  return isWhole(value) && value >= 0;
}

/** One cell of the JSON form, with every cell under it, or undefined for a cell tmux did not
 *  write: one missing a member its type needs, or holding one of the wrong type, at any depth. */
function readJsonCell(value: unknown): TmuxLayoutCell | undefined {
  if (!isObject(value)) {
    return undefined;
  }
  const { c: cells, h: height, i: index, I: paneId, t: type, w: width, x, y, z } = value;
  if (!isSize(width) || !isSize(height) || !isWhole(x) || !isWhole(y)) {
    return undefined;
  }
  const box = { height, width, x, y };
  if (type === "p") {
    // tmux refuses a pane cell that holds cells (`layout_parse_json_layout`), and so does this.
    // `i` and `z` are counts from 0, and tmux writes `i` on every pane and `z` on a floating one.
    if (cells !== undefined || typeof paneId !== "string" || !PANE_ID.test(paneId)) {
      return undefined;
    }
    if ((index !== undefined && !isSize(index)) || (z !== undefined && !isSize(z))) {
      return undefined;
    }
    return {
      ...box,
      kind: "pane",
      paneId,
      ...(index === undefined ? {} : { index }),
      ...(z === undefined ? {} : { z }),
    };
  }
  // Only a pane floats, so a split holding a `z` is a cell tmux did not write.
  if (
    (type !== "v" && type !== "h") ||
    z !== undefined ||
    !Array.isArray(cells) ||
    cells.length === 0
  ) {
    return undefined;
  }
  const children: TmuxLayoutCell[] = [];
  for (const cell of cells) {
    const child = readJsonCell(cell);
    if (child === undefined) {
      return undefined;
    }
    children.push(child);
  }
  return { ...box, children, into: type === "v" ? "rows" : "columns", kind: "split" };
}

/** Version 2's root cell: the string must be the JSON object tmux writes, of the one version. */
function readJsonForm(layout: string): TmuxLayoutCell | undefined {
  let parsed: unknown;
  try {
    parsed = JSON.parse(layout);
  } catch {
    return undefined;
  }
  return isObject(parsed) && parsed.V === JSON_FORM_VERSION ? readJsonCell(parsed.L) : undefined;
}

/**
 * Version 1's group of floating panes after the root cell, `<cell,cell,…>`, front first: each a
 * pane the tree already holds at the same box, which the group marks floating with its place in
 * the stack. Undefined for a group that names anything else, or names a pane twice.
 */
function readFloatingGroup(cursor: Cursor): Map<string, TmuxLayoutPane> | undefined {
  const group = new Map<string, TmuxLayoutPane>();
  cursor.at += 1; // the `<`, which the caller has already read
  for (;;) {
    const cell = readCell(cursor);
    if (cell === undefined || cell.kind !== "pane" || group.has(cell.paneId)) {
      return undefined;
    }
    group.set(cell.paneId, { ...cell, z: group.size });
    if (readLiteral(cursor, ">")) {
      return group;
    }
    if (!readLiteral(cursor, ",")) {
      return undefined;
    }
  }
}

/** `cell` with every pane `floating` names marked as that floating pane, or undefined where a
 *  pane of the group is not at the box the tree gives it. */
function markFloating(
  cell: TmuxLayoutCell,
  floating: Map<string, TmuxLayoutPane>
): TmuxLayoutCell | undefined {
  if (cell.kind === "pane") {
    const named = floating.get(cell.paneId);
    if (named === undefined) {
      return cell;
    }
    const sameBox =
      named.width === cell.width &&
      named.height === cell.height &&
      named.x === cell.x &&
      named.y === cell.y;
    return sameBox ? named : undefined;
  }
  const children: TmuxLayoutCell[] = [];
  for (const child of cell.children) {
    const marked = markFloating(child, floating);
    if (marked === undefined) {
      return undefined;
    }
    children.push(marked);
  }
  return { ...cell, children };
}

/** Version 1's root cell: the checksum, then one cell, then tmux 3.7's group of floating panes
 *  if the window has any, which ends the string. */
function readStringForm(layout: string): TmuxLayoutCell | undefined {
  if (!CHECKSUM.test(layout)) {
    return undefined;
  }
  const cursor: Cursor = { at: 5, text: layout };
  const root = readCell(cursor);
  if (root === undefined) {
    return undefined;
  }
  if (cursor.at === layout.length) {
    return root;
  }
  if (layout[cursor.at] !== "<") {
    return undefined;
  }
  const floating = readFloatingGroup(cursor);
  if (floating === undefined || cursor.at !== layout.length) {
    return undefined;
  }
  const marked = markFloating(root, floating);
  // Every pane the group names is one the tree holds: tmux 3.7 keeps a floating pane's cell among
  // the tiled ones, so a group naming another pane is a string tmux did not write.
  if (
    marked === undefined ||
    collectPanes(marked).filter((pane) => pane.z !== undefined).length !== floating.size
  ) {
    return undefined;
  }
  return marked;
}

function collectPanes(cell: TmuxLayoutCell, into: TmuxLayoutPane[] = []): TmuxLayoutPane[] {
  if (cell.kind === "pane") {
    into.push(cell);
    return into;
  }
  for (const child of cell.children) {
    collectPanes(child, into);
  }
  return into;
}

/**
 * Reads a `window_layout` string of either form into its root cell, the window's size and its
 * panes, or answers undefined for a string neither grammar covers whole — a truncated or
 * unbalanced one, a cell that is neither a pane nor a split at any depth, anything after the root
 * cell but tmux 3.7's group of floating panes, a JSON form of another version, a pane named twice —
 * and for one longer than `TMUX_LAYOUT_MAX`, whatever it holds. A caller then has a window it
 * cannot draw rather than one it would draw wrongly.
 */
export function parseTmuxLayout(layout: string): TmuxLayout | undefined {
  // The cap is read here and not taken on trust from the sender. A session's client drops a
  // longer layout before it registers one, but the listener stores whatever a session sends and
  // Dispatch answers it unchanged, so a client with a session bearer could register a string
  // nested about one level per eight characters - and `readCell`/`readChildren` recurse once per
  // level, which within the cap is about a thousand levels, well inside every engine's stack. The
  // JSON form spends more than thirty characters on a level (`{"t":"v","w":0,"h":0,"x":0,"y":0,
  // "c":[`), so the same cap holds `readJsonCell`'s recursion shallower still. A refused layout
  // costs that window its drawing and nothing else: it falls back to its list of panes.
  if (layout.length > TMUX_LAYOUT_MAX) {
    return undefined;
  }
  const root = layout.startsWith("{") ? readJsonForm(layout) : readStringForm(layout);
  if (root === undefined) {
    return undefined;
  }
  const panes = collectPanes(root);
  // A pane is in one cell of its window, so a string naming one twice is not one tmux wrote, and a
  // drawing keyed by pane id could not draw both.
  if (new Set(panes.map((pane) => pane.paneId)).size !== panes.length) {
    return undefined;
  }
  const floatingRoot = root.kind === "pane" && root.z !== undefined;
  return {
    panes,
    root,
    ...(floatingRoot ? {} : { window: { height: root.height, width: root.width } }),
  };
}

/** The part of `box` inside a window of `size`, or undefined where none of it is. */
function clip(box: TmuxBox, size: TmuxSize): TmuxBox | undefined {
  const left = Math.max(box.x, 0);
  const top = Math.max(box.y, 0);
  const right = Math.min(box.x + box.width, size.width);
  const bottom = Math.min(box.y + box.height, size.height);
  return right > left && bottom > top
    ? { height: bottom - top, width: right - left, x: left, y: top }
    : undefined;
}

/**
 * The window `layout` is drawn as: its size, which is the layout's own where the layout states
 * one and `reported` (the size the session reported beside the layout) where the root is a
 * floating pane, and each pane's box clipped to it, in the order tmux paints them. Undefined where
 * neither gives a size of more than nothing, since there is then no box to draw the panes in.
 *
 * tmux paints every tiled pane first and the floating panes over them, the back of their stack
 * first (`screen-redraw.c` walks the window's z-index list from its tail), so a floating pane is
 * over every tiled pane whatever their order in the string, and `z` 0 is over every other
 * floating pane. A floating pane is clipped to the window the way tmux clips it on the screen.
 */
export function drawWindow(
  layout: TmuxLayout,
  reported: TmuxSize | undefined
): TmuxWindowDrawing | undefined {
  const window = layout.window ?? reported;
  if (window === undefined || window.width <= 0 || window.height <= 0) {
    return undefined;
  }
  const tiled = layout.panes.filter((pane) => pane.z === undefined);
  // `sort` is stable, so two floating panes at one `z` - which tmux never writes - keep the
  // layout's order.
  const floating = layout.panes
    .filter((pane) => pane.z !== undefined)
    .sort((back, front) => (front.z ?? 0) - (back.z ?? 0));
  const shown: TmuxDrawnPane[] = [];
  const outside: TmuxLayoutPane[] = [];
  for (const cell of [...tiled, ...floating]) {
    const box = clip(cell, window);
    if (box === undefined) {
      outside.push(cell);
    } else {
      shown.push({ box, cell });
    }
  }
  return { outside, shown, window };
}
