import { expect, test } from "bun:test";

import { thePageLeftItThere } from "../doc/readers-place";
import { isInMarginView, scrollMarginTo } from "./useCardHold";

/** The margin's scrollport: a box `MARGIN_HEIGHT` tall standing `MARGIN_TOP` down the page.
 *  happy-dom lays nothing out and clamps no scroll, so the box's rectangle and how far it may
 *  scroll are stated here, and it clamps as a browser does. */
const MARGIN_TOP = 80;
const MARGIN_HEIGHT = 500;
const MARGIN_CONTENT = 3_000;

function marginColumn(): HTMLElement {
  let scrolled = 0;
  const container = document.createElement("div");
  Object.defineProperty(container, "scrollTop", {
    configurable: true,
    get: () => scrolled,
    set: (next: number) => {
      scrolled = Math.max(0, Math.min(next, MARGIN_CONTENT - MARGIN_HEIGHT));
    },
  });
  container.getBoundingClientRect = () =>
    ({
      bottom: MARGIN_TOP + MARGIN_HEIGHT,
      height: MARGIN_HEIGHT,
      top: MARGIN_TOP,
    }) as DOMRect;
  return container;
}

/** A card drawn `below` pixels under the top of the margin's scrollport, as the margin has
 *  already measured it. */
function cardAt(below: number, height: number): DOMRect {
  return {
    bottom: MARGIN_TOP + below + height,
    height,
    top: MARGIN_TOP + below,
  } as DOMRect;
}

test("the margin's own placement of a card says the page made it, and a reader's scroll does not", () => {
  const container = marginColumn();

  // The margin placing the card a comment link names is the page moving what it draws, and it
  // goes through the one call that records it (`putTheBoxAt`, `features/doc/readers-place.ts`):
  // a landing correction watching the same box reads the placement as the page's own and goes
  // on watching, rather than as the reader taking the margin over (`refocus-when-settled.ts`).
  scrollMarginTo(container, cardAt(900, 120), container.getBoundingClientRect());

  expect(container.scrollTop).toBe(900);
  expect(thePageLeftItThere(container)).toBe(true);

  // The reader scrolls the margin themselves: the box stands where no call of the page's left
  // it, and the correction stops.
  container.scrollTop = 1_200;

  expect(thePageLeftItThere(container)).toBe(false);
});

test("a card placed with padding above it starts that far below the top of the margin", () => {
  const container = marginColumn();
  container.scrollTop = 300;

  // The composer's own placement, which starts the element rather than centring it.
  scrollMarginTo(container, cardAt(200, 120), container.getBoundingClientRect(), 40);

  expect(container.scrollTop).toBe(460);
  expect(thePageLeftItThere(container)).toBe(true);
  expect(isInMarginView(cardAt(40, 120), container.getBoundingClientRect())).toBe(true);
});
