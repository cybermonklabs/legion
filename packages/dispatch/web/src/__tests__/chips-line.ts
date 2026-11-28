/**
 * The control row's chips line laid out as a browser would lay it out, which happy-dom does not:
 * a case that reads which chips the line holds and which go behind `+N more` lays it out here
 * (`ControlRow.test.tsx`, `focus-hand-off-inventory.test.tsx`).
 */

import { spyOn } from "bun:test";

/** Puts Tailwind's rules for the row in the document for one test. Answers what takes them out. */
export function withCss(css: string): () => void {
  const style = document.createElement("style");
  style.textContent = css;
  document.head.appendChild(style);
  return () => style.remove();
}

/** The width of `+N more` in a line `layOutLine` lays out, whatever its N. */
const MORE_WIDTH = 60;

/**
 * Lays the chips line out as a browser would, which happy-dom does not: the box the chips and
 * `+N more` share is `room.width` px wide, each chip as wide as `widths` says for its words, and
 * `+N more` `MORE_WIDTH` px; the chips stand 8 px apart (`gap-2`). Any other element is a box of
 * no width. A test that changes `room.width` reports it to the line's observers. Answers what
 * puts happy-dom's own layout back.
 */
export function layOutLine(
  room: { width: number },
  widths: Readonly<Record<string, number>>
): () => void {
  const removeCss = withCss(".gap-2{column-gap:8px}");
  const boxes = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    const words = this.textContent ?? "";
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
