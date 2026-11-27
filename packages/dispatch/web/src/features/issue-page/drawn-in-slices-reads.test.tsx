import { afterEach, beforeEach, expect, test } from "bun:test";
import { act, render } from "@testing-library/react";
import { memo, Profiler, type ReactNode, useCallback, useRef } from "react";

import { COMMIT_MS, holdSlices, SLICE_LIMIT_MS, useDrawnInSlices } from "./drawn-in-slices";
import { clockByHand, layoutByHand, tasksByHand } from "./slices-fixtures";

/**
 * What a slice reads and how much one of its commits draws (`drawn-in-slices.ts`): the row the
 * reader reads (`referenceRow`) is read once, and again only where the reader scrolled, the focus
 * moved or the rows above it changed, so a commit that draws rows below it alone reads no row's
 * top and the browser lays the slice's rows out once, as it paints; a commit that draws rows above
 * it reads that row's top alone; no commit of a slice draws more than `COMMIT_MS` of rows by what
 * its costliest row took; and a first drawing from the first row draws the rows that reach the
 * window's foot alone, and that row alone where the rows stand below it.
 *
 * The page is laid out by hand (`layoutByHand`) and each slice is run by hand (`tasksByHand`), as
 * `drawn-in-slices.test.tsx` lays and runs them; a line's drawing moves the clock by hand by what
 * it costs (`clockByHand`).
 */

/** Every stand-in a case made, put back after it. */
let restores: (() => void)[] = [];
beforeEach(() => {
  restores = [];
});
afterEach(() => {
  for (const restore of restores.reverse()) restore();
});

/** What the lines' drawing reads and writes: the clock it moves by `cost` a line, and how many
 *  lines have been drawn. */
const drawing = { advance: (_ms: number) => {}, cost: 0, drawn: 0 };

/** One line the hook draws. */
const DrawnLine = memo(function DrawnLine({ line }: { line: string }): ReactNode {
  drawing.advance(drawing.cost);
  drawing.drawn += 1;
  return (
    <div data-line={line} data-node-row="">
      {line}
    </div>
  );
});

/** The lines drawn through the hook, each top it reads counted into `reads`. */
function Lines({
  anchor,
  focused,
  lines,
  reads,
}: {
  anchor: string;
  focused?: string;
  lines: readonly string[];
  reads: string[];
}): ReactNode {
  const box = useRef<HTMLDivElement | null>(null);
  const measure = useCallback(
    (key: string) => {
      reads.push(key);
      return box.current?.querySelector(`[data-line="${key}"]`)?.getBoundingClientRect().top;
    },
    [reads]
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

/** Has each line's drawing cost `cost` ms on a clock by hand, so a slice draws some of the rows
 *  and not all; answers how to put the clock back. */
function rowsCost(cost: number): () => void {
  const clock = clockByHand();
  drawing.advance = clock.advance;
  drawing.cost = cost;
  return () => {
    drawing.advance = () => {};
    drawing.cost = 0;
    clock.restore();
  };
}

test("a slice that draws rows below the row the reader reads alone reads no row's top", async () => {
  const layout = layoutByHand();
  const tasks = tasksByHand();
  restores.push(layout.restore, tasks.restore, rowsCost(7));
  const reads: string[] = [];
  const view = render(<Lines anchor="l00" lines={LINES} reads={reads} />);
  try {
    expect(reads.length, "the first drawing read no row's top").toBeGreaterThan(0);
    for (let ran = 0; ran < LINES.length && tasks.pending() > 0; ran += 1) {
      reads.length = 0;
      await tasks.runNext();
      expect(reads, `slice ${ran + 1}, which drew rows below the first alone, read tops`).toEqual(
        []
      );
    }
    expect(linesDrawn(view.container), "the slices left rows not drawn").toEqual(LINES);
  } finally {
    view.unmount();
  }
});

test("a slice that draws rows above the row the reader reads reads that row's top alone", async () => {
  const layout = layoutByHand();
  const tasks = tasksByHand();
  restores.push(layout.restore, tasks.restore, rowsCost(7));
  const reads: string[] = [];
  const view = render(<Lines anchor="l30" focused="l30" lines={LINES} reads={reads} />);
  try {
    const anchor = () => view.container.querySelector('[data-line="l30"]');
    const stood = layout.topOf(anchor());
    let drewAbove = false;
    for (let ran = 0; ran < LINES.length && tasks.pending() > 0; ran += 1) {
      reads.length = 0;
      const before = linesDrawn(view.container).indexOf("l30");
      await tasks.runNext();
      const above = linesDrawn(view.container).indexOf("l30") > before;
      drewAbove ||= above;
      expect(
        reads.filter((key) => key !== "l30"),
        `slice ${ran + 1} read the top of a row other than the one the reader reads`
      ).toEqual([]);
      if (!above) {
        expect(reads, `slice ${ran + 1}, which drew no row above the anchor, read tops`).toEqual(
          []
        );
      }
      expect(layout.topOf(anchor()), `slice ${ran + 1} moved the anchor`).toBe(stood);
    }
    expect(drewAbove, "no slice drew a row above the anchor").toBe(true);
    expect(linesDrawn(view.container), "the slices left rows not drawn").toEqual(LINES);
  } finally {
    view.unmount();
  }
});

test("a scroll the reader makes has the next commit read the row they read, and later commits that row alone", async () => {
  const layout = layoutByHand();
  const tasks = tasksByHand();
  restores.push(layout.restore, tasks.restore, rowsCost(7));
  const reads: string[] = [];
  const view = render(<Lines anchor="l30" lines={LINES} reads={reads} />);
  try {
    // The slices draw the rows below the anchor first; the reader scrolls once they have.
    while (tasks.pending() > 0 && !linesDrawn(view.container).includes("l59")) {
      await tasks.runNext();
    }
    expect(linesDrawn(view.container), "a slice drew every row above the anchor").not.toContain(
      "l00"
    );
    const anchor = view.container.querySelector('[data-line="l30"]');
    layout.scrollTo((layout.topOf(anchor) ?? 0) + layout.scrolled() + 250);
    act(() => {
      window.dispatchEvent(new Event("scroll"));
    });
    const row = view.container.querySelector('[data-line="l33"]');
    const stood = layout.topOf(row);
    expect(stood, "the first row in the window after the scroll").toBe(50);
    reads.length = 0;
    await tasks.runNext();
    expect(reads, "the commit after the reader's scroll read the rows again").toContain("l33");
    for (let ran = 0; ran < LINES.length && tasks.pending() > 0; ran += 1) {
      reads.length = 0;
      await tasks.runNext();
      expect(
        reads.filter((key) => key !== "l33"),
        `slice ${ran + 2} after the scroll read a row other than the one the reader reads`
      ).toEqual([]);
    }
    expect(linesDrawn(view.container), "the slices left rows not drawn").toEqual(LINES);
    expect(layout.topOf(row), "the rows drawn above moved the row the reader reads").toBe(stood);
  } finally {
    view.unmount();
  }
});

test("no commit of a slice draws more rows than COMMIT_MS covers by its costliest row", async () => {
  const layout = layoutByHand();
  const tasks = tasksByHand();
  const clock = clockByHand();
  restores.push(layout.restore, tasks.restore, clock.restore, () => {
    drawing.advance = () => {};
    drawing.cost = 0;
  });
  drawing.advance = clock.advance;
  for (const cost of [3, 7, 12]) {
    drawing.cost = cost;
    // The lines each commit drew, by the commits React reports.
    let counted = drawing.drawn;
    const commits: number[] = [];
    const view = render(
      <Profiler
        id="lines"
        onRender={() => {
          commits.push(drawing.drawn - counted);
          counted = drawing.drawn;
        }}
      >
        <Lines anchor="l00" lines={LINES} reads={[]} />
      </Profiler>
    );
    try {
      for (let ran = 0; ran < LINES.length && tasks.pending() > 0; ran += 1) {
        commits.length = 0;
        const started = clock.now();
        await tasks.runNext();
        for (const [index, lines] of commits.entries()) {
          expect(
            lines * cost,
            `commit ${index + 1} of slice ${ran + 1}, of rows costing ${cost} ms, drew past ${COMMIT_MS} ms`
          ).toBeLessThanOrEqual(Math.max(COMMIT_MS, cost));
        }
        expect(clock.now() - started, `slice ${ran + 1} ran to ${SLICE_LIMIT_MS} ms`).toBeLessThan(
          SLICE_LIMIT_MS
        );
      }
      expect(linesDrawn(view.container), `rows costing ${cost} ms were not all drawn`).toEqual(
        LINES
      );
    } finally {
      view.unmount();
    }
  }
});

test("a first drawing from the first row draws the rows to the window's foot, and the first row alone where the rows stand below it", () => {
  const tasks = tasksByHand();
  restores.push(tasks.restore);
  // The rows start below the window's foot, as the outline does under the children's section at
  // 390 px: the first commit draws the first row alone, and no fill draws more.
  const below = layoutByHand({ top: 1_000 });
  const view = render(<Lines anchor="l00" lines={LINES} reads={[]} />);
  try {
    expect(linesDrawn(view.container), "rows below the window's foot were drawn at once").toEqual([
      "l00",
    ]);
  } finally {
    view.unmount();
    below.restore();
  }
  // The rows start 200 px down a window 600 px tall: the rows drawn reach its foot, and none stands
  // a screen below the first.
  const inside = layoutByHand({ top: 200 });
  const again = render(<Lines anchor="l00" lines={LINES} reads={[]} />);
  try {
    const tops = linesDrawn(again.container).map(
      (line) => inside.topOf(again.container.querySelector(`[data-line="${line}"]`)) ?? Number.NaN
    );
    expect(
      Math.max(...tops),
      "the rows drawn do not reach the window's foot"
    ).toBeGreaterThanOrEqual(600);
    expect(
      tops.filter((top) => top >= 200 + 600),
      "a row a screen below the first was drawn at once"
    ).toEqual([]);
  } finally {
    again.unmount();
    inside.restore();
  }
});

test("pending input ends a slice after its current commit and every row still draws", async () => {
  const layout = layoutByHand();
  const tasks = tasksByHand();
  restores.push(layout.restore, tasks.restore, rowsCost(7));
  let pending = true;
  const scheduling = Object.getOwnPropertyDescriptor(navigator, "scheduling");
  Object.defineProperty(navigator, "scheduling", {
    configurable: true,
    value: { isInputPending: () => pending },
  });
  restores.push(() => {
    if (scheduling === undefined) Reflect.deleteProperty(navigator, "scheduling");
    else Object.defineProperty(navigator, "scheduling", scheduling);
  });
  const view = render(<Lines anchor="l00" lines={LINES} reads={[]} />);
  try {
    const drawn = linesDrawn(view.container).length;
    await tasks.runNext();
    expect(linesDrawn(view.container).length).toBe(drawn + 1);
    expect(tasks.pending()).toBe(1);
    pending = false;
    await tasks.runAll();
    expect(linesDrawn(view.container)).toEqual(LINES);
  } finally {
    view.unmount();
  }
});

test("while the slices are held no slice draws a row, and once they are let go every row draws", async () => {
  const layout = layoutByHand();
  const tasks = tasksByHand();
  restores.push(layout.restore, tasks.restore, rowsCost(7), () => holdSlices(false));
  const view = render(<Lines anchor="l00" lines={LINES} reads={[]} />);
  try {
    const drawn = linesDrawn(view.container);
    holdSlices(true);
    await tasks.runAll();
    expect(linesDrawn(view.container), "a slice drew a row while the slices were held").toEqual(
      drawn
    );
    holdSlices(false);
    await tasks.runAll();
    expect(linesDrawn(view.container), "the slices left rows not drawn").toEqual(LINES);
  } finally {
    view.unmount();
  }
});

test("browser scroll anchoring during a slice above the reference does not double-scroll the reference row", async () => {
  const layout = layoutByHand();
  const tasks = tasksByHand();
  restores.push(layout.restore, tasks.restore, rowsCost(7));
  // Simulate Blink's scroll anchoring adjusting `window.scrollY` by +32 px during the first
  // layout read after a slice inserts rows above the anchor.
  let anchorShift = 0;
  let beforeCount = 0;
  function AnchoredLines({ lines }: { lines: readonly string[] }): ReactNode {
    const box = useRef<HTMLDivElement | null>(null);
    const measure = useCallback((key: string) => {
      const count = box.current?.querySelectorAll("[data-line]").length ?? 0;
      if (anchorShift !== 0 && count > beforeCount) {
        layout.scrollTo(layout.scrolled() + anchorShift);
        anchorShift = 0;
      }
      return box.current?.querySelector(`[data-line="${key}"]`)?.getBoundingClientRect().top;
    }, []);
    const { drawn } = useDrawnInSlices({
      anchor: "l59",
      focused: "l59",
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
  const view = render(<AnchoredLines lines={LINES} />);
  try {
    const anchor = () => view.container.querySelector('[data-line="l59"]');
    layout.scrollTo((layout.topOf(anchor()) ?? 0) + layout.scrolled() - 400);
    expect(layout.topOf(anchor())).toBe(400);
    // Trigger scroll-anchor adjustment on the layout read inside `keepReference` once slice 1
    // inserts rows above the anchor.
    beforeCount = view.container.querySelectorAll("[data-line]").length;
    anchorShift = 32;
    await tasks.runNext();
    expect(
      layout.topOf(anchor()),
      "keepReference double-scrolled on top of the browser's scroll anchor"
    ).toBe(400);
  } finally {
    view.unmount();
  }
});

test("after the reader acts with every row drawn, a fold or short unfold reads no row's top", async () => {
  const layout = layoutByHand();
  const tasks = tasksByHand();
  restores.push(layout.restore, tasks.restore);
  const reads: string[] = [];
  const view = render(<Lines anchor="l00" focused="l00" lines={LINES} reads={reads} />);
  try {
    await tasks.runAll();
    expect(linesDrawn(view.container)).toEqual(LINES);
    // Reader acts (pointerdown), then folds 3 rows and unfolds them again.
    act(() => {
      window.dispatchEvent(new Event("pointerdown"));
    });
    reads.length = 0;
    const folded = LINES.filter((line) => line !== "l01" && line !== "l02" && line !== "l03");
    view.rerender(<Lines anchor="l00" focused="l00" lines={folded} reads={reads} />);
    expect(reads, "folding rows after the reader acted read a row's top").toEqual([]);
    view.rerender(<Lines anchor="l00" focused="l00" lines={LINES} reads={reads} />);
    expect(reads, "unfolding a few rows after the reader acted read a row's top").toEqual([]);
  } finally {
    view.unmount();
  }
});
