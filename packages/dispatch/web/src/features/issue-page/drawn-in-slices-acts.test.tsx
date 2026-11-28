import { afterEach, beforeEach, expect, test } from "bun:test";
import { act, render } from "@testing-library/react";
import { memo, type ReactNode, useCallback, useRef } from "react";

import { useDrawnInSlices } from "./drawn-in-slices";
import { framesByHand } from "./outline-fixtures";
import { type LayoutByHand, layoutByHand, SCREEN_PX, tasksByHand } from "./slices-fixtures";

/**
 * What the commit of a reader's act that changes the rows reads (`drawn-in-slices.ts`): a fold,
 * an unfold and an Indent. That commit reads no row's box and not the window's scroll, so the
 * browser lays the change out once, in the frame that paints it, and not inside the act's task:
 * a read of a box there lays out every row of the view first. The screen below the rows an unfold
 * or an Indent brings is filled in that frame, before it is painted, and the row the reader reads
 * is read there too, so the slices after the act still hold it where it stands (the state table
 * "The rows drawn in slices", rows 3, 9 and 10).
 *
 * The page is laid out by hand (`layoutByHand`), its slices are run by hand (`tasksByHand`) and so
 * are its frames (`framesByHand`), as `drawn-in-slices-reads.test.tsx` lays and runs them.
 */

/** Every stand-in a case made, put back after it. */
let restores: (() => void)[] = [];
beforeEach(() => {
  restores = [];
});
afterEach(() => {
  for (const restore of restores.reverse()) restore();
});

/** One line the hook draws. */
const DrawnLine = memo(function DrawnLine({ line }: { line: string }): ReactNode {
  return (
    <div data-line={line} data-node-row="">
      {line}
    </div>
  );
});

/** The lines drawn through the hook, as `TreeRows.tsx` draws its rows: a row's top read from its
 *  element's box. */
function Lines({
  anchor,
  focused,
  lines,
}: {
  anchor: string;
  focused: string;
  lines: readonly string[];
}): ReactNode {
  const box = useRef<HTMLDivElement | null>(null);
  const measure = useCallback(
    (key: string) =>
      box.current?.querySelector(`[data-line="${key}"]`)?.getBoundingClientRect().top,
    []
  );
  const { drawn } = useDrawnInSlices({
    anchor,
    focused,
    keyOf: (line: string) => line,
    measure,
    rows: lines,
  });
  return (
    <div ref={box}>
      {drawn.map((line) => (
        <DrawnLine key={line} line={line} />
      ))}
    </div>
  );
}

/** Sixty lines, `l00` to `l59`. */
const LINES = Array.from({ length: 60 }, (_unused, at) => `l${String(at).padStart(2, "0")}`);

/** The lines drawn in `container`, in order. */
function linesDrawn(container: HTMLElement): string[] {
  return [...container.querySelectorAll<HTMLElement>("[data-line]")].map(
    (line) => line.dataset.line ?? ""
  );
}

/** Counts each read that makes the browser lay the page out, from here on: an element's box, and
 *  the window's scroll. It stands over the layout by hand, which it is made after, and is put
 *  back before it. */
function countLayoutReads(): { readonly count: () => number; readonly reset: () => void } {
  const laidOut = HTMLElement.prototype.getBoundingClientRect;
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  let reads = 0;
  HTMLElement.prototype.getBoundingClientRect = function (this: HTMLElement): DOMRect {
    reads += 1;
    return laidOut.call(this);
  };
  Object.defineProperty(window, "scrollY", {
    configurable: true,
    get: () => {
      reads += 1;
      return scrollY?.get?.call(window) ?? 0;
    },
  });
  restores.push(() => {
    HTMLElement.prototype.getBoundingClientRect = laidOut;
    if (scrollY !== undefined) Object.defineProperty(window, "scrollY", scrollY);
  });
  return {
    count: () => reads,
    reset: () => {
      reads = 0;
    },
  };
}

/** The page laid out by hand, with `height` for each row, its slices and frames run by hand, and
 *  the reads that lay it out counted. */
function byHand(height?: (element: Element) => number) {
  const layout = layoutByHand(height === undefined ? {} : { height });
  const tasks = tasksByHand();
  const frames = framesByHand();
  restores.push(layout.restore, tasks.restore, frames.restore);
  return { frames, layout, reads: countLayoutReads(), tasks };
}

/** The reader's first act, a press anywhere: from it on, a read moves nothing (the first drawing's
 *  row 5). */
function readerActs(): void {
  act(() => {
    window.dispatchEvent(new Event("pointerdown"));
  });
}

/** The top of the line `key` drawn in `container`, in the window laid out by hand. */
function topOf(layout: LayoutByHand, container: HTMLElement, key: string): number {
  return layout.topOf(container.querySelector(`[data-line="${key}"]`)) ?? Number.NaN;
}

test("an unfold of more rows than a screen holds reads no row's box in its commit, and the frame after it fills the screen below the row", async () => {
  // The replies are 40 px each, shorter than the rows the page measured at its first drawing.
  const { frames, layout, reads, tasks } = byHand((element) =>
    element.getAttribute("data-line")?.startsWith("r") === true ? 40 : 100
  );
  const view = render(<Lines anchor="l00" focused="l00" lines={LINES} />);
  try {
    await tasks.runAll();
    expect(linesDrawn(view.container), "the slices left rows not drawn").toEqual(LINES);
    readerActs();
    const replies = Array.from({ length: 30 }, (_unused, at) => `r${String(at).padStart(2, "0")}`);
    const unfolded = [LINES[0] ?? "", ...replies, ...LINES.slice(1)];
    reads.reset();
    view.rerender(<Lines anchor="l00" focused="l00" lines={unfolded} />);
    expect(reads.count(), "the unfold's commit read a row's box or the window's scroll").toBe(0);
    const drawn = linesDrawn(view.container);
    expect(drawn, "the unfold drew no reply at once").toContain("r00");
    expect(drawn, "the unfold drew every reply at once").not.toContain("r29");
    expect(frames.asked().length, "the unfold asked for no frame to fill the screen").toBe(1);
    await frames.run();
    const filled = linesDrawn(view.container).filter((line) => line.startsWith("r"));
    expect(
      topOf(layout, view.container, filled.at(-1) ?? "") - topOf(layout, view.container, "r00"),
      "the frame after the unfold left the screen below the unfolded row unfilled"
    ).toBeGreaterThanOrEqual(SCREEN_PX);
    expect(filled, "the frame after the unfold drew every reply").not.toContain("r29");
    await tasks.runAll();
    expect(linesDrawn(view.container), "the slices after the unfold left rows not drawn").toEqual(
      unfolded
    );
  } finally {
    view.unmount();
  }
});

test("a fold above the row the reader reads, while rows are still to be drawn, reads no row's box in its commit, and the slices after it hold that row where it stands", async () => {
  const { frames, layout, reads, tasks } = byHand();
  const view = render(<Lines anchor="l30" focused="l30" lines={LINES} />);
  try {
    const anchor = () => topOf(layout, view.container, "l30");
    // The reader's window, 100 px above the row they read.
    layout.scrollTo(anchor() + layout.scrolled() - 100);
    readerActs();
    const folded = LINES.filter((line) => line !== "l25" && line !== "l26");
    reads.reset();
    view.rerender(<Lines anchor="l30" focused="l30" lines={folded} />);
    expect(reads.count(), "the fold's commit read a row's box or the window's scroll").toBe(0);
    expect(linesDrawn(view.container), "the fold left a folded row drawn").not.toContain("l25");
    // The browser's scroll anchoring holds the row where it stood, as a scroll the hook did not
    // make.
    layout.scrollTo(layout.scrolled() - 200);
    act(() => {
      window.dispatchEvent(new Event("scroll"));
    });
    expect(anchor(), "the fold moved the row the reader reads").toBe(100);
    await frames.run();
    for (let ran = 0; ran < LINES.length && tasks.pending() > 0; ran += 1) {
      await tasks.runNext();
      expect(anchor(), `slice ${ran + 1} after the fold moved the row the reader reads`).toBe(100);
    }
    expect(linesDrawn(view.container), "the slices after the fold left rows not drawn").toEqual(
      folded
    );
  } finally {
    view.unmount();
  }
});

test("an Indent, whose rows take new keys, reads no row's box in its commit, and its rows are all drawn after it", async () => {
  const { frames, reads, tasks } = byHand();
  const view = render(<Lines anchor="l00" focused="l10" lines={LINES} />);
  try {
    await tasks.runAll();
    readerActs();
    // The row of l10 indented under l09, with the nine rows under it: each row's key is its path.
    const moved = (line: string) => (line >= "l10" && line <= "l19" ? `l09/${line}` : line);
    const indented = LINES.map(moved);
    reads.reset();
    view.rerender(<Lines anchor="l00" focused="l09/l10" lines={indented} />);
    expect(reads.count(), "the Indent's commit read a row's box or the window's scroll").toBe(0);
    expect(linesDrawn(view.container), "the Indent drew its own row late").toContain("l09/l10");
    await frames.run();
    await tasks.runAll();
    expect(linesDrawn(view.container), "the rows the Indent moved were not all drawn").toEqual(
      indented
    );
  } finally {
    view.unmount();
  }
});
