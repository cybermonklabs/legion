import { afterEach, beforeAll, beforeEach, expect, spyOn, test } from "bun:test";
import { act } from "@testing-library/react";

import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { KEY, node } from "./decisions-filter/decisions-fixtures";
import type { TreeNode } from "./issue-tree";
import { framesByHand } from "./outline-fixtures";
import {
  BAND_PX,
  drawOutline,
  idsOf,
  type LayoutByHand,
  layoutByHand,
  outlineRow,
  outlineRows,
  SCREEN_PX,
  type TasksByHand,
  tasksByHand,
} from "./slices-fixtures";
import { focusedRow } from "./slots/stage-fixtures";

/**
 * The first drawing of the outline's rows and its scroll once its later rows are drawn in slices
 * ("The first drawing and its scroll" of the plan), rows 1, 3, 4 and 5; rows 2 and 6 are
 * `first-drawing-scroll.test.tsx`'s. The rows the page opens on are the first screen drawn: the
 * question that waits on the reader, the node an address names, and the rows that fill a screen
 * above and below it. The commit that draws them brings that row under the header in its layout
 * effect, before any frame. The question takes the focus in the frame after, as at the base, and
 * the node an address names in the frame after the commit that has every row up to it drawn,
 * each with no scroll of its own, which then moves nothing. A read that answers later with rows
 * above the focused row leaves it where it stood, until the reader acts; from then on nothing the
 * page does moves the focus or the scroll.
 *
 * happy-dom lays nothing out, so the page is laid out by hand (`layoutByHand`), every row 100 px
 * tall in a window 600 px tall under a header's band, and the slices are tasks run by hand
 * (`tasksByHand`); the frames are held by hand (`framesByHand`), so a case reads the page before
 * the frame and after it.
 */

beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** Every stand-in a case made, put back after it. */
let restores: (() => void)[] = [];
let layout: LayoutByHand;
let tasks: TasksByHand;
/** Runs every frame asked for and not run yet (`framesByHand`). */
let runFrames: () => Promise<void>;
beforeEach(() => {
  layout = layoutByHand({ band: true });
  tasks = tasksByHand();
  const frames = framesByHand();
  runFrames = () => frames.run();
  restores = [layout.restore, tasks.restore, frames.restore];
});
afterEach(() => {
  for (const restore of restores.reverse()) restore();
});

/** Forty comments, `c00` to `c39`, with an open question that waits on the reader, `q30`, in
 *  place of the 31st where `asking`. */
function issueOf(asking: boolean): TreeNode {
  return node({
    children: idsOf(40).map((id) =>
      asking && id === "c30"
        ? node({
            id: "q30",
            question: true,
            state: "open",
            text: "Which width does the header condense at?",
            type: "ask",
            waiting: true,
          })
        : node({ id, text: `The comment ${id}.`, type: "comment" })
    ),
    id: KEY,
    type: "issue",
  });
}

/** `root` with a comment `c29b` drawn before its 31st row, as a read that answers with it does. */
function withOneAbove(root: TreeNode): TreeNode {
  const above = node({ id: "c29b", text: "A comment the later read brings.", type: "comment" });
  return { ...root, children: [...root.children.slice(0, 30), above, ...root.children.slice(30)] };
}

/** Every focus given to a row from now on, by the row's node id, with its options. */
function watchFocuses(): { readonly given: { id?: string; options?: FocusOptions }[] } {
  const given: { id?: string; options?: FocusOptions }[] = [];
  const real = HTMLElement.prototype.focus;
  const spy = spyOn(HTMLElement.prototype, "focus").mockImplementation(function (
    this: HTMLElement,
    options?: FocusOptions
  ) {
    const id = this.closest<HTMLElement>("[data-node-row]")?.dataset.nodeId;
    given.push({
      ...(id === undefined ? {} : { id }),
      ...(options === undefined ? {} : { options }),
    });
    real.call(this, options);
  });
  restores.push(() => spy.mockRestore());
  return { given };
}

/** Holds the first drawing of `root`'s rows to the row `landsOn`: in the task the reads that place
 *  the rows answered in, with no frame run, the commit draws that row's screen and not the
 *  outline's first and last rows, leaves it under the header and gives it no focus; the frame
 *  after gives it the focus, once and without the browser's scroll, and moves it nowhere. */
async function expectLandedAtFirstDrawing(
  container: HTMLElement,
  landsOn: string,
  focuses: { readonly given: { id?: string; options?: FocusOptions }[] },
  what: string
): Promise<void> {
  const drawn = outlineRows(container);
  expect(drawn, `${what}: the first drawing drew no row of it`).toContain(landsOn);
  expect(drawn, `${what}: the first drawing drew the outline's first row too`).not.toContain("c00");
  expect(drawn, `${what}: the first drawing drew the outline's last row too`).not.toContain("c39");
  const top = layout.topOf(outlineRow(container, landsOn));
  expect(
    top !== undefined && top >= BAND_PX && top < SCREEN_PX,
    `${what}: the first drawing left the row at ${top} px, not under the header's ${BAND_PX} px band`
  ).toBe(true);
  expect(
    focuses.given.filter((focus) => focus.id === landsOn),
    `${what}: the row took the focus in the commit that drew it`
  ).toEqual([]);
  const scrolled = layout.scrolls.length;
  await runFrames();
  expect(focusedRow(), `${what}: the frame after the first drawing gave it no focus`).toBe(landsOn);
  expect(
    focuses.given.filter((focus) => focus.id === landsOn),
    `${what}: the row was focused with the browser's scroll, or more than once`
  ).toEqual([{ id: landsOn, options: { preventScroll: true } }]);
  expect(layout.scrolls.slice(scrolled), `${what}: the focus scrolled the page`).toEqual([]);
  expect(layout.topOf(outlineRow(container, landsOn)), `${what}: the focus moved the row`).toBe(
    top
  );
}

// Row 1. Conversation, a question waiting on the reader: the first commit of rows draws the
// question's screen first, and its layout effect scrolls the question under the header before
// the frame is painted; the frame after gives it the focus with `preventScroll`.
test("row 1: the first commit of rows draws the waiting question's screen and brings it under the header", async () => {
  const root = issueOf(true);
  const outline = drawOutline({ placesSettled: false, root });
  try {
    expect(
      outlineRows(outline.view.container),
      "rows were drawn before the reads answered"
    ).toEqual([]);
    const focuses = watchFocuses();
    act(() => outline.redraw({ placesSettled: true }));
    await expectLandedAtFirstDrawing(
      outline.view.container,
      "q30",
      focuses,
      "the waiting question"
    );
  } finally {
    outline.view.unmount();
  }
});

// Row 3. An address that names a node drawn at the first drawing: the first commit of rows draws
// that node's screen and brings it under the header, as row 1 does for the question, and the node
// takes the focus only in the frame after the commit that has every row up to it drawn, so the
// rows a reader reads above the focus are there once it is: the slices draw them, and the commit
// of each leaves the node where it stood.
test("row 3: the first commit of rows draws the addressed node's screen, and the node takes the focus only once every row up to it is drawn", async () => {
  const root = issueOf(false);
  const named = root.children[30];
  if (named === undefined) throw new Error("the tree holds no 31st comment");
  const outline = drawOutline({ placesSettled: false, reveal: [root, named], root });
  const { container } = outline.view;
  const upToIt = idsOf(31);
  try {
    expect(outlineRows(container), "rows were drawn before the reads answered").toEqual([]);
    const focuses = watchFocuses();
    act(() => outline.redraw({ placesSettled: true }));
    const drawn = outlineRows(container);
    expect(drawn, "the first drawing drew no row of the addressed node").toContain("c30");
    expect(drawn, "the first drawing drew the outline's first row too").not.toContain("c00");
    expect(drawn, "the first drawing drew the outline's last row too").not.toContain("c39");
    const top = layout.topOf(outlineRow(container, "c30"));
    expect(
      top !== undefined && top >= BAND_PX && top < SCREEN_PX,
      `the first drawing left the addressed node at ${top} px, not under the header's ${BAND_PX} px band`
    ).toBe(true);
    // Each frame asked for, then the next slice, until no slice is left: wherever the node holds
    // the focus, every row up to it is drawn, and the frame that gave it the focus scrolled
    // nothing.
    do {
      const scrolled = layout.scrolls.length;
      await runFrames();
      if (focusedRow() === "c30") {
        expect(
          upToIt.filter((id) => outlineRow(container, id) === null),
          "the addressed node took the focus with these rows above it not drawn"
        ).toEqual([]);
        expect(layout.scrolls.slice(scrolled), "the focus scrolled the page").toEqual([]);
        break;
      }
    } while (await tasks.runNext());
    expect(focusedRow(), "the addressed node never took the focus").toBe("c30");
    expect(
      focuses.given.filter((focus) => focus.id === "c30"),
      "the addressed node was focused with the browser's scroll, or more than once"
    ).toEqual([{ id: "c30", options: { preventScroll: true } }]);
    expect(
      layout.topOf(outlineRow(container, "c30")),
      "the rows drawn above the addressed node, or its focus, moved it"
    ).toBe(top);
  } finally {
    outline.view.unmount();
  }
});

// Row 4. A read that answers later with a row above the focused one leaves the focused row where
// it stood (the slices' row 3), while the reader has not acted.
test("row 4: a later read that draws a row above the focused row leaves it where it stood", async () => {
  const root = issueOf(true);
  const outline = drawOutline({ root });
  try {
    await runFrames();
    expect(focusedRow(), "the page opened on no question").toBe("q30");
    const before = layout.topOf(outlineRow(outline.view.container, "q30"));
    act(() => outline.redraw({ root: withOneAbove(root) }));
    expect(
      outlineRows(outline.view.container),
      "the later read's row was not drawn above the question"
    ).toContain("c29b");
    expect(
      layout.topOf(outlineRow(outline.view.container, "q30")),
      "the later read's row moved the focused question"
    ).toBe(before);
    expect(focusedRow(), "the later read moved the focus").toBe("q30");
  } finally {
    outline.view.unmount();
  }
});

// Row 5. Once the reader has pressed, scrolled or typed, a later read moves neither the focus nor
// the scroll: the opening is done, as at the base.
test("row 5: once the reader has pressed, scrolled or typed, a later read moves neither the focus nor the scroll", async () => {
  // Each act is the one event the page reads it by (`useNodeEditing.ts`'s `READER_ACTS`), made on
  // the page itself, so it presses, turns or types on nothing else.
  const acts: readonly (readonly [string, () => Event])[] = [
    ["a press", () => new PointerEvent("pointerdown", { bubbles: true })],
    ["a turn of the wheel", () => new WheelEvent("wheel", { bubbles: true })],
    ["a key", () => new KeyboardEvent("keydown", { bubbles: true, key: "Shift" })],
  ];
  for (const [what, made] of acts) {
    const root = issueOf(true);
    const outline = drawOutline({ root });
    try {
      await runFrames();
      expect(focusedRow(), `before ${what}, the page opened on no question`).toBe("q30");
      act(() => {
        document.body.dispatchEvent(made());
      });
      const scrolled = layout.scrolls.length;
      outline.redraw({ root: withOneAbove(root) });
      await runFrames();
      expect(
        outlineRows(outline.view.container),
        `after ${what}, the later read's row was not drawn`
      ).toContain("c29b");
      expect(focusedRow(), `after ${what}, the later read moved the focus`).toBe("q30");
      expect(layout.scrolls.slice(scrolled), `after ${what}, the later read scrolled`).toEqual([]);
    } finally {
      outline.view.unmount();
    }
  }
});
