import { afterEach, beforeAll, beforeEach, expect, jest, spyOn, test } from "bun:test";
import { act, render, waitFor } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { memo, Profiler, type ReactNode, useCallback, useRef } from "react";

import { pageFocusedRow } from "../../theme/classes";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { type RowSlices, SLICE_LIMIT_MS, SLICE_MS, useDrawnInSlices } from "./drawn-in-slices";
import {
  BAND_PX,
  type ClockByHand,
  clockByHand,
  commentsUnder,
  drawOutline,
  idsOf,
  type LayoutByHand,
  type LayoutOptions,
  layoutByHand,
  outlineRow,
  outlineRows,
  SCREEN_PX,
  type TasksByHand,
  tasksByHand,
} from "./slices-fixtures";
import { focusedRow } from "./slots/stage-fixtures";

/**
 * The rows drawn in slices (`drawn-in-slices.ts`), one case for each row of the plan's state
 * table "The rows drawn in slices". The rows of rows 1, 2, 3, 11 and 12 are lines a case draws
 * through the hook alone; those of rows 4 to 10 are the outline's (`IssueTree.tsx`, whose rows
 * `TreeRows.tsx` draws through the hook), since what they hold is what the outline's keys, its
 * address, the Contents list, `z`, a fold and a page's control do with rows not drawn yet.
 *
 * happy-dom lays nothing out, so every row would read as no height and every row would be drawn
 * at once: the page is laid out by hand (`layoutByHand`), each row `ROW_PX` tall in a window
 * `SCREEN_PX` tall. Each slice is a task the browser's `scheduler.postTask` runs, which a case
 * runs by hand (`tasksByHand`), and a slice's length is read on a clock by hand that each line's
 * drawing moves by what it costs (`clockByHand`).
 */

beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** Every stand-in a case made, put back after it. */
let restores: (() => void)[] = [];
beforeEach(() => {
  restores = [];
});
afterEach(() => {
  for (const restore of restores.reverse()) restore();
  jest.useRealTimers();
});

/** The page laid out by hand, its tasks run by hand, each put back after the case. */
function byHand(layout: LayoutOptions = {}): { layout: LayoutByHand; tasks: TasksByHand } {
  const laid = layoutByHand(layout);
  const tasks = tasksByHand();
  restores.push(laid.restore, tasks.restore);
  return { layout: laid, tasks };
}

/** One line the hook draws: its key, and what drawing it costs on the clock by hand. */
interface Line {
  readonly cost?: number;
  readonly key: string;
  readonly words?: string;
}

/** `count` lines, `l00` on, each costing `cost` ms to draw. */
function linesOf(count: number, cost = 0): Line[] {
  return Array.from({ length: count }, (_unused, at) => ({
    cost,
    key: `l${String(at).padStart(2, "0")}`,
  }));
}

/** What the lines' drawing reads and writes: the clock it moves, the number of times each line
 *  was drawn, and the order in which each was first drawn. */
const drawing: {
  advance: (ms: number) => void;
  readonly counts: Map<string, number>;
} = { advance: () => {}, counts: new Map() };

const DrawnLine = memo(function DrawnLine({ line }: { line: Line }): ReactNode {
  drawing.advance(line.cost ?? 0);
  drawing.counts.set(line.key, (drawing.counts.get(line.key) ?? 0) + 1);
  return (
    <div data-line={line.key} data-node-row="">
      {line.words ?? line.key}
    </div>
  );
});

/** The key the hook reads a line by. */
const keyOfLine = (line: Line): string => line.key;

/** The lines drawn through the hook, with what the hook hands back for keys to draw up to a row
 *  (`held.slices`). */
function Lines({
  anchor,
  focused,
  held,
  lines,
}: {
  anchor?: string;
  focused?: string;
  held: { slices?: RowSlices };
  lines: readonly Line[];
}): ReactNode {
  const box = useRef<HTMLDivElement | null>(null);
  const measure = useCallback(
    (key: string) =>
      box.current?.querySelector(`[data-line="${key}"]`)?.getBoundingClientRect().top,
    []
  );
  const { drawn, slices } = useDrawnInSlices({
    anchor,
    focused,
    keyOf: keyOfLine,
    measure,
    rows: lines,
  });
  held.slices = slices;
  return (
    <div ref={box}>
      {drawn.map((line) => (
        <DrawnLine key={line.key} line={line} />
      ))}
    </div>
  );
}

/** The keys of the lines drawn in `container`, in order. */
function linesDrawn(container: HTMLElement): string[] {
  return [...container.querySelectorAll<HTMLElement>("[data-line]")].map(
    (line) => line.dataset.line ?? ""
  );
}

/** Whether `drawn` is one run of `lines` with no line left out between its first and its last. */
function oneRun(lines: readonly Line[], drawn: readonly string[]): boolean {
  const from = lines.findIndex((line) => line.key === drawn[0]);
  return (
    from !== -1 &&
    lines
      .slice(from, from + drawn.length)
      .map(keyOfLine)
      .join() === drawn.join()
  );
}

// Row 1. No rows while the reads that place them are out; once they answer, one commit draws the
// anchor's screen: the anchor and the rows that fill one screen above it and one below, and none
// past them. Where the rows are shorter than the guess of that commit, the rows that fill the
// screen are drawn in the same task, before the browser could paint, and no slice runs first.
test("row 1: once the reads that place the rows answer, one commit draws the anchor and a screen above and below it", () => {
  const { layout, tasks } = byHand();
  let commits = 0;
  const held = {};
  const draw = (lines: readonly Line[]) => (
    <Profiler
      id="lines"
      onRender={() => {
        commits += 1;
      }}
    >
      <Lines anchor="l30" held={held} lines={lines} />
    </Profiler>
  );
  const lines = linesOf(60);
  const view = render(draw([]));
  try {
    expect(linesDrawn(view.container), "a row was drawn before the reads answered").toEqual([]);
    commits = 0;
    view.rerender(draw(lines));
    const drawn = linesDrawn(view.container);
    expect(commits, "the anchor's screen was drawn in more than one commit").toBe(1);
    const top = (key: string | undefined) =>
      layout.topOf(view.container.querySelector(`[data-line="${key}"]`)) ?? Number.NaN;
    expect(drawn, "the anchor was not drawn").toContain("l30");
    expect(oneRun(lines, drawn), `the first drawing left a gap: ${drawn.join()}`).toBe(true);
    expect(
      top("l30") - top(drawn[0]),
      "the rows drawn above the anchor fill no screen"
    ).toBeGreaterThanOrEqual(SCREEN_PX);
    expect(
      top(drawn.at(-1)) - top("l30"),
      "the rows drawn below the anchor fill no screen"
    ).toBeGreaterThanOrEqual(SCREEN_PX);
    expect(drawn, "the first drawing drew the first row, two screens above").not.toContain("l00");
    expect(drawn, "the first drawing drew the last row, two screens below").not.toContain("l59");
    expect(tasks.pending(), "no slice was asked for the rows left").toBe(1);
  } finally {
    view.unmount();
  }

  // Rows of 40 px, which the first commit's guess of a row's height does not fill a screen with:
  // the rows that fill it are drawn in the same task.
  const short = layoutByHand({ height: () => 40 });
  restores.push(short.restore);
  const again = render(<Lines anchor="l30" held={held} lines={[]} />);
  try {
    again.rerender(<Lines anchor="l30" held={held} lines={lines} />);
    const drawn = linesDrawn(again.container);
    const top = (key: string | undefined) =>
      short.topOf(again.container.querySelector(`[data-line="${key}"]`)) ?? Number.NaN;
    expect(
      top("l30") - top(drawn[0]),
      "rows of 40 px drawn above the anchor fill no screen"
    ).toBeGreaterThanOrEqual(SCREEN_PX);
    expect(
      top(drawn.at(-1)) - top("l30"),
      "rows of 40 px drawn below the anchor fill no screen"
    ).toBeGreaterThanOrEqual(SCREEN_PX);
    expect(drawn.length, "rows of 40 px were drawn whole at once").toBeLessThan(lines.length);
  } finally {
    again.unmount();
  }
});

/** Runs the slices one at a time until none is asked for, and answers each one's length on the
 *  clock by hand, with the lines drawn after each. */
async function eachSlice(
  tasks: TasksByHand,
  clock: ClockByHand,
  container: HTMLElement,
  lines: readonly Line[]
): Promise<{ readonly drawn: readonly string[]; readonly length: number }[]> {
  const slices: { readonly drawn: readonly string[]; readonly length: number }[] = [];
  for (let ran = 0; ran < lines.length && tasks.pending() > 0; ran += 1) {
    const started = clock.now();
    await tasks.runNext();
    const drawn = linesDrawn(container);
    slices.push({ drawn, length: clock.now() - started });
    if (drawn.length < lines.length) {
      expect(tasks.pending(), "a slice that left rows to draw asked for no next one").toBe(1);
    }
  }
  return slices;
}

// Row 2. Each slice is a task that draws the rows after the drawn ones, then the rows before
// them, until 60 ms of wall time is spent, and never past 100 ms; the next slice is asked for at
// once, so no second without a task over 50 ms passes until the last row is in the document.
test("row 2: each slice draws the rows after the drawn ones, then those before, for 60 ms and never past 100, and asks for the next at once", async () => {
  const { tasks } = byHand();
  const clock = clockByHand();
  restores.push(clock.restore);
  drawing.advance = clock.advance;
  restores.push(() => {
    drawing.advance = () => {};
  });
  // Rows that cost 7 ms each: a slice draws as many as fit in 60 ms, plus the one that crosses it.
  const lines = linesOf(60, 7);
  const view = render(<Lines anchor="l30" held={{}} lines={lines} />);
  try {
    const first = linesDrawn(view.container);
    const below = lines.findIndex((line) => line.key === first.at(-1));
    const slices = await eachSlice(tasks, clock, view.container, lines);
    expect(slices.at(-1)?.drawn, "the slices ended with rows not drawn").toEqual(
      lines.map(keyOfLine)
    );
    for (const [at, slice] of slices.entries()) {
      expect(oneRun(lines, slice.drawn), `slice ${at + 1} left a gap`).toBe(true);
      const after = slice.drawn.at(-1) === lines.at(-1)?.key;
      const above = slice.drawn[0] !== first[0];
      expect(
        !above || after,
        `slice ${at + 1} drew a row above the first screen while rows below it were not drawn`
      ).toBe(true);
      if (at < slices.length - 1) {
        expect(slice.length, `slice ${at + 1} ended before 60 ms`).toBeGreaterThanOrEqual(SLICE_MS);
      }
      expect(slice.length, `slice ${at + 1} ran to 100 ms`).toBeLessThan(SLICE_LIMIT_MS);
    }
    expect(below, "the first drawing drew the last row").toBeLessThan(lines.length - 1);
    // The slices' lengths, which the report of this change states.
    expect(slices.map((slice) => slice.length)).toEqual([63, 63, 63, 63, 63]);
  } finally {
    view.unmount();
  }

  // Rows that cost 45 ms and 55 ms each: a slice that would pass 100 ms with its next row ends
  // without it.
  for (const cost of [45, 55]) {
    const costly = linesOf(30, cost);
    const drawn = render(<Lines anchor="l00" held={{}} lines={costly} />);
    try {
      const slices = await eachSlice(tasks, clock, drawn.container, costly);
      for (const [at, slice] of slices.entries()) {
        expect(
          slice.length,
          `slice ${at + 1} of rows costing ${cost} ms ran to 100 ms`
        ).toBeLessThan(SLICE_LIMIT_MS);
        expect(
          slice.length,
          `slice ${at + 1} of rows costing ${cost} ms was no task longer than 50 ms`
        ).toBeGreaterThan(50);
      }
      expect(slices.at(-1)?.drawn, `rows costing ${cost} ms were not all drawn`).toEqual(
        costly.map(keyOfLine)
      );
    } finally {
      drawn.unmount();
    }
  }
});

// Row 3. Rows drawn above the anchor, by a slice or by a read that answers, leave it where it
// stood: the commit's layout effect scrolls by how far it moved.
test("row 3: rows drawn above the anchor, by a slice or by a read that answers, leave it where it stood", async () => {
  const { layout, tasks } = byHand();
  const lines = linesOf(60);
  const draw = (drawn: readonly Line[]) => (
    <Lines anchor="l30" focused="l30" held={{}} lines={drawn} />
  );
  const view = render(draw(lines));
  try {
    const anchor = () => view.container.querySelector('[data-line="l30"]');
    // The reader's window, 100 px above the anchor.
    layout.scrollTo((layout.topOf(anchor()) ?? 0) + layout.scrolled() - 100);
    expect(layout.topOf(anchor())).toBe(100);
    for (let ran = 0; ran < lines.length && tasks.pending() > 0; ran += 1) {
      await tasks.runNext();
      expect(layout.topOf(anchor()), `slice ${ran + 1} moved the anchor`).toBe(100);
    }
    expect(linesDrawn(view.container), "the slices left rows not drawn").toEqual(
      lines.map(keyOfLine)
    );
    expect(
      layout.scrolls.some((by) => by > 0),
      "no slice drew a row above the anchor"
    ).toBe(true);
    // A read answers with one more line above the anchor.
    const read = [...lines.slice(0, 30), { key: "l29b" }, ...lines.slice(30)];
    view.rerender(draw(read));
    expect(linesDrawn(view.container), "the read's new line was not drawn").toContain("l29b");
    expect(layout.topOf(anchor()), "the read's new line above the anchor moved it").toBe(100);
  } finally {
    view.unmount();
  }
});

/** The outline's tree over 40 comments, `c00` to `c39`, laid out by hand, with the comments
 *  each of `replies` names holding that many replies. */
function fortyComments(replies: Readonly<Record<string, number>> = {}) {
  return commentsUnder(idsOf(40), replies);
}

// Row 4. While rows are still to be drawn, `j` on the last row drawn draws the next row in the
// same commit, and that row takes the focus and its ring.
test("row 4: j on the last row drawn draws the next row and gives it the focus ring", async () => {
  const { tasks } = byHand();
  const user = userEvent.setup();
  const { view } = drawOutline({ root: fortyComments() });
  try {
    await waitFor(() => expect(focusedRow(), "the page opened on no row").toBe("c00"));
    const first = outlineRows(view.container);
    expect(first, "the first drawing drew every row").not.toContain("c39");
    for (let at = 1; at < first.length; at += 1) await user.keyboard("j");
    const last = first.at(-1) ?? "";
    expect(focusedRow(), "j did not reach the last row drawn").toBe(last);
    const next = idsOf(40)[idsOf(40).indexOf(last) + 1] ?? "";
    expect(outlineRow(view.container, next), "the next row was drawn already").toBeNull();
    await user.keyboard("j");
    expect(focusedRow(), "j on the last row drawn did not move to the next row").toBe(next);
    expect(
      outlineRow(view.container, next)?.querySelector("[data-node-card]")?.className,
      "the next row took no focus ring"
    ).toContain(pageFocusedRow);
    expect(tasks.pending(), "the rows were all drawn by then").toBe(1);
  } finally {
    view.unmount();
  }
});

// Row 5. While rows are still to be drawn, `k` on the first row drawn, with rows above it not
// drawn, draws the row above in the same commit, and that row takes the focus.
test("row 5: k on the first row drawn, with rows above it not drawn, draws the row above and gives it the focus", async () => {
  const { tasks } = byHand();
  const user = userEvent.setup();
  const root = fortyComments();
  const named = root.children[30];
  if (named === undefined) throw new Error("the tree holds no 31st comment");
  const { view } = drawOutline({ reveal: [root, named], root });
  try {
    await waitFor(() => expect(focusedRow(), "the address landed on no row").toBe("c30"));
    const first = outlineRows(view.container);
    expect(first, "the first drawing drew the first row").not.toContain("c00");
    const top = first[0] ?? "";
    for (let at = first.indexOf("c30"); at > 0; at -= 1) await user.keyboard("k");
    expect(focusedRow(), "k did not reach the first row drawn").toBe(top);
    const above = idsOf(40)[idsOf(40).indexOf(top) - 1] ?? "";
    expect(outlineRow(view.container, above), "the row above was drawn already").toBeNull();
    await user.keyboard("k");
    expect(focusedRow(), "k on the first row drawn did not move to the row above").toBe(above);
    expect(tasks.pending(), "the rows were all drawn by then").toBe(1);
  } finally {
    view.unmount();
  }
});

/** Whether the row of `id` in `container` stands in the window under the band. */
function underTheBand(layout: LayoutByHand, container: HTMLElement, id: string): boolean {
  const top = layout.topOf(outlineRow(container, id));
  return top !== undefined && top >= BAND_PX && top < SCREEN_PX;
}

// Row 6. While rows are still to be drawn, an address that names a row not drawn draws every row
// up to it in one commit, whose layout effect brings it under the header, and the row takes the
// focus in the frame after, as an address's row does at the base (`IssueTree.tsx`'s `landOnRow`).
test("row 6: an address that names a row not drawn draws every row up to it and lands on it under the header", async () => {
  const { layout, tasks } = byHand({ band: true });
  const root = fortyComments();
  const named = root.children[30];
  if (named === undefined) throw new Error("the tree holds no 31st comment");
  const outline = drawOutline({ root });
  const { view } = outline;
  try {
    await waitFor(() => expect(focusedRow(), "the page opened on no row").toBe("c00"));
    expect(outlineRow(view.container, "c30"), "the named row was drawn already").toBeNull();
    act(() => outline.redraw({ addressKey: "followed", reveal: [root, named] }));
    const drawn = outlineRows(view.container);
    expect(drawn.slice(0, 31), "the address drew the rows up to its own with one left out").toEqual(
      idsOf(31)
    );
    expect(
      underTheBand(layout, view.container, "c30"),
      "the commit that drew the named row left it off the screen"
    ).toBe(true);
    await waitFor(() => expect(focusedRow(), "the named row took no focus").toBe("c30"));
    expect(
      underTheBand(layout, view.container, "c30"),
      "the named row is not under the header"
    ).toBe(true);
    expect(tasks.pending(), "the rows were all drawn by then").toBe(1);
  } finally {
    view.unmount();
  }
});

// Row 7. While rows are still to be drawn, a Contents entry that names a row not drawn does as an
// address does.
test("row 7: a Contents entry that names a row not drawn draws every row up to it and lands on it", async () => {
  const { layout, tasks } = byHand({ band: true });
  const outline = drawOutline({ root: fortyComments() });
  const { view } = outline;
  try {
    await waitFor(() => expect(focusedRow(), "the page opened on no row").toBe("c00"));
    expect(outlineRow(view.container, "c30"), "the named row was drawn already").toBeNull();
    await outline.ask("c30");
    expect(
      outlineRows(view.container).slice(0, 31),
      "the entry drew the rows up to its own with one left out"
    ).toEqual(idsOf(31));
    expect(focusedRow(), "the entry's row took no focus").toBe("c30");
    expect(
      underTheBand(layout, view.container, "c30"),
      "the entry's row is not under the header"
    ).toBe(true);
    expect(tasks.pending(), "the rows were all drawn by then").toBe(1);
  } finally {
    view.unmount();
  }
});

/** Folds or unfolds the row of `id` in `container` by a press on its fold control. */
async function pressFold(user: UserEvent, container: HTMLElement, id: string): Promise<void> {
  const control = outlineRow(container, id)?.querySelector<HTMLElement>("button[data-node-bullet]");
  if (control == null) throw new Error(`the row ${id} draws no fold control`);
  await user.click(control);
}

// Row 8. While rows are still to be drawn, `z` unfolds every row the reader folded, a row drawn
// at once and one drawn late alike, and the rows they hold are drawn as any rows are.
test("row 8: z unfolds every row the reader folded, drawn at once or late", async () => {
  const { tasks } = byHand();
  const user = userEvent.setup();
  const root = fortyComments({ c00: 3, c35: 3 });
  const late = root.children[35];
  if (late === undefined) throw new Error("the tree holds no 36th comment");
  const outline = drawOutline({ root });
  const { view } = outline;
  const expanded = (id: string) =>
    outlineRow(view.container, id)?.getAttribute("aria-expanded") ?? "no row";
  try {
    await waitFor(() => expect(focusedRow(), "the page opened on no row").toBe("c00"));
    expect(outlineRow(view.container, "c35"), "the late row was drawn at once").toBeNull();
    await pressFold(user, view.container, "c00");
    // The late row is drawn by an address that names it, and folded there.
    act(() => outline.redraw({ addressKey: "followed", reveal: [root, late] }));
    await pressFold(user, view.container, "c35");
    expect([expanded("c00"), expanded("c35")], "the two rows are not folded").toEqual([
      "false",
      "false",
    ]);
    expect(tasks.pending(), "the rows were all drawn before z").toBe(1);
    await user.keyboard("z");
    expect([expanded("c00"), expanded("c35")], "z left a row folded").toEqual(["true", "true"]);
    expect(
      outlineRow(view.container, "c00-r000"),
      "z drew no reply of the first row"
    ).not.toBeNull();
    expect(
      outlineRow(view.container, "c35-r000"),
      "z drew no reply of the late row"
    ).not.toBeNull();
    await tasks.runAll();
    expect(outlineRows(view.container), "the slices after z left rows not drawn").toEqual(
      root.children.flatMap((comment) => [comment.id, ...comment.children.map((one) => one.id)])
    );
  } finally {
    view.unmount();
  }
});

// Row 9. While rows are still to be drawn, a fold applies at once, and the slices go on and draw
// no row it hides; its unfold draws the rows it holds, the first screen of them at once.
test("row 9: a fold applies at once, and the slices draw no row it hides", async () => {
  const { tasks } = byHand();
  const user = userEvent.setup();
  const root = commentsUnder(idsOf(10), { c00: 20 });
  const { view } = drawOutline({ root });
  const replies = (root.children[0]?.children ?? []).map((reply) => reply.id);
  try {
    await waitFor(() => expect(focusedRow(), "the page opened on no row").toBe("c00"));
    expect(
      outlineRow(view.container, replies.at(-1) ?? ""),
      "every reply was drawn at once"
    ).toBeNull();
    await pressFold(user, view.container, "c00");
    expect(outlineRow(view.container, "c00")?.getAttribute("aria-expanded")).toBe("false");
    expect(
      outlineRows(view.container).filter((id) => replies.includes(id)),
      "the fold left replies drawn"
    ).toEqual([]);
    await tasks.runAll();
    expect(outlineRows(view.container), "the slices drew a row the fold hides").toEqual(idsOf(10));
    await pressFold(user, view.container, "c00");
    const unfolded = outlineRows(view.container);
    expect(unfolded, "the unfold drew no reply at once").toContain(replies[0] ?? "");
    expect(unfolded, "the unfold drew every reply at once").not.toContain(replies.at(-1) ?? "");
    await tasks.runAll();
    expect(outlineRows(view.container), "the slices left replies not drawn").toEqual([
      "c00",
      ...replies,
      ...idsOf(10).slice(1),
    ]);
  } finally {
    view.unmount();
  }
});

// Row 10. A page's control pressed draws the 50 children behind it, the first screen of them at
// once and the rest in slices, and no row past a page is drawn before its press.
test("row 10: a page's control draws its 50 children, the first screen first, and nothing past it", async () => {
  const { tasks } = byHand();
  const user = userEvent.setup();
  const root = commentsUnder(["t"], { t: 120 });
  const replies = (root.children[0]?.children ?? []).map((reply) => reply.id);
  const { view } = drawOutline({ root });
  try {
    await waitFor(() => expect(focusedRow(), "the page opened on no row").toBe("t"));
    await tasks.runAll();
    expect(outlineRows(view.container), "the first page was not drawn whole").toEqual([
      "t",
      ...replies.slice(0, 50),
    ]);
    const control = view.container.querySelector<HTMLElement>("[data-tree-page] button");
    if (control === null) throw new Error("the outline draws no page's control");
    await user.click(control);
    const pressed = outlineRows(view.container);
    expect(pressed, "the press drew no row of the next page at once").toContain(replies[50] ?? "");
    expect(pressed, "the press drew the next page whole at once").not.toContain(replies[99] ?? "");
    expect(tasks.pending(), "the press asked for no slice").toBe(1);
    await tasks.runAll();
    expect(outlineRows(view.container), "the slices drew past the page pressed").toEqual([
      "t",
      ...replies.slice(0, 100),
    ]);
  } finally {
    view.unmount();
  }
});

// Row 11. The view changing in the rows' place, or the page leaving, ends the slice the rows
// asked for: none runs after, whether the browser holds `scheduler.postTask` or the rows asked
// for a timer.
test("row 11: another view in the rows' place or the page leaving ends the slice asked for", async () => {
  const { tasks } = byHand();
  const view = render(<Lines anchor="l00" held={{}} lines={linesOf(60)} />);
  expect(tasks.pending(), "no slice was asked for").toBe(1);
  view.unmount();
  expect(tasks.pending(), "the page left with its slice still asked for").toBe(0);
  expect(await tasks.runNext(), "a slice ran after the page left").toBe(false);

  // Another view's rows in the same place.
  const other = linesOf(60).map((line) => ({ key: `other-${line.key}` }));
  const changed = render(<Lines anchor="l00" held={{}} lines={linesOf(60)} />);
  try {
    changed.rerender(<Lines anchor="other-l10" held={{}} lines={other} />);
    expect(tasks.pending(), "the first view's slice still stands beside the second's").toBe(1);
    const drawn = linesDrawn(changed.container);
    expect(drawn, "the second view's first screen holds no anchor").toContain("other-l10");
    expect(
      drawn.every((key) => key.startsWith("other-")),
      "a row of the first view stayed"
    ).toBe(true);
    await tasks.runAll();
    expect(linesDrawn(changed.container), "the second view's slices left rows").toEqual(
      other.map(keyOfLine)
    );
  } finally {
    changed.unmount();
  }

  // With no `scheduler.postTask`, the slice is a timer, cleared as the page leaves.
  tasks.restore();
  jest.useFakeTimers();
  const set = spyOn(globalThis, "setTimeout");
  const cleared = spyOn(globalThis, "clearTimeout");
  restores.push(
    () => set.mockRestore(),
    () => cleared.mockRestore()
  );
  drawing.counts.clear();
  const timed = render(<Lines anchor="l00" held={{}} lines={linesOf(60)} />);
  const asked = set.mock.results.flatMap((result, at) =>
    set.mock.calls[at]?.[1] === 0 ? [result.value] : []
  );
  expect(asked.length, "the rows asked for no timer of their slice").toBe(1);
  const drawnBefore = drawing.counts.size;
  timed.unmount();
  expect(
    cleared.mock.calls.map((call) => call[0]),
    "the page left with its slice's timer standing"
  ).toContain(asked[0]);
  act(() => jest.runOnlyPendingTimers());
  expect(drawing.counts.size, "a slice drew rows after the page left").toBe(drawnBefore);
});

// Row 12. With every row drawn, a read that changes one row draws that row again and no other,
// and asks for no slice.
test("row 12: with every row drawn, a read that changes one row draws that row alone again", async () => {
  const { tasks } = byHand();
  const lines = linesOf(60);
  const view = render(<Lines anchor="l00" held={{}} lines={lines} />);
  try {
    await tasks.runAll();
    expect(linesDrawn(view.container)).toEqual(lines.map(keyOfLine));
    drawing.counts.clear();
    const read = lines.map((line) =>
      line.key === "l10" ? { ...line, words: "The tenth line, read again." } : line
    );
    view.rerender(<Lines anchor="l00" held={{}} lines={read} />);
    expect([...drawing.counts.keys()], "a read of one row drew others again").toEqual(["l10"]);
    expect(tasks.pending(), "a read of one row asked for a slice").toBe(0);
  } finally {
    view.unmount();
  }
});
