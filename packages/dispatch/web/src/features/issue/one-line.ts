import { type RefObject, useLayoutEffect, useState } from "react";

import { firstLineOf } from "./issue-tree";

/**
 * What the zoomed tree reads once for every row it draws on one line (dispatch://INFRA-1196 R1,
 * R17): how wide the tree is, and what a row needs to say how wide its own words are drawn.
 *
 * A one-line row cuts its node's first line at a word to the room its own box leaves it, and no
 * row may read its own layout to learn that room: that is a layout read per row, and again on
 * every resize, for a page that draws 56 of them. So the tree reads one width - its own, which
 * is every row's before the row's indent - with one `ResizeObserver` (`useOneLineBox`), and each
 * row sums its own parts against it: the parts its classes size in rem, and its words, measured
 * on a canvas in the face the tree sets them in, which lays out nothing on the page.
 */
export interface OneLineBox {
  /** The face the tree's words are set in, as the tree's computed `font-family`. */
  readonly family: string;
  /** How many CSS pixels one `rem` is, which every part of a row its classes size is in. */
  readonly rem: number;
  /** The tree's own width, in CSS pixels. */
  readonly width: number;
  /** How wide `text` is drawn at `size` rem in the tree's face, in CSS pixels. */
  readonly widthOf: (text: string, size: number) => number;
}

/**
 * The tree's one reading of its own width, for its one-line rows: taken as the tree is first
 * laid out, before it is painted, and again whenever the tree's box changes size, by one
 * `ResizeObserver` on the tree and none on any row. A reading that says what it said before
 * leaves the rows alone. Undefined while the rows are not one line, and where the page has no
 * canvas to measure words on, which is no browser this page runs in; a row given none draws
 * its first line uncut.
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
    const read = (width: number) => {
      const family = window.getComputedStyle(element).fontFamily;
      const rem = Number.parseFloat(window.getComputedStyle(document.documentElement).fontSize);
      if (!(width > 0 && rem > 0)) return;
      setBox((held) => {
        if (held?.width === width && held.rem === rem && held.family === family) return held;
        // Setting a canvas's font parses it, so a run of words in one size sets it once.
        let set = "";
        return {
          family,
          rem,
          width,
          widthOf: (text, size) => {
            const font = `${size * rem}px ${family}`;
            if (font !== set) {
              context.font = font;
              set = font;
            }
            return context.measureText(text).width;
          },
        };
      });
    };
    read(element.getBoundingClientRect().width);
    if (typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver((entries) => {
      const entry = entries[entries.length - 1];
      if (entry !== undefined) read(entry.contentRect.width);
    });
    observer.observe(element);
    return () => observer.disconnect();
  }, [oneLine, tree]);
  return oneLine ? box : undefined;
}

/** What a one-line row draws of its node's first line, and where it draws it. */
export interface OneLineCut {
  /** Whether the node's text carries on past `line`, which the row draws its ellipsis for. */
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
 * The node's first line as a one-line row draws it: `firstLineOf`'s line, cut again at a word to
 * the pixels the row's own box leaves it (R1). The line is the longest run of its own words,
 * the ellipsis included where the text carries on, that fits beside the row's other words. Where
 * those words leave it less than its first word - a long author's id on a phone - it goes on a
 * line of its own under them, where it has the row's whole width but the state's; a row that
 * kept it beside them would draw it 0 px wide and hide the words beside it past its edge. A
 * first word no such line holds - a token as long as `firstLineOf` lets one be - is drawn
 * whole, wrapping inside itself, since a box that cut its tail would cut inside a word, which
 * is the one thing R1 bans.
 *
 * `widthOf` is how wide a run of the line's words is drawn; `before` and `after` are the pixels
 * the row's other parts take on either side of the line, the gaps to it included; `room` is the
 * width the row's parts share.
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
  // its own first, since it carries no ellipsis where the text ends with it.
  const fitting = (budget: number): { cut: boolean; line: string } | undefined => {
    if (widthOf(first.cut ? `${first.line}…` : first.line) <= budget) return first;
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
