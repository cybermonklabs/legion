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
 * says, a 600 px box at the top by default; `observer` false draws the rows in a browser with no
 * intersection observer, and `fonts` stands for the page's font set.
 */
function drawRows({
  box = () => new DOMRect(0, 0, 600, 600),
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
  const boxes = spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement
  ) {
    return box(this);
  });
  restores.push(() => boxes.mockRestore());
  const view = render(
    <ol>
      {Array.from({ length: ROWS }, (_unused, at) => (
        // biome-ignore lint/suspicious/noArrayIndexKey: the rows never move.
        <li key={at}>
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
    words.filter((element) => !cut(element)),
    "a far row was left whole"
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
      return new DOMRect(0, 0, 600, 600);
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
// typeface arriving measures the rows near the screen again, and only those.
test("a row that goes away keeps its cut, and the typeface's arrival measures the near rows again", () => {
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
  expect(cutWhenRead.at(-1), "the typeface did not measure the near row alone").toHaveLength(1);
  expect([cut(away), cut(stays)]).toEqual([true, true]);
});
