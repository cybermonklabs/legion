import { afterAll, afterEach, beforeAll, beforeEach, expect, test } from "bun:test";
import { waitFor } from "@testing-library/react";

import {
  bindTheIssuePagesKeys,
  eventOf,
  type ReadersStep,
  THE_READERS_CASES,
} from "../../__tests__/readers-place-harness";
import { installResizeObserver, type ResizeControl } from "../../__tests__/scroll-box";
import { appKeymap } from "../shell/keymap";
// This watch's own windows are read on the real clock: the correction it makes is made in an
// animation frame, and the window in which the movement that correction started is still its own
// is a `setTimeout` it arms itself, with nothing of either exposed for a row to drive
// (`CORRECTION_SCROLL_MS`, `refocus-when-settled.ts`). So the rows below wait those windows out
// rather than moving a fake clock across them.
import { moveTheScroll, placeInView, putTheBoxAt, putTheReaderBack } from "./readers-place";
import { theReaderIsScrolling, watchTheReadersScroll } from "./readers-scroll";
import { refocusWhenSettled } from "./refocus-when-settled";
import { scrollElementToTop } from "./scroll-block";

/** The observers the rows below fire by hand: happy-dom lays nothing out and watches no size, so
 *  a drawn root grows only when its test says it did. */
let sizes: ResizeControl;
let restoreScroll: (() => void) | undefined;

beforeEach(() => {
  sizes = installResizeObserver();
});

afterEach(() => {
  sizes.restore();
  restoreScroll?.();
  restoreScroll = undefined;
  // The selection is the page's, not a row's: one left standing reaches every later row in
  // the same happy-dom, where a click on a reference while words are selected navigates
  // nowhere (`useReferenceClick`, `document-references.tsx`).
  document.getSelection()?.removeAllRanges();
});

/** A drawn document root of a stated height standing at a stated place in the page, with the
 *  growth a late card gives it and the growth a late piece of chrome gives what stands above
 *  it. Its rectangle is a browser's: where it stands on the reader's screen is where it
 *  stands in the page less where the page is scrolled to. */
function drawnRoot(): {
  element: HTMLElement;
  grow(by: number): void;
  growAbove(by: number): void;
} {
  let height = 100;
  let inThePage = 0;
  const element = document.createElement("div");
  element.getBoundingClientRect = () => ({ height, top: inThePage - window.scrollY }) as DOMRect;
  return {
    element,
    grow(by: number) {
      height += by;
      sizes.resize();
    },
    /** What stands above the document grows, which pushes the document down the page and
     *  changes the size of nothing the watch is observing. */
    growAbove(by: number) {
      inThePage += by;
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

/** The page's own scroll, as a row that moves it holds it. */
interface ThePage {
  /** Scrolls the page to `top`, and tells the listeners in a later turn, as a browser does. */
  to(top: number): Promise<void>;
}

/** The page's own scroll, which is what carries a document that no column of its own carries:
 *  happy-dom lays nothing out, so `whatScrolls` finds no scrolling ancestor and the watch reads
 *  `window.scrollY`, as it does on a document page. `window.scrollBy` moves it too, so a row
 *  can call the page's own place-keeping (`moveTheScroll`) and have it land here, as it does in
 *  a browser. */
function pageScroll(): ThePage {
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

/** The Inbox's reading column: the box that scrolls the document drawn in it, and the heading
 *  of the section the column was opened at, standing `HEADING_BELOW_PX` down that document.
 *  happy-dom lays nothing out and scrolls nothing, so what `whatScrolls` reads to find the box
 *  - its overflow, and its content standing taller than it does - is stated here, as are the
 *  rects the placement measures its move against. Writing `scrollTop` tells the listeners as a
 *  browser does, in a later turn than the line that moved the box. */
function readingColumn(): {
  column: HTMLElement;
  drawn: { element: HTMLElement; grow(by: number): void };
  heading: HTMLElement;
} {
  let scrolled = 0;
  let height = 100;
  const column = document.createElement("div");
  column.style.overflowY = "auto";
  Object.defineProperty(column, "scrollHeight", { configurable: true, get: () => 4_000 });
  Object.defineProperty(column, "clientHeight", { configurable: true, get: () => 400 });
  Object.defineProperty(column, "scrollTop", {
    configurable: true,
    get: () => scrolled,
    set: (next: number) => {
      scrolled = next;
      void toldTheListeners();
    },
  });
  column.getBoundingClientRect = () => ({ height: 400, top: 0 }) as DOMRect;
  const element = document.createElement("div");
  // Where the document and its heading stand on the reader's screen: where they are drawn in
  // the column, less how far down the column is scrolled.
  element.getBoundingClientRect = () => ({ height, top: -scrolled }) as DOMRect;
  const heading = document.createElement("section");
  heading.getBoundingClientRect = () => ({ top: HEADING_BELOW_PX - scrolled }) as DOMRect;
  element.append(heading);
  column.append(element);
  document.body.append(column);
  return {
    column,
    drawn: {
      element,
      grow(by: number) {
        height += by;
        sizes.resize();
      },
    },
    heading,
  };
}

/** How far down the column the section's heading is drawn, which is how far the placement
 *  scrolls the column to put it at the top. */
const HEADING_BELOW_PX = 300;

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
    expect(sizes.live()).toBe(0);
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

test("a scroll answering a growth above the document leaves it watching", async () => {
  const page = pageScroll();
  const drawn = drawnRoot();
  let focused = 0;
  const stop = refocusWhenSettled(() => (focused += 1), drawn.element);

  try {
    // A version's own heading and the page's chrome land above the document while the diagrams
    // below are still drawing, and the browser's own scroll anchoring answers that growth by
    // scrolling the page by exactly what grew. Nothing of it reaches `moveTheScroll` - no
    // script scrolled at all - and the document is standing on the line of the screen it was
    // already on, which is the page moving what it draws rather than the reader moving
    // themselves (`e2e/mermaid.e2e.ts`, "a past version's highlighted mark lands on its block
    // once a diagram above it draws", where this stop left the mark 10,000 px below the fold).
    await Bun.sleep(AFTER_THE_CORRECTION);
    drawn.growAbove(21);
    await page.to(21);
    drawn.grow(300);
    await waitFor(() => expect(focused).toBe(1));
  } finally {
    stop();
  }
});

// A link's landing corrects through the editor's own focus scroll (`focusBlock` and `focusMark`,
// `@legion/proof-editor`), which no call of `readers-place.ts` makes. A press held across it - a
// click outside the document that takes no focus, a finger resting on a phone - read that scroll
// as the press's first movement: the landing ended at its first correction, and the keepers'
// window opened on the same scroll and closed having seen one, taking whatever grew meanwhile as
// the reader's place (the review's harness: two growths ran the landing once, and the keepers'
// watch reported a window closed `scrolled: true`).
test("a press held across a landing's correction leaves the landing correcting and opens no keepers' window", async () => {
  const page = pageScroll();
  const drawn = drawnRoot();
  document.body.append(drawn.element);
  // The keepers' watch of the reader's own scroll, told each time one of its windows closes.
  const windows: boolean[] = [];
  const releaseTheKeepers = watchTheReadersScroll((scrolled) => {
    windows.push(scrolled);
  });
  let landed = 0;
  // The editor's own focus scroll: the page brought to the target, which each growth pushes
  // 300 px further down, and the listeners told in a later turn, as a browser tells them.
  const stop = refocusWhenSettled(() => {
    landed += 1;
    void page.to(1_000 + 300 * landed);
  }, drawn.element);
  try {
    await Bun.sleep(AFTER_THE_CORRECTION);
    // A mouse button held down on the page: a click on nothing that takes the focus.
    document.body.dispatchEvent(eventOf({ pointerType: "mouse", type: "pointerdown" }));
    drawn.grow(300);
    await Bun.sleep(AFTER_THE_QUIET);
    drawn.grow(300);
    await Bun.sleep(AFTER_THE_QUIET);

    expect({ landed, scrolling: theReaderIsScrolling(), windows }).toEqual({
      landed: 2,
      scrolling: false,
      windows: [],
    });
  } finally {
    document.body.dispatchEvent(new Event("pointerup", { bubbles: true }));
    stop();
    releaseTheKeepers();
    drawn.element.remove();
  }
});

// The same press held across the browser's own scroll anchoring, which answers a growth above
// the document by scrolling the page by exactly what grew: the document stands on the line of
// the screen it stood on, which this watch reads as the page moving what it draws. Read as the
// press's movement first, it ended the landing before that reading was made.
test("a press held across a scroll answering a growth above the document leaves the landing correcting", async () => {
  const page = pageScroll();
  const drawn = drawnRoot();
  let landed = 0;
  const stop = refocusWhenSettled(() => (landed += 1), drawn.element);
  try {
    await Bun.sleep(AFTER_THE_CORRECTION);
    document.body.dispatchEvent(eventOf({ pointerType: "mouse", type: "pointerdown" }));
    drawn.growAbove(64);
    await page.to(64);
    drawn.grow(300);
    await Bun.sleep(AFTER_THE_QUIET);

    expect(landed).toBe(1);
  } finally {
    document.body.dispatchEvent(new Event("pointerup", { bubbles: true }));
    stop();
  }
});

test("the reading column's own placement leaves the watch watching", async () => {
  const { column, drawn, heading } = readingColumn();
  let focused = 0;
  const stop = refocusWhenSettled(() => (focused += 1), drawn.element);

  try {
    // Past the window in which the movement the arming scroll started is still its own, so the
    // placement's scroll below is read on its own account.
    await Bun.sleep(AFTER_THE_CORRECTION);
    // The Inbox's reading column puts the section it was opened at back at the top of the
    // column on every drawing pass, while the document under it is still being drawn
    // (`features/inbox/pane-placement.ts`). That is the page placing what it draws, as much as
    // a keeper's correction is, and the growth those passes answer moves this watch's target
    // too - so a landing correction armed in the same column goes on watching. The placement
    // scrolls through the one call that records where it left the box (`moveTheBox`).
    scrollElementToTop(heading, column);
    expect(column.scrollTop).toBe(HEADING_BELOW_PX);
    // The browser tells the listeners after the write, so the event reaches the watch with no
    // correction of its own pending: the record the placement left is all that saves it.
    await Bun.sleep(0);
    drawn.grow(300);
    await waitFor(() => expect(focused).toBe(1));
  } finally {
    stop();
    column.remove();
  }
});

test("a block the page placed in view leaves the watch watching", async () => {
  const { column, drawn, heading } = readingColumn();
  let focused = 0;
  const stop = refocusWhenSettled(() => (focused += 1), drawn.element);

  try {
    await Bun.sleep(AFTER_THE_CORRECTION);
    // Every page that brought an element into view by the browser's own `scrollIntoView` now
    // goes through the wrapper that measures that move and writes it through the recorded call
    // (`placeInView`, `readers-place.ts`) - a thread opened at its first unread message, a
    // highlighted row the arrows reached, the card a comment link names. The browser's call
    // tells nobody, and this watch would read it as the reader taking the column over.
    placeInView(heading, { block: "start" });
    expect(column.scrollTop).toBe(HEADING_BELOW_PX);
    await Bun.sleep(0);
    drawn.grow(300);
    await waitFor(() => expect(focused).toBe(1));
  } finally {
    stop();
    column.remove();
  }
});

test("a box the page put at a place of its own leaves the watch watching", async () => {
  const { column, drawn } = readingColumn();
  let focused = 0;
  const stop = refocusWhenSettled(() => (focused += 1), drawn.element);

  try {
    await Bun.sleep(AFTER_THE_CORRECTION);
    // The caller that knows where the box is to stand rather than how far to move it: a list
    // put back at its own start, a transcript written to its end, a panel put back where the
    // reader left it (`putTheBoxAt`, `readers-place.ts`).
    putTheBoxAt(column, HEADING_BELOW_PX);
    expect(column.scrollTop).toBe(HEADING_BELOW_PX);
    await Bun.sleep(0);
    drawn.grow(300);
    await waitFor(() => expect(focused).toBe(1));
  } finally {
    stop();
    column.remove();
  }
});

test("a reader scrolling the reading column ends the watch", async () => {
  const { column, drawn } = readingColumn();
  let focused = 0;
  const stop = refocusWhenSettled(() => (focused += 1), drawn.element);

  try {
    await Bun.sleep(AFTER_THE_CORRECTION);
    // A scrollbar drag in the column, a find-in-page jump, an assistive tool: none of them
    // presses, wheels, touches or types, and none of them is the placement, which is the only
    // thing that writes this box and says so. The reader is somewhere else, and where the
    // landing put them is not where they are.
    column.scrollTop = HEADING_BELOW_PX;
    await Bun.sleep(0);
    drawn.grow(300);
    await Bun.sleep(AFTER_THE_QUIET);
    expect(focused).toBe(0);
  } finally {
    stop();
    column.remove();
  }
});

/** The two times the page puts a reader back while it is still moving under them, and a reader
 *  may be pressing keys through either: the editor being built over the draw, whose blocks
 *  paint at their own height after the put-back, and a panel landing above a reader deep in the
 *  document, which the page's own keeper answers. */
type Situation = "a panel's arrival" | "the editor's build";

/** The reader presses that key on the page, which hears it as the browser hands it on: the
 *  watches on the capture phase first, then the registry's own listener, which takes a
 *  shortcut's key, runs its binding and moves a chord on (`KeymapProvider`). */
function pressOnThePage(key: string): void {
  const event = eventOf({ key, onThePage: true, type: "keydown" }) as KeyboardEvent;
  document.body.dispatchEvent(event);
  appKeymap.handleKeyDown(event);
}

/** How far the reader's drag on the scroll bar carries the page. */
const A_DRAG_PX = 333;

/**
 * The reader does `input` on the page, which hears each event as a browser hands it on: a key at
 * the box it is pressed into - put on the page for as long as the press takes, where it is not
 * on it already - and then at the registry's own listener where it is pressed on the page, a
 * press at the page, and any other event at the window. `editor` is the drawn root itself, which
 * takes what is typed into it.
 *
 * A scroll while a press is held is a drag on the scroll bar of the box carrying the document,
 * and moves that box: this watch reads a scroll by where it left the box before it asks the
 * table (`scrolled()`, `refocus-when-settled.ts`), and a scroll that moved nothing is nobody's
 * drag. The editor's build drags it inside the window in which the movement the put-back's own
 * scroll started is still that scroll's (`CORRECTION_SCROLL_MS`), which does not excuse a drag.
 */
async function deliver(
  input: readonly ReadersStep[],
  editor: HTMLElement,
  page: ThePage
): Promise<void> {
  for (const step of input) {
    if (step.type === "scroll") {
      await page.to(window.scrollY + A_DRAG_PX);
    } else if (step.type === "pointerdown") {
      document.body.dispatchEvent(eventOf(step));
    } else if (step.type !== "keydown" || step.key === undefined) {
      window.dispatchEvent(eventOf(step));
    } else {
      const into = step.into?.(editor) ?? document.body;
      const placed = !into.isConnected;
      if (placed) document.body.append(into);
      const event = eventOf(step) as KeyboardEvent;
      try {
        into.dispatchEvent(event);
      } finally {
        if (placed) into.remove();
      }
      if (step.onThePage === true) appKeymap.handleKeyDown(event);
    }
  }
}

// The page's own shortcuts, as the issue page registers them, in the application's registry:
// the one the table reads (`bindTheIssuePagesKeys`, the harness).
let unregister: (() => void) | undefined;
beforeAll(() => {
  unregister = bindTheIssuePagesKeys();
});
afterAll(() => unregister?.());

/**
 * Whether the watch still puts the reader back after `act` in `situation`: whether a growth of
 * the drawn document after it is answered by the put-back running again. `act` is given the
 * editor, which is the drawn root itself and takes what is typed into it, and the page's scroll.
 */
async function stillPutsThemBack(
  situation: Situation,
  act: (editor: HTMLElement, page: ThePage) => Promise<void>
): Promise<boolean> {
  const page = pageScroll();
  const drawn = drawnRoot();
  drawn.element.contentEditable = "true";
  document.body.append(drawn.element);
  let focused = 0;
  const stop = refocusWhenSettled(() => (focused += 1), drawn.element);
  try {
    if (situation === "a panel's arrival") {
      // Long after the put-back's own scroll: a panel lands above the document as the reader
      // acts, and the page's keeper answers it through the one call that says who scrolled
      // (`moveTheScroll`), which the browser reports in a later turn.
      await Bun.sleep(AFTER_THE_CORRECTION);
      await act(drawn.element, page);
      drawn.growAbove(88);
      moveTheScroll(drawn.element, 88);
      await Bun.sleep(0);
    } else {
      await act(drawn.element, page);
    }
    drawn.grow(300);
    const until = Date.now() + AFTER_THE_QUIET;
    while (focused === 0 && Date.now() < until) await Bun.sleep(10);
    return focused > 0;
  } finally {
    stop();
    drawn.element.remove();
    restoreScroll?.();
    restoreScroll = undefined;
  }
}

/** What the watch does in each situation after the two acts every key is read against: nothing
 *  at all, and a turn of the wheel. Read once for each and kept, since each reading waits the
 *  watch's own windows out. */
const references = new Map<string, Promise<boolean>>();
function reference(situation: Situation, act: "a wheel" | "nothing"): Promise<boolean> {
  const name = `${act} in ${situation}`;
  let read = references.get(name);
  if (read === undefined) {
    read = stillPutsThemBack(situation, async () => {
      if (act === "a wheel") window.dispatchEvent(new Event("wheel"));
    });
    references.set(name, read);
  }
  return read;
}

// A reader whose input moves nothing is still where the page put them, so the put-back goes on;
// one whose input scrolls or moves the focus has the page. Pressed through the editor's build
// with every key ending the watch, Shift left a reader 924.6 px from their place in 5 runs of 8
// (`e2e/large-documents-build.e2e.ts`, "the editor opens the document where its reader had got
// to").
//
// Each row reads its input against what this same watch does after nothing and after a wheel,
// rather than against a count of re-runs: what a landing does once the reader has or has not
// moved on is the watch's own rule, held by the rows above, and a change to it is not a change
// to which inputs move a reader. The first row of each situation holds those two readings apart,
// because a watch that did the same after both would leave the rows under it reading nothing:
// should the landing's own rule ever change that way, that row is red and says so.
for (const situation of ["the editor's build", "a panel's arrival"] as const) {
  test(`a wheel and no act at all leave the put-back differently during ${situation}`, async () => {
    const untouched = await reference(situation, "nothing");

    expect(await reference(situation, "a wheel")).not.toBe(untouched);
  });

  // Every input of the table, delivered on the page (`deliver`).
  for (const row of THE_READERS_CASES) {
    const outcome = row.moves
      ? "is the reader moving on, as a wheel is"
      : "leaves the put-back as no act at all leaves it";
    test(`${row.entry}: ${row.name} during ${situation} ${outcome}`, async () => {
      const read = await stillPutsThemBack(situation, (editor, page) =>
        deliver(row.input, editor, page)
      );

      expect(read).toBe(await reference(situation, row.moves ? "a wheel" : "nothing"));
    });
  }

  // The focus moving out of the document is no entry of the table but this watch's own stop
  // (`focused`, `refocus-when-settled.ts`), and is read as one once the reader has acted: a press
  // on a control outside the document, a tap into a field outside it, a dialog that took the focus.
  // The press is what a browser hands the page before the focus it moves, so it comes first here.
  test(`the focus moving out of the document: a focus moved to a control outside the document during ${situation} is the reader moving on, as a wheel is`, async () => {
    const read = await stillPutsThemBack(situation, async () => {
      const control = document.createElement("button");
      document.body.append(control);
      control.dispatchEvent(eventOf({ pointerType: "mouse", type: "pointerdown" }));
      control.focus();
      control.remove();
    });

    expect(read).toBe(await reference(situation, "a wheel"));
  });

  // The page moving the focus by itself, with no input of the reader's before it: a comment link
  // expands its card in the margin, whose reply field takes the focus, and React commits that
  // after the landing's own focus in some runs and before it in others. Read as the reader, it
  // ended the landing before the web font grew the paragraphs above the quote, and the quote was
  // left 21.6 px from where the link put it in 3 of 20 Firefox runs (`e2e/deep-links.e2e.ts`, "a
  // composer opened from the document is in the margin's view after a comment link").
  test(`the focus moving out of the document: a focus the page moves to a field outside the document by itself during ${situation} leaves the put-back as no act at all leaves it`, async () => {
    const read = await stillPutsThemBack(situation, async () => {
      const field = document.createElement("textarea");
      document.body.append(field);
      field.focus();
      field.remove();
    });

    expect(read).toBe(await reference(situation, "nothing"));
  });
}

/** A document drawn as the editor draws one after the swap: twenty blocks a hundred pixels
 *  tall, standing where the page is scrolled to. `grow` grows the block at `index`, which moves
 *  every block under it, and `hide` takes the whole document's box away, as a tab the reader has
 *  switched from does; each tells the drawn root's observers, as the browser does after layout. */
function aDocument(): {
  element: HTMLElement;
  grow(index: number, by: number): void;
  hide(hidden: boolean): void;
} {
  const grown = new Map<number, number>();
  let hidden = false;
  const topOf = (index: number) => {
    let top = 0;
    for (let above = 0; above < index; above += 1) top += 100 + (grown.get(above) ?? 0);
    return top - window.scrollY;
  };
  const element = document.createElement("div");
  const tell = () => {
    sizes.resize();
  };
  element.getBoundingClientRect = () =>
    (hidden
      ? { height: 0, top: 0 }
      : { height: topOf(20) + window.scrollY, top: topOf(0) }) as DOMRect;
  for (let index = 0; index < 20; index += 1) {
    const block = document.createElement("section");
    block.getBoundingClientRect = () =>
      (hidden ? { bottom: 0, height: 0, top: 0 } : { top: topOf(index) }) as DOMRect;
    block.getClientRects = () =>
      (hidden ? [] : [block.getBoundingClientRect()]) as unknown as DOMRectList;
    element.append(block);
  }
  document.body.append(element);
  return {
    element,
    grow(index, by) {
      grown.set(index, (grown.get(index) ?? 0) + by);
      tell();
    },
    hide(next) {
      hidden = next;
      tell();
    },
  };
}

/** Where the reader stood when the editor was swapped in: block 10's top 50 px above the top of
 *  their screen, which is the page scrolled to 1,050. */
const THEIR_PLACE = { index: 10, top: -50 };

// The page's own `s` focuses the issue's Status, and a plain `focus()` scrolls the page to the
// header. That scroll comes while the watch's own correction is still due, which lets it through
// as that correction's movement, so only the key itself can end the watch: read as the letter it
// is, the next growth put the reader back on the link's target with the control they asked for
// off the screen (the review's scratch row: `s` then a growth ran the landing once, PageDown ran
// it 0 times).
test("a shortcut that focuses a header control ends a link's landing, though its scroll comes inside a correction", async () => {
  const page = pageScroll();
  await page.to(1_050);
  const drawn = drawnRoot();
  document.body.append(drawn.element);
  let landed = 0;
  const stop = refocusWhenSettled(() => (landed += 1), drawn.element);
  // `p`, which focuses the issue's priority as `s` focuses its Status.
  const header = appKeymap.register("global", [
    {
      effect: "moves the reader",
      id: "priority-in-the-header",
      keys: "p",
      label: "Focus the priority",
      run: () => void page.to(0),
    },
  ]);
  try {
    pressOnThePage("p");
    await Bun.sleep(0);
    drawn.grow(300);
    await Bun.sleep(AFTER_THE_QUIET);

    expect(landed).toBe(0);
  } finally {
    header();
    stop();
    drawn.element.remove();
  }
});

test("a shortcut that focuses a header control during the editor's put-back leaves the reader at the control", async () => {
  const page = pageScroll();
  await page.to(1_050);
  const shown = aDocument();
  const stop = refocusWhenSettled(
    () => putTheReaderBack(shown.element, THEIR_PLACE),
    shown.element
  );
  const header = appKeymap.register("global", [
    {
      effect: "moves the reader",
      id: "priority-in-the-header",
      keys: "p",
      label: "Focus the priority",
      run: () => void page.to(0),
    },
  ]);
  try {
    pressOnThePage("p");
    await Bun.sleep(0);
    // A block above the reader's old place paints at its own height as the build goes on.
    shown.grow(3, 200);
    await Bun.sleep(AFTER_THE_QUIET);

    expect(window.scrollY).toBe(0);
  } finally {
    header();
    stop();
    shown.element.remove();
  }
});

// A tab chord switches the tab with `button.click()`, which sends no press, so the put-back
// armed after the swap was not ended: the Spec, hidden, reads as no box, and the put-back that
// was still watching it ran on the tab the reader had switched to, and again when they came back.
test("a tab chord during the editor's put-back ends it, and coming back to the tab is not put back", async () => {
  const page = pageScroll();
  await page.to(1_050);
  const shown = aDocument();
  let putBack = 0;
  const stop = refocusWhenSettled(() => {
    putBack += 1;
    putTheReaderBack(shown.element, THEIR_PLACE);
  }, shown.element);
  // `t v`, the Activity tab's chord.
  const tabs = appKeymap.register("global", [
    {
      effect: "moves the reader",
      id: "tab-activity",
      keys: "t v",
      label: "Activity tab",
      run: () => shown.hide(true),
    },
  ]);
  try {
    pressOnThePage("t");
    pressOnThePage("v");
    await Bun.sleep(AFTER_THE_QUIET);
    // Back to the Spec, which the build has meanwhile grown above the reader's old place.
    shown.hide(false);
    shown.grow(3, 200);
    await Bun.sleep(AFTER_THE_QUIET);

    expect({ putBack, scrollY: window.scrollY }).toEqual({ putBack: 0, scrollY: 1_050 });
  } finally {
    tabs();
    stop();
    shown.element.remove();
  }
});

// However the tab was switched - its own button, the palette's row, a script - a document drawn
// with no box is no place to put a reader back on: measured as its all-zero rectangle, the
// put-back scrolled the tab the reader had switched to (1,000 to 1,040 in the review's harness).
test("a put-back still watching a document hidden behind another tab moves nothing", async () => {
  const page = pageScroll();
  await page.to(1_050);
  const shown = aDocument();
  const stop = refocusWhenSettled(
    () => putTheReaderBack(shown.element, THEIR_PLACE),
    shown.element
  );
  try {
    // The tab's own button, clicked by something that is no input of the reader's.
    shown.hide(true);
    await Bun.sleep(AFTER_THE_QUIET);

    expect(window.scrollY).toBe(1_050);
  } finally {
    stop();
    shown.element.remove();
  }
});
