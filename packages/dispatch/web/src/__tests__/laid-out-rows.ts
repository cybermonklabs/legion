/**
 * Rows laid down the window, and the moves a page makes over them: the focus it asks for and the
 * scroll it asks for. A case of the selection bar's hand-off where the row the reader marked last
 * cannot take the focus (`useLastMarkedPlace`) - a press took that row away, or no row is marked -
 * reads what the reader can see and whether anything scrolled. happy-dom lays nothing out and
 * scrolls nothing, so a case says here where each row stands and reads each move here.
 */

import { spyOn } from "bun:test";

/** How tall each row `layRowsOut` lays down is drawn. */
const ROW_PX = 100;

/** Lays each row `rowSelector` names, and the checkbox in it, down the window, `ROW_PX` a row,
 *  with the row at `first` in the page's order at the window's top, as a reader deep in a list
 *  sees it. A row and its checkbox are drawn where the row stands now, so the checkbox of a row
 *  that moves up into a place that went is drawn there. The selection bar's hand-off reads a
 *  row's own box for the space it takes in the list, where a press is still taking it away. Every
 *  other element keeps happy-dom's own box, in which every element reads as in sight. Answers the
 *  function that gives happy-dom its own boxes back. */
export function layRowsOut(rowSelector: string, first: number): () => void {
  const real = Element.prototype.getBoundingClientRect;
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    const row = this.matches(`input[type="checkbox"], ${rowSelector}`)
      ? this.closest(rowSelector)
      : null;
    if (row === null) return real.call(this);
    const index = [...document.querySelectorAll(rowSelector)].indexOf(row);
    return new DOMRect(0, (index - first) * ROW_PX, 200, ROW_PX);
  });
  return () => rect.mockRestore();
}

/** Draws `chrome` over the window's top `px`, as a list's sticky head is drawn over the rows that
 *  scroll under it, for `document.elementFromPoint`, which happy-dom answers with no element. A
 *  point above `px` names `chrome`; when `below` is given, a point at or past `below.at` names
 *  `below.element` (so a hit-test outside a clipping box's bottom edge fails to hit a row). Any
 *  other point names the checkbox that `layRowsOut`, given the same `rowSelector` and `first`,
 *  lays there, else no element. Answers the function that gives happy-dom its own answer back. */
export function drawOver(
  chrome: Element,
  px: number,
  rowSelector: string,
  first: number,
  below?: { readonly at: number; readonly element: Element }
): () => void {
  const point = spyOn(document, "elementFromPoint").mockImplementation((_x, y) => {
    if (y < px) return chrome;
    if (below !== undefined && y >= below.at) return below.element;
    const row = document.querySelectorAll(rowSelector)[first + Math.floor(y / ROW_PX)];
    return row?.querySelector('input[type="checkbox"]') ?? null;
  });
  return () => point.mockRestore();
}

/** One focus the page asked for: the element, and the options it asked with. */
export interface FocusMove {
  readonly options: FocusOptions | undefined;
  readonly target: HTMLElement;
}

/** Every focus the page asks for from now on, with the options it asks with, and every element
 *  it scrolls into view, until `restore`. A focus is made as asked; a scroll is only noted. */
export function watchMoves(): {
  focused: FocusMove[];
  restore: () => void;
  scrolled: Element[];
} {
  const realFocus = HTMLElement.prototype.focus;
  const focused: FocusMove[] = [];
  const scrolled: Element[] = [];
  const focus = spyOn(HTMLElement.prototype, "focus").mockImplementation(function (
    this: HTMLElement,
    options?: FocusOptions
  ) {
    focused.push({ options, target: this });
    realFocus.call(this, options);
  });
  const scroll = spyOn(Element.prototype, "scrollIntoView").mockImplementation(function (
    this: Element
  ) {
    scrolled.push(this);
  });
  return {
    focused,
    restore: () => {
      focus.mockRestore();
      scroll.mockRestore();
    },
    scrolled,
  };
}
