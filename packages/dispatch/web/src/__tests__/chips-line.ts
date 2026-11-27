/**
 * The control bar laid out as a browser would lay it out, which happy-dom does not: a case that
 * reads which chips the line holds and which go behind `+N more`, or which layout the bar's own
 * width gives, lays it out here (`ControlRow.test.tsx`, `focus-hand-off-inventory.test.tsx`).
 * And the bar itself, as a page's case finds it (`controlBar`).
 */

import { spyOn } from "bun:test";

import { CONTROL, controlIn } from "../components/control-names";

/** The control bar `root` holds (`ControlRow`); a failure names `what`, the page that draws no
 *  bar there. */
export function controlBar(what: string, root: ParentNode = document): HTMLElement {
  const found = controlIn(root, CONTROL.controlRow);
  if (found === null) throw new Error(`${what} draws no control bar`);
  return found;
}

/** Puts Tailwind's rules for the bar in the document for one test. Answers what takes them out. */
export function withCss(css: string): () => void {
  const style = document.createElement("style");
  style.textContent = css;
  document.head.appendChild(style);
  return () => style.remove();
}

/** A chip's words as a reader reads them: its text, without the × of its remove control or the ↗
 *  of a link beside it. */
export function wordsOf(element: Element): string {
  return (element.textContent ?? "").replace(/[×↗]/g, "");
}

/** The width of `+N more` in a line `layOutLine` lays out, whatever its N. */
const MORE_WIDTH = 60;

/**
 * Lays the bar out as a browser would, which happy-dom does not: the bar itself is `bar.width` px
 * wide - 0 where unset, a bar laid out nowhere, which takes the wide layout - the box the chips
 * and `+N more` share is `room.width` px wide, each chip as wide as `widths` says for its words
 * (`wordsOf`), and `+N more` `MORE_WIDTH` px; the chips stand 8 px apart (`gap-2`). Any other
 * element is a box of no width, 44 px tall. A test that changes `room.width` or `bar.width`
 * reports it to the observers (`installResizeObserver`), as a browser reports a box resized: the
 * bar reads its own width from its own box, never the window's, which stays happy-dom's 1024 px.
 * Answers what puts happy-dom's own layout back.
 */
export function layOutLine(
  room: { readonly width: number },
  widths: Readonly<Record<string, number>>,
  bar: { readonly width: number } = { width: 0 }
): () => void {
  const removeCss = withCss(".gap-2{column-gap:8px}");
  const boxes = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    if (this.getAttribute("data-control") === CONTROL.controlRow) {
      return new DOMRect(0, 0, bar.width, 44);
    }
    const words = wordsOf(this);
    let width = widths[words] ?? 0;
    if (this.firstElementChild?.hasAttribute("data-chip-strip") === true) width = room.width;
    else if (/^\+\d+ more$/.test(words)) width = MORE_WIDTH;
    return new DOMRect(0, 0, width, 44);
  });
  return () => {
    boxes.mockRestore();
    removeCss();
  };
}

/** Lays the bar out `bar.width` px wide, its chips' box with no room (`layOutLine`). */
export function layOutBar(bar: { readonly width: number }): () => void {
  return layOutLine({ width: 0 }, {}, bar);
}
