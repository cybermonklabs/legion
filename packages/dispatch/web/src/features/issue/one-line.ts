import { type RefObject, useLayoutEffect, useState } from "react";

import { drawnLine, firstLineOf } from "./issue-tree";

/**
 * What the Contents pane reads once for every entry it draws on one line (dispatch://INFRA-1196
 * R1, R14): how wide its list is, and what an entry needs to say how wide its own words are
 * drawn. The outline's rows draw their words whole (the architect's ruling 1 on R1); an entry of
 * the list is one of the places the page draws one line for a node.
 *
 * An entry cuts its node's first line at a word to the room its own box leaves it, and no entry
 * may read its own layout to learn that room: that is a layout read per entry, and again on
 * every resize. So the list reads one width - its own, which is every entry's before its indent
 * - with one `ResizeObserver` (`useOneLineBox`), and each entry sums its own parts against it:
 * the parts its classes size in pixels, and its words, measured on a canvas in the face the
 * list sets them in, which lays out nothing on the page.
 */
export interface OneLineBox {
  /** The face the tree's words are set in, as the tree's computed `font-family`. */
  readonly family: string;
  /** How many CSS pixels one `rem` is, which every part of a row its classes size is in. */
  readonly rem: number;
  /** The tree's own width, in CSS pixels. */
  readonly width: number;
  /** How wide `text` is drawn at `size` rem in the tree's face, at `weight` where it is not the
   *  face's regular one, in CSS pixels. */
  readonly widthOf: (text: string, size: number, weight?: number) => number;
}

/**
 * The list's one reading of its own width, for its one-line entries: taken as the list is first
 * laid out, before it is painted, again whenever its box changes size, by one `ResizeObserver`
 * on the list and none on any entry, and again whenever the document's fonts finish loading. A
 * reading that says what it said before leaves the entries alone. Undefined while `oneLine` is
 * false, and where the page has no canvas to measure words on, which is no browser this page
 * runs in; an entry given none draws its first line uncut.
 *
 * The fonts are read because `styles.css` loads Inter with `font-display: swap`: the list's
 * first frames are drawn in the fallback while Inter's file is on its way, and drawn again in
 * Inter once it arrives, and neither the family the list names nor its width changes then. An
 * entry cut in the first face would keep that cut: where that face is wider it leaves out a
 * word its box has room for, and where it is narrower its line runs past its box. So each
 * `loadingdone` - a face the document was loading has arrived, or failed - makes a new reading
 * whatever the width, and each entry measures its words again in the face that now draws them.
 */
export function useOneLineBox(
  tree: RefObject<HTMLElement | null>,
  oneLine: boolean
): OneLineBox | undefined {
  const [box, setBox] = useState<OneLineBox | undefined>(undefined);
  useLayoutEffect(() => {
    const element = tree.current;
    if (!oneLine || element === null) return;
    const context = document.createElement("canvas").getContext("2d");
    if (context === null) return;
    let width = element.getBoundingClientRect().width;
    // `faces`: the document's fonts changed, so the same family and width draw other widths.
    const read = (faces: boolean) => {
      const family = window.getComputedStyle(element).fontFamily;
      const rem = Number.parseFloat(window.getComputedStyle(document.documentElement).fontSize);
      if (!(width > 0 && rem > 0)) return;
      setBox((held) => {
        if (!faces && held?.width === width && held.rem === rem && held.family === family) {
          return held;
        }
        // Setting a canvas's font parses it, so a run of words in one size sets it once.
        let set = "";
        return {
          family,
          rem,
          width,
          widthOf: (text, size, weight) => {
            const font = `${weight === undefined ? "" : `${weight} `}${size * rem}px ${family}`;
            if (font !== set) {
              context.font = font;
              set = font;
            }
            return context.measureText(text).width;
          },
        };
      });
    };
    read(false);
    const fonts = "fonts" in document ? document.fonts : undefined;
    const loaded = () => read(true);
    fonts?.addEventListener("loadingdone", loaded);
    const observer =
      typeof ResizeObserver === "undefined"
        ? undefined
        : new ResizeObserver((entries) => {
            const entry = entries[entries.length - 1];
            if (entry === undefined) return;
            width = entry.contentRect.width;
            read(false);
          });
    observer?.observe(element);
    return () => {
      fonts?.removeEventListener("loadingdone", loaded);
      observer?.disconnect();
    };
  }, [oneLine, tree]);
  return oneLine ? box : undefined;
}

/** What a one-line entry draws of its node's first line, and where it draws it. */
export interface OneLineCut {
  /** Whether the node's text carries on past `line`, which the entry draws its ellipsis for
   *  where the line ends inside a sentence (`drawnLine`). */
  readonly cut: boolean;
  readonly line: string;
  /** Whether the line is drawn on a line of its own under the row's other words, because
   *  beside them it has less room than its first word takes. */
  readonly under: boolean;
  /** Whether even that line of its own holds less than the first word, so the word wraps
   *  inside itself rather than have its tail hidden. */
  readonly wraps: boolean;
}

/** How far a sum of a row's parts may be from the browser's own layout of them, in pixels: each
 *  box is laid out to a fraction of a pixel the canvas does not round to. */
const ROUNDING = 1;

/**
 * The node's first line as a one-line entry draws it: `firstLineOf`'s line, cut again at a word
 * to the pixels the entry's own box leaves it (R1). The line is the longest run of its own
 * words, measured as it is drawn (`drawnLine`: with its ellipsis where the text carries on past
 * a line that ends inside a sentence), that fits beside the entry's other parts. Where those
 * leave it less than its first word it goes on a line of its own under them, where it has the
 * whole width; a first word no such line holds - a token as long as `firstLineOf` lets one be -
 * is drawn whole, wrapping inside itself, since a box that cut its tail would cut inside a
 * word, which is the one thing R1 bans.
 *
 * `widthOf` is how wide a run of the line's words is drawn; `before` and `after` are the pixels
 * the entry's other parts take on either side of the line, the gaps to it included; `room` is
 * the width the entry's parts share.
 */
export function oneLineCut({
  after,
  before,
  room,
  text,
  widthOf,
}: {
  after: number;
  before: number;
  room: number;
  text: string;
  widthOf: (text: string) => number;
}): OneLineCut {
  const first = firstLineOf(text);
  if (first.line === "") return { ...first, under: false, wraps: false };
  const ends: number[] = [];
  for (let at = first.line.indexOf(" "); at > 0; at = first.line.indexOf(" ", at + 1)) {
    ends.push(at);
  }
  // The longest of the line's own words that fits `budget`. A run of more words is never
  // narrower than a run of fewer, so the runs are searched by halves; the whole line is read on
  // its own first, since it carries no ellipsis where the text ends with it or it is a whole
  // sentence. A run cut at a word inside the line ends inside its sentence, so it always
  // carries one.
  const fitting = (budget: number): { cut: boolean; line: string } | undefined => {
    if (widthOf(drawnLine(first)) <= budget) return first;
    let found: number | undefined;
    let low = 0;
    let high = ends.length - 1;
    while (low <= high) {
      const middle = Math.floor((low + high) / 2);
      const end = ends[middle] ?? 0;
      if (widthOf(`${first.line.slice(0, end)}…`) <= budget) {
        found = end;
        low = middle + 1;
      } else {
        high = middle - 1;
      }
    }
    return found === undefined ? undefined : { cut: true, line: first.line.slice(0, found) };
  };
  const beside = fitting(room - before - after - ROUNDING);
  if (beside !== undefined) return { ...beside, under: false, wraps: false };
  const under = fitting(room - after - ROUNDING);
  if (under !== undefined) return { ...under, under: true, wraps: false };
  const word = ends[0];
  return word === undefined
    ? { ...first, under: true, wraps: true }
    : { cut: true, line: first.line.slice(0, word), under: true, wraps: true };
}
