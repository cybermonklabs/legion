import { afterEach, expect, spyOn, test } from "bun:test";
import { act, render } from "@testing-library/react";

import { installIntersectionObserver } from "../../__tests__/intersection-observer";
import { CutWords } from "./CutWords";

/**
 * The page's one measurer of where each row's words are cut (`clamp-measurer.ts`). Each row once
 * measured its own words in its own layout effect, after the rows before it had written to the
 * page, so the browser laid the page out again for each row; one batch that read every near row
 * of a large issue at once then held the main thread for 414 ms.
 */

/** How many rows the page draws, and how many of them stand within a screen of the viewport. */
const ROWS = 60;
const NEAR = 50;

const WORDS = Array.from({ length: 100 }, (_unused, at) => `word${at + 1}`).join(" ");

const restores: (() => void)[] = [];

afterEach(() => {
  for (const restore of restores.splice(0).reverse()) restore();
});

/** A size observer a test drives: `resize` reports a new size of `target` to every observer
 *  watching it. happy-dom's reports nothing. */
function installResizeObserver() {
  const live: { callback: ResizeObserverCallback; targets: Set<Element> }[] = [];
  const original = globalThis.ResizeObserver;
  class ObserverStub {
    readonly callback: ResizeObserverCallback;
    readonly targets = new Set<Element>();
    constructor(callback: ResizeObserverCallback) {
      this.callback = callback;
      live.push(this);
    }
    disconnect(): void {
      this.targets.clear();
    }
    observe(target: Element): void {
      this.targets.add(target);
    }
    unobserve(target: Element): void {
      this.targets.delete(target);
    }
  }
  globalThis.ResizeObserver = ObserverStub as unknown as typeof ResizeObserver;
  restores.push(() => {
    globalThis.ResizeObserver = original;
  });
  return (target: Element, width: number) =>
    act(() => {
      for (const observer of live) {
        if (!observer.targets.has(target)) continue;
        const entry = { contentRect: { width }, target } as unknown as ResizeObserverEntry;
        observer.callback([entry], observer as unknown as ResizeObserver);
      }
    });
}

/**
 * `ROWS` rows of cut words, with the frames the page asks for run by the test and a clock that
 * moves `readMs` on each read of a box's height. Each such read notes how many rows were drawn
 * cut when it was made, in the list of the frame it was made in. Every box lays out where `box`
 * says, by default a 600 px box right under the view, where a row is near and wholly outside the
 * view; `observer` false draws the rows in a browser with no intersection observer, and `fonts`
 * stands for the page's font set.
 */
function drawRows({
  box = () => new DOMRect(0, window.innerHeight, 600, 600),
  fonts,
  observer = true,
  readMs = 0,
}: {
  box?: (element: HTMLElement) => DOMRect;
  fonts?: EventTarget;
  observer?: boolean;
  readMs?: number;
} = {}) {
  const near = installIntersectionObserver();
  restores.push(near.restore);
  if (!observer) {
    globalThis.IntersectionObserver = undefined as unknown as typeof IntersectionObserver;
  }
  const resize = installResizeObserver();
  if (fonts !== undefined) {
    Object.defineProperty(document, "fonts", { configurable: true, value: fonts });
    restores.push(() => Reflect.deleteProperty(document, "fonts"));
  }
  const frames: FrameRequestCallback[] = [];
  const frame = spyOn(window, "requestAnimationFrame").mockImplementation((callback) => {
    frames.push(callback);
    return frames.length;
  });
  const cancel = spyOn(window, "cancelAnimationFrame").mockImplementation(() => {});
  let clock = 0;
  const now = spyOn(performance, "now").mockImplementation(() => clock);
  restores.push(
    () => frame.mockRestore(),
    () => cancel.mockRestore(),
    () => now.mockRestore()
  );
  const cutWhenRead: number[][] = [[]];
  const own = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "scrollHeight");
  Object.defineProperty(HTMLElement.prototype, "scrollHeight", {
    configurable: true,
    get: () => {
      clock += readMs;
      cutWhenRead.at(-1)?.push(document.querySelectorAll("[data-node-more]").length);
      return 600;
    },
  });
  restores.push(() => {
    if (own === undefined) Reflect.deleteProperty(HTMLElement.prototype, "scrollHeight");
    else Object.defineProperty(HTMLElement.prototype, "scrollHeight", own);
  });
  // On Element, which owns the method: a spy on HTMLElement leaves an own property behind once
  // restored, which hides every later spy on Element (Margin.landing.test.tsx's `stubRects`).
  // Any other element keeps the empty box the DOM gives it.
  const boxes = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    return this instanceof HTMLElement ? box(this) : new DOMRect();
  });
  restores.push(() => boxes.mockRestore());
  const view = render(
    <ol>
      {Array.from({ length: ROWS }, (_unused, at) => (
        // biome-ignore lint/suspicious/noArrayIndexKey: the rows never move.
        <li data-node-row="" key={at}>
          <CutWords className="" whole={false}>
            <p>{WORDS}</p>
          </CutWords>
        </li>
      ))}
    </ol>
  );
  restores.push(() => view.unmount());
  const words = [...view.container.querySelectorAll<HTMLElement>("[data-node-words]")];
  /** Runs the next frame the page asked for; its reads go in a list of their own. */
  const runFrame = () => {
    cutWhenRead.push([]);
    act(() => {
      frames.shift()?.(clock);
    });
  };
  /** Whether the row `words` holds is drawn cut. */
  const cut = (element: Element | undefined) =>
    element?.parentElement?.querySelector("[data-node-more]") != null;
  return { cut, cutWhenRead, frames, near, resize, runFrame, view, words };
}

// When many rows mount, the words of the ones near the viewport are measured in one batch, a
// frame after the intersection observer says where they stand. Each of them is read once, and
// every read of the batch comes before its first write: no read sees a row already cut. The rows
// away from the viewport are not read.
test("many rows' words are measured in one batch a frame, every read before any write", () => {
  const { cutWhenRead, frames, near, runFrame, view, words } = drawRows();
  expect(words).toHaveLength(ROWS);
  expect(cutWhenRead.flat(), "a row read its words' layout as it mounted").toEqual([]);
  expect(frames, "a batch was asked for before anything said where the rows stand").toEqual([]);

  act(() => {
    for (const element of words.slice(0, NEAR)) near.show(element);
  });
  expect(frames, "the near rows asked for more than one batch").toHaveLength(1);
  runFrame();
  expect(
    cutWhenRead.flat(),
    "a near row was read other than once, or a far row was read"
  ).toHaveLength(NEAR);
  expect(
    cutWhenRead.flat().filter((cut) => cut > 0),
    "a read of the batch came after a row of it was drawn cut"
  ).toEqual([]);
  expect(view.container.querySelectorAll("[data-node-more]")).toHaveLength(NEAR);
  expect(
    words.slice(NEAR).filter((element) => element.parentElement?.querySelector("[data-node-more]")),
    "a row away from the viewport was cut"
  ).toEqual([]);
  expect(frames, "the batch asked for another").toEqual([]);
});

// A frame reads for at most 50 ms. With each read taking 10 ms, a batch of 50 near rows goes on
// over ten frames of five rows each; each frame reads all its rows before it draws any of them
// cut, and each frame cuts the rows it read.
test("a batch bigger than one slice goes on over the frames after it, each slice reading before it writes", () => {
  const { cutWhenRead, frames, near, runFrame, view, words } = drawRows({ readMs: 10 });
  act(() => {
    for (const element of words.slice(0, NEAR)) near.show(element);
  });
  const slices: number[] = [];
  while (frames.length > 0) {
    const cutBefore = view.container.querySelectorAll("[data-node-more]").length;
    runFrame();
    const reads = cutWhenRead.at(-1) ?? [];
    slices.push(reads.length);
    expect(
      reads.filter((cut) => cut !== cutBefore),
      "a slice read after it had drawn a row cut"
    ).toEqual([]);
    expect(view.container.querySelectorAll("[data-node-more]")).toHaveLength(
      cutBefore + reads.length
    );
    if (slices.length > NEAR) throw new Error("the batch never ended");
  }
  expect(slices, "the batch was not cut into slices of 50 ms").toEqual(
    Array.from({ length: NEAR / 5 }, () => 5)
  );
});

// A browser with no intersection observer says nothing of where a row stands as the page
// scrolls, so every row is measured, however far down it is laid out.
test("with no intersection observer, a row far from the screen is cut", () => {
  const { cut, frames, runFrame, words } = drawRows({
    box: () => new DOMRect(0, 5000, 600, 600),
    observer: false,
  });
  expect(frames, "no batch was asked for as the rows mounted").toHaveLength(1);
  runFrame();
  expect(
    words.flatMap((element, at) => (cut(element) ? [] : [at])),
    "the rows left whole, by their place from the top, with no intersection observer"
  ).toEqual([]);
});

// The observer's word is from an earlier frame: a row it calls near is read only where its box
// stands within a screen of the viewport as the slice reads it, and a box with no size yet is
// read once the size observer reports one.
test("a row the observer calls near but laid out two screens away is not read, and a box with no size is read once it has one", () => {
  let sized = false;
  let far: Element | undefined;
  let unsized: Element | undefined;
  const drawn = drawRows({
    box: (element) => {
      if (element === far) return new DOMRect(0, 5000, 600, 600);
      if (element === unsized && !sized) return new DOMRect(0, 0, 0, 0);
      return new DOMRect(0, window.innerHeight, 600, 600);
    },
  });
  [far, unsized] = drawn.words;
  act(() => {
    if (far !== undefined) drawn.near.show(far);
    if (unsized !== undefined) drawn.near.show(unsized);
  });
  drawn.runFrame();
  expect(drawn.cutWhenRead.flat(), "a row away from the screen or with no size was read").toEqual(
    []
  );
  sized = true;
  if (unsized !== undefined) drawn.resize(unsized, 600);
  expect(drawn.frames, "the box's first size asked for no batch").toHaveLength(1);
  drawn.runFrame();
  expect(drawn.cut(unsized), "the box that took a size was not cut").toBe(true);
  expect(drawn.cut(far), "the row away from the screen was cut").toBe(false);
});

// The cut sets the words' box's height, so a cut row's own resize at the width it had measures
// nothing; a new width, or one of its blocks changing size, measures it again.
test("a cut row's own resize at its width measures nothing, and a new width or a block's resize measures it again", () => {
  const { cutWhenRead, frames, near, resize, runFrame, words } = drawRows();
  const row = words[0];
  if (row === undefined) throw new Error("no row was drawn");
  act(() => near.show(row));
  resize(row, 600);
  runFrame();
  expect(cutWhenRead.flat()).toHaveLength(1);
  resize(row, 600);
  expect(frames, "the cut box's own height asked for a batch").toEqual([]);
  resize(row, 640);
  expect(frames, "a new width asked for no batch").toHaveLength(1);
  runFrame();
  const block = row.firstElementChild;
  if (block === null) throw new Error("the row draws no block");
  resize(block, 640);
  expect(frames, "a block's resize asked for no batch").toHaveLength(1);
  runFrame();
  expect(cutWhenRead.flat(), "the row was not measured again").toHaveLength(3);
});

// A row that goes away from the screen keeps the cut it last had, with no read; the page's
// typeface arriving measures again every row cut before it, the one away included, and no row
// never measured.
test("a row that goes away keeps its cut, and the typeface's arrival measures again every row measured before it, and only those", () => {
  const fonts = new EventTarget();
  const { cut, cutWhenRead, frames, near, runFrame, words } = drawRows({ fonts });
  const [away, stays] = words;
  act(() => {
    if (away !== undefined) near.show(away);
    if (stays !== undefined) near.show(stays);
  });
  runFrame();
  expect([cut(away), cut(stays)]).toEqual([true, true]);
  act(() => {
    if (away !== undefined) near.hide(away);
  });
  expect(frames, "a row going away asked for a batch").toEqual([]);
  expect(cut(away), "the row that went away lost its cut").toBe(true);
  act(() => {
    fonts.dispatchEvent(new Event("loadingdone"));
  });
  expect(frames, "the typeface's arrival asked for no batch").toHaveLength(1);
  runFrame();
  expect(
    cutWhenRead.at(-1),
    "the typeface did not measure again the two rows measured before it alone"
  ).toHaveLength(2);
  expect([cut(away), cut(stays)]).toEqual([true, true]);
});

/**
 * `drawRows` laid out as a page the test scrolls: the rows (`[data-node-row]`) stand one under
 * another, each `whole` px while its words are whole, or its cut and a 24 px "more" once cut, and
 * `window.scrollBy` moves the page. The page mounts scrolled a screen above its first row, so the
 * mount's own pass, which `mountPass` lets run, finds no row in view, and every cut is a batch's.
 * A row's words lay out `whole` px tall until `typefaceArrives` lays them out taller in the
 * page's typeface, which tells in the count a cut holds back (`hiddenOf`) which face it was
 * measured on. `read` holds the place of every row whose box, or whose words' box, was read;
 * `scrolls` holds each scroll, with how many rows were drawn cut when it was made.
 */
function stackRows(whole: number) {
  const MORE = 24;
  let words: HTMLElement[] = [];
  let rows: Element[] = [];
  let scrolled = -window.innerHeight;
  let laidOut = whole;
  const fonts = new EventTarget();
  const read = new Set<number>();
  const wordsHeight = (element: HTMLElement | undefined) => {
    const cut = Number.parseFloat(element?.style.maxHeight ?? "");
    return Number.isFinite(cut) ? cut : whole;
  };
  const heightOf = (at: number) =>
    wordsHeight(words[at]) + (rows[at]?.querySelector("[data-node-more]") == null ? 0 : MORE);
  const pageTop = (at: number) =>
    Array.from({ length: at }, (_unused, before) => heightOf(before)).reduce(
      (sum, h) => sum + h,
      0
    );
  /** Where row `at` stands in the viewport. */
  const topOf = (at: number) => pageTop(at) - scrolled;
  const drawn = drawRows({
    box: (element) => {
      // A block is laid out whole from its words' top, as the words would draw it uncut.
      const block = words.indexOf(element.parentElement as HTMLElement);
      if (block >= 0) return new DOMRect(0, topOf(block), 600, laidOut);
      const at = words.indexOf(element);
      if (at >= 0) {
        read.add(at);
        return new DOMRect(0, topOf(at), 600, wordsHeight(element));
      }
      const row = rows.indexOf(element);
      if (row < 0) return new DOMRect(0, 0, 0, 0);
      read.add(row);
      return new DOMRect(0, topOf(row), 600, heightOf(row));
    },
    fonts,
  });
  words = drawn.words;
  rows = [...drawn.view.container.querySelectorAll("[data-node-row]")];
  const cutCount = () => drawn.view.container.querySelectorAll("[data-node-more]").length;
  const scrolls: { cut: number; top: number }[] = [];
  const scroll = spyOn(window, "scrollBy").mockImplementation(((
    options?: ScrollToOptions | number,
    y?: number
  ) => {
    const top = typeof options === "number" ? (y ?? 0) : (options?.top ?? 0);
    scrolls.push({ cut: cutCount(), top });
    scrolled += top;
  }) as typeof window.scrollBy);
  restores.push(() => scroll.mockRestore());
  /** Says rows `from` up to `to` come within a screen of the view, or go away from it. */
  const near = (from: number, to: number, shown: boolean) =>
    act(() => {
      for (const element of words.slice(from, to)) {
        if (shown) drawn.near.show(element);
        else drawn.near.hide(element);
      }
    });
  return {
    cutCount,
    drawn,
    /** How many words row `at`'s cut holds back, or `undefined` where it is drawn whole. */
    hiddenOf: (at: number) => {
      const more = rows[at]?.querySelector("[data-node-more]")?.textContent ?? "";
      const count = /(\d+)/u.exec(more)?.[1];
      return count === undefined ? undefined : Number(count);
    },
    /** Lets the pass the rows' mount asked for run: it finds no row in view. */
    mountPass: () => act(async () => {}),
    near,
    pageTop,
    read,
    scrollTo: (top: number) => {
      scrolled = top;
    },
    scrolls,
    topOf,
    /** The page's typeface arrives, in which every row's words lay out `height` px tall. */
    typefaceArrives: (height: number) => {
      laidOut = height;
      act(() => {
        fonts.dispatchEvent(new Event("loadingdone"));
      });
    },
  };
}

/** Where rows `from` up to `to` stand, as `topOf` reads them. */
function topsOf(topOf: (at: number) => number, from: number, to: number): number[] {
  return Array.from({ length: to - from }, (_unused, at) => topOf(from + at));
}

// A long jump into the middle of the page lands on rows not measured yet. A slice that cuts rows
// below the view alone scrolls nothing and reads no other row's box. After the jump, a slice
// cuts the rows above the view and below it and leaves every row in view whole, wholly or in
// part, and the rows in view stand where they stood, by one scroll in the same frame, after
// every cut. The rows are 300 px whole on a 768 px view.
test("a slice cuts the rows above and below the view and leaves the rows in view whole and where they stood, and one that cuts rows below it alone scrolls nothing", async () => {
  const { cutCount, drawn, mountPass, near, pageTop, read, scrollTo, scrolls, topOf } =
    stackRows(300);
  await mountPass();
  const screen = window.innerHeight;

  // Rows 0 and 1 stand wholly in view and row 2 crosses its foot; rows 3 to 5 lie below it,
  // within a screen.
  scrollTo(0);
  expect(topOf(3), "row 3 is not below the view").toBeGreaterThanOrEqual(screen);
  near(3, 6, true);
  read.clear();
  drawn.runFrame();
  expect(
    [3, 4, 5].map((at) => drawn.cut(drawn.words[at])),
    "a row below the view was not cut"
  ).toEqual([true, true, true]);
  expect(scrolls, "a slice that cut rows below the view alone scrolled the page").toEqual([]);
  expect(
    [...read].sort((one, other) => one - other),
    "the slice read another row's box"
  ).toEqual([3, 4, 5]);

  // The jump: row 20 crosses the view's top, rows 21 and 22 stand wholly in view and row 23
  // crosses its foot; rows 17 to 25, whole, come within a screen of the view and rows 3 to 5 go
  // away.
  near(3, 6, false);
  scrollTo(pageTop(20) + 150);
  expect(topOf(20), "row 20 does not cross the view's top").toBeLessThan(0);
  expect(topOf(23), "row 23 does not cross the view's foot").toBeLessThan(screen);
  expect(topOf(24), "row 24 is not below the view").toBeGreaterThanOrEqual(screen);
  const before = topsOf(topOf, 20, 24);
  near(17, 26, true);
  drawn.runFrame();
  expect(
    topsOf(topOf, 20, 24).map((top, at) => top - (before[at] ?? 0)),
    "a row in view moved as the rows around it were cut"
  ).toEqual([0, 0, 0, 0]);
  expect(
    drawn.words.slice(17, 26).map((element) => drawn.cut(element)),
    "a row in view was cut, or a row outside it was not"
  ).toEqual([true, true, true, false, false, false, false, true, true]);
  expect(scrolls, "the page was scrolled other than once, after every cut of the slice").toEqual([
    { cut: cutCount(), top: expect.any(Number) },
  ]);
});

// Rows taller than the view leave no row wholly in view. The one that spans the view's top, row
// 10, keeps its whole length though only part of it is in view, and the slice holds it where it
// stood as the row above it is cut. The rows are 1,000 px whole on a 768 px view.
test("a row taller than the view that spans its top keeps its whole length, and stands where it stood as the row above it is cut", async () => {
  const { cutCount, drawn, mountPass, near, pageTop, scrollTo, scrolls, topOf } = stackRows(1000);
  await mountPass();
  scrollTo(pageTop(10) + 200);
  const before = topOf(10);
  expect(before, "row 10 does not span the view's top").toBeLessThan(0);
  expect(topOf(11), "row 11 does not start below the view").toBeGreaterThanOrEqual(
    window.innerHeight
  );
  near(9, 12, true);
  drawn.runFrame();
  expect(
    topOf(10) - before,
    "the row that spans the view's top moved as the row above it was cut"
  ).toBe(0);
  expect(
    drawn.words.slice(9, 12).map((element) => drawn.cut(element)),
    "the row in view was cut, or a row outside it was not"
  ).toEqual([true, false, true]);
  expect(scrolls, "the page was scrolled other than once, after every cut of the slice").toEqual([
    { cut: cutCount(), top: expect.any(Number) },
  ]);
});

// A row a slice leaves whole in view is put in the next slice by every scroll. A scroll that
// leaves it in view measures nothing; one that leaves it wholly above the view cuts it, and the
// row that spans the view's top then stands where it stood. The rows are 1,000 px whole.
test("a row left whole in view is cut once a scroll leaves it wholly outside the view, and not before", async () => {
  const { cutCount, drawn, mountPass, near, pageTop, scrollTo, scrolls, topOf } = stackRows(1000);
  await mountPass();
  scrollTo(pageTop(10) + 200);
  near(9, 12, true);
  drawn.runFrame();
  expect(drawn.cut(drawn.words[10]), "the row in view was cut").toBe(false);

  scrollTo(pageTop(10) + 500);
  document.dispatchEvent(new Event("scroll"));
  expect(drawn.frames, "a scroll asked for no batch").toHaveLength(1);
  drawn.runFrame();
  expect(drawn.cutWhenRead.at(-1), "a scroll that left the row in view measured it").toEqual([]);
  expect(drawn.cut(drawn.words[10]), "the row still in view was cut").toBe(false);

  scrollTo(pageTop(11) + 50);
  const before = topOf(11);
  expect(topOf(10) + 1000, "row 10 does not lie wholly above the view").toBeLessThanOrEqual(0);
  document.dispatchEvent(new Event("scroll"));
  drawn.runFrame();
  expect(drawn.cut(drawn.words[10]), "the row that left the view was not cut").toBe(true);
  expect(
    topOf(11) - before,
    "the row that spans the view's top moved as the row above it was cut"
  ).toBe(0);
  expect(scrolls.at(-1), "the last write of the slice was not its scroll").toEqual({
    cut: cutCount(),
    top: expect.any(Number),
  });
});

// A row spans the view's top where its top is at or above the view's top and its foot below it:
// a row taller than the view whose top is exactly at the view's top is held as the row above it
// is cut. The rows are 1,000 px whole on a 768 px view.
test("a row taller than the view whose top is at the view's top stands where it stood as the row above it is cut", async () => {
  const { drawn, mountPass, near, pageTop, scrollTo, topOf } = stackRows(1000);
  await mountPass();
  scrollTo(pageTop(10));
  expect(topOf(10), "row 10's top is not at the view's top").toBe(0);
  near(9, 12, true);
  drawn.runFrame();
  expect(
    topOf(10),
    "the row whose top is at the view's top moved as the row above it was cut"
  ).toBe(0);
  expect(
    drawn.words.slice(9, 12).map((element) => drawn.cut(element)),
    "the row in view was cut, or a row outside it was not"
  ).toEqual([true, false, true]);
});

// The rows in view as the page mounts are measured once the commit's layout effects have run and
// drawn cut before the page is painted: with no animation frame run, and every read of them
// before any cut. A row outside the view is left to the batches. Rows 0 to 2 stand in view.
test("the rows in view as they mount are cut before the page is painted, every read before any write, and the rows outside the view are left to the batches", async () => {
  let rows: (Element | null)[] = [];
  const drawn = drawRows({
    box: (element) => {
      const at = rows.indexOf(element.closest("li"));
      return new DOMRect(0, at * 300, 600, element.matches("p") ? 600 : 300);
    },
  });
  rows = [...drawn.view.container.querySelectorAll("li")];
  await act(async () => {});
  expect(
    drawn.words.slice(0, 6).map((element) => drawn.cut(element)),
    "a row in view as it mounted was not cut before a frame ran, or a row outside it was"
  ).toEqual([true, true, true, false, false, false]);
  expect(drawn.frames, "the rows in view asked for a frame to be cut").toEqual([]);
  expect(
    drawn.cutWhenRead.flat(),
    "a row in view was read other than once, or a read came after a cut"
  ).toEqual([0, 0, 0]);
});

// The typeface's arrival marks stale every row whose cut was measured on the fallback face. A
// stale row outside the view is measured in the next batch wherever it stands, the one left far
// behind included; a stale row in view keeps its cut until a scroll leaves it wholly outside the
// view; a row never measured is not read. The words hold back 52 words cut on the fallback face,
// where they lay out 300 px tall, and 76 on the typeface, where they lay out 600 px tall.
test("the typeface's arrival measures again every row cut on the fallback face: one outside the view at once, near or far, and one in view once a scroll leaves it wholly outside the view", async () => {
  const { drawn, hiddenOf, mountPass, near, pageTop, scrollTo, typefaceArrives } = stackRows(300);
  await mountPass();
  const hidden = (rows: number[]) => rows.map((at) => hiddenOf(at));
  // Row 43 is cut as it stands just under the view, then left far behind.
  scrollTo(pageTop(40));
  near(43, 44, true);
  drawn.runFrame();
  near(43, 44, false);
  // Rows 3 to 5 are cut under the view at the page's top; then the page scrolls until rows 3
  // and 4 stand in view and row 5 just under it.
  scrollTo(0);
  near(3, 6, true);
  drawn.runFrame();
  scrollTo(400);
  expect(hidden([3, 4, 5, 43]), "a row was not cut on the fallback face").toEqual([52, 52, 52, 52]);

  typefaceArrives(600);
  expect(drawn.frames, "the typeface's arrival asked for no batch").toHaveLength(1);
  drawn.runFrame();
  expect(
    hidden([3, 4, 5, 43]),
    "a stale row outside the view kept its cut on the fallback face, or one in view was cut again"
  ).toEqual([52, 52, 76, 76]);
  expect(
    [0, 1, 2, 40].map((at) => drawn.cut(drawn.words[at])),
    "a row never measured was measured"
  ).toEqual([false, false, false, false]);

  // A scroll leaves row 3 wholly above the view; row 4 stays in it.
  scrollTo(pageTop(4) + 10);
  document.dispatchEvent(new Event("scroll"));
  drawn.runFrame();
  expect(
    hidden([3, 4]),
    "a stale row the scroll left outside the view kept its cut on the fallback face"
  ).toEqual([76, 52]);
});
