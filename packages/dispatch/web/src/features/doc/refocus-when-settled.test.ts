import { afterEach, beforeEach, expect, test } from "bun:test";
import { waitFor } from "@testing-library/react";

import { moveTheScroll } from "./readers-place";
import { refocusWhenSettled } from "./refocus-when-settled";

/** The observers the rows below fire by hand: happy-dom lays nothing out and watches no size, so
 *  a drawn root grows only when its test says it did. */
let observers: ResizeObserverCallback[] = [];
let originalObserver: typeof ResizeObserver;
let restoreScroll: (() => void) | undefined;

beforeEach(() => {
  observers = [];
  originalObserver = globalThis.ResizeObserver;
  globalThis.ResizeObserver = class {
    constructor(callback: ResizeObserverCallback) {
      observers.push(callback);
    }
    observe() {}
    disconnect() {}
    unobserve() {}
  } as unknown as typeof ResizeObserver;
});

afterEach(() => {
  globalThis.ResizeObserver = originalObserver;
  restoreScroll?.();
  restoreScroll = undefined;
  // The selection is the page's, not a row's: one left standing reaches every later row in
  // the same happy-dom, where a click on a reference while words are selected navigates
  // nowhere (`useReferenceClick`, `document-references.tsx`).
  document.getSelection()?.removeAllRanges();
});

/** A drawn document root of a stated height, with the growth a late card gives it. */
function drawnRoot(): { element: HTMLElement; grow(by: number): void } {
  let height = 100;
  const element = document.createElement("div");
  element.getBoundingClientRect = () => ({ height }) as DOMRect;
  return {
    element,
    grow(by: number) {
      height += by;
      for (const observer of observers) {
        observer([], undefined as unknown as ResizeObserver);
      }
    },
  };
}

/** Longer than the quiet the re-run waits out, so a row proving no re-run came waits past the
 *  moment one would have; a row proving one came waits for the run itself. */
const AFTER_THE_QUIET = 300;

/** Past the window in which the movement a correction started is still the correction's own
 *  (`CORRECTION_SCROLL_MS`), so a row that scrolls after it scrolls as somebody else. A real
 *  wait, as the quiet above is: the window is a `setTimeout` the watch arms itself, with
 *  nothing of it exposed to wait on, and a fake clock would have to drive the animation frame
 *  the correction is made in too. */
const AFTER_THE_CORRECTION = 400;

/** What a browser does with a scroll: it tells the listeners after the write, in its own
 *  later turn rather than inside the line that moved the box, which is what lets the page
 *  record where it left that box before anybody reads the event (`moveTheScroll`). */
function toldTheListeners(): Promise<void> {
  return Bun.sleep(0).then(() => {
    window.dispatchEvent(new Event("scroll"));
  });
}

/** The page's own scroll, which is what carries a document that no column of its own carries:
 *  happy-dom lays nothing out, so `whatScrolls` finds no scrolling ancestor and the watch reads
 *  `window.scrollY`, as it does on a document page. `window.scrollBy` moves it too, so a row
 *  can call the page's own place-keeping (`moveTheScroll`) and have it land here, as it does in
 *  a browser. */
function pageScroll(): { to(top: number): Promise<void> } {
  let top = 0;
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const scrollBy = window.scrollBy;
  Object.defineProperty(window, "scrollY", { configurable: true, get: () => top });
  window.scrollBy = ((_x: number, y: number) => {
    top += y;
    void toldTheListeners();
  }) as never;
  restoreScroll = () => {
    if (scrollY !== undefined) Object.defineProperty(window, "scrollY", scrollY);
    window.scrollBy = scrollBy;
  };
  return {
    to(next: number) {
      top = next;
      return toldTheListeners();
    },
  };
}

test("a document that grows under the target focuses it again, once", async () => {
  const drawn = drawnRoot();
  let focused = 0;
  const stop = refocusWhenSettled(() => (focused += 1), drawn.element);

  try {
    drawn.grow(300);
    drawn.grow(500);
    await waitFor(() => expect(focused).toBe(1));
    await Bun.sleep(AFTER_THE_QUIET);
    expect(focused).toBe(1);
  } finally {
    stop();
  }
});

test("a document that keeps its height leaves the target where the focus put it", async () => {
  const drawn = drawnRoot();
  let focused = 0;
  const stop = refocusWhenSettled(() => (focused += 1), drawn.element);

  try {
    // A resize that reports the same height - the observer's own first call - moves nothing.
    drawn.grow(0);
    await Bun.sleep(AFTER_THE_QUIET);
    expect(focused).toBe(0);
  } finally {
    stop();
  }
});

test("a reader who moved on after the document grew is not pulled back", async () => {
  const drawn = drawnRoot();
  let focused = 0;
  const stop = refocusWhenSettled(() => (focused += 1), drawn.element);

  try {
    drawn.grow(300);
    window.dispatchEvent(new Event("wheel"));
    await Bun.sleep(AFTER_THE_QUIET);
    expect(focused).toBe(0);
  } finally {
    stop();
  }
});

test("a dropped re-run never runs, however the document grows after it", async () => {
  const drawn = drawnRoot();
  let focused = 0;
  refocusWhenSettled(() => (focused += 1), drawn.element)();

  drawn.grow(300);
  await Bun.sleep(AFTER_THE_QUIET);
  expect(focused).toBe(0);
});

test("a caller with no drawn root and no diagram waiting arms nothing", async () => {
  let focused = 0;
  const stop = refocusWhenSettled(() => (focused += 1));

  try {
    expect(observers).toHaveLength(0);
    await Bun.sleep(AFTER_THE_QUIET);
    expect(focused).toBe(0);
  } finally {
    stop();
  }
});

test("a scroll the watch did not make ends it", async () => {
  const page = pageScroll();
  const drawn = drawnRoot();
  let focused = 0;
  const stop = refocusWhenSettled(() => (focused += 1), drawn.element);

  try {
    // None of this is a press, a wheel, a touch or a key: a scrollbar drag, a find-in-page
    // jump, an assistive tool, a scroll made over the automation protocol. The page is
    // somewhere else all the same, so where the landing put the reader is not where they are.
    await Bun.sleep(AFTER_THE_CORRECTION);
    await page.to(900);
    drawn.grow(300);
    await Bun.sleep(AFTER_THE_QUIET);
    expect(focused).toBe(0);
  } finally {
    stop();
  }
});

test("the frames of a scroll the watch did make leave it watching", async () => {
  const page = pageScroll();
  const drawn = drawnRoot();
  let focused = 0;
  const stop = refocusWhenSettled(() => (focused += 1), drawn.element);

  try {
    // A browser animating the landing's own scroll reports a position every frame on the way
    // to it, and none of those is where the scroll left the page. Reading the first of them as
    // the reader's would end the watch before it had corrected anything.
    for (const top of [120, 480, 900]) await page.to(top);
    drawn.grow(300);
    await waitFor(() => expect(focused).toBe(1));
  } finally {
    stop();
  }
});

test("a reader selecting words in the document ends it", async () => {
  const drawn = drawnRoot();
  drawn.element.append(document.createTextNode("Paragraph 2: surrounding context"));
  document.body.append(drawn.element);
  let focused = 0;
  const stop = refocusWhenSettled(() => (focused += 1), drawn.element);

  try {
    // Dragging over words presses first, but a selection extended from the keyboard after the
    // focus, a find-in-page match and an assistive tool's selection press nothing at all, and
    // a reader who has selected words is working in the page rather than waiting for a link.
    document.getSelection()?.selectAllChildren(drawn.element);
    drawn.grow(300);
    await Bun.sleep(AFTER_THE_QUIET);
    expect(focused).toBe(0);
  } finally {
    stop();
    drawn.element.remove();
  }
});

test("a selection made outside the document leaves the target where the link put it", async () => {
  const drawn = drawnRoot();
  document.body.append(drawn.element);
  const elsewhere = document.createElement("p");
  elsewhere.append(document.createTextNode("A card in the margin"));
  document.body.append(elsewhere);
  let focused = 0;
  const stop = refocusWhenSettled(() => (focused += 1), drawn.element);

  try {
    // The margin's own cards and composer are selected in the same page, and nothing they do
    // says the reader has left the target a link put them on.
    document.getSelection()?.selectAllChildren(elsewhere);
    drawn.grow(300);
    await waitFor(() => expect(focused).toBe(1));
  } finally {
    stop();
    drawn.element.remove();
    elsewhere.remove();
  }
});

test("a scroll the page's own place-keeping made leaves it watching", async () => {
  pageScroll();
  const drawn = drawnRoot();
  let focused = 0;
  const stop = refocusWhenSettled(() => (focused += 1), drawn.element);

  try {
    // The page holding the reader still while its chrome lands, long after the landing's own
    // scroll has settled (`keepWhatStandsUnder` and `keepTheReadersPlace`, which both scroll
    // through `moveTheScroll`). That is the page moving what it draws, not the reader moving
    // themselves, and the growth it answers moves this watch's target too.
    await Bun.sleep(AFTER_THE_CORRECTION);
    moveTheScroll(drawn.element, 900);
    // The browser tells the listeners after the write, so the event reaches the watch with no
    // correction of its own pending: the record `moveTheScroll` left is all that saves it.
    await Bun.sleep(0);
    drawn.grow(300);
    await waitFor(() => expect(focused).toBe(1));
  } finally {
    stop();
  }
});
