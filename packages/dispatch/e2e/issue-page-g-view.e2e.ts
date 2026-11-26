import type { Page } from "@playwright/test";

import { createComment, createIssue, createProject } from "./api";
import {
  DESKTOP,
  longWords,
  nodeRows,
  openIssue,
  PHONE,
  phase,
  plannerActor,
  unfoldEveryBranch,
} from "./issue-page-g";
import { expect, test } from "./test";

/**
 * A node's row on the new issue page, as R11 holds it: nothing changes a row the reader
 * can see. Every row is drawn whole, so no row changes its length once drawn, and a jump of the
 * view lands on rows that hold still: the words the reader was reading stay where they are, when
 * the row holding them is taller than the view and when it stands wholly in view. So this row
 * reads what the reader sees - the words under the middle of the view, and the top of the element
 * that holds them - for 60 frames after each jump, and allows no change at all.
 */

const PROJECT = "VIEW";
const asPlanner = { actor: plannerActor, as: "agent" as const };

/** How many long rows of the planner's the seed writes. */
const ROWS = 45;

/** Where the three rows taller than the view stand among them, and the words each holds: the
 *  middle one is the tallest, and it stands far from the rows drawn at load and from the foot. */
const TALL = new Map<number, number>([
  [12, 1500],
  [24, 2000],
  [36, 1500],
]);

interface TallRowsSeed {
  readonly key: string;
  readonly rows: readonly string[];
  readonly tallest: string;
}

/**
 * One issue whose conversation is the reader's comment and, under it, `ROWS` replies of the
 * planner's: most of 160 words, and three of 1,500 to 2,000 words, which are taller than the view
 * at both widths. Every word is told apart by its row and its place, so the words under one point
 * of the view name where the reader is.
 */
async function seedTallRows(): Promise<TallRowsSeed> {
  await createProject({ key: PROJECT, name: "View" });
  const { key } = await createIssue({
    force: true,
    project: PROJECT,
    title: "A conversation with rows taller than the view",
  });
  const head = await createComment(key, { body: "Each reply under this one is long." });
  const rows: string[] = [];
  let tallest = "";
  for (let at = 0; at < ROWS; at += 1) {
    const words = TALL.get(at) ?? 160;
    const body = longWords(`r${at}w`, words);
    const { id } = await createComment(key, { body, reply_to: head.id }, asPlanner);
    rows.push(id);
    if (words === Math.max(...TALL.values())) tallest = id;
  }
  return { key, rows, tallest };
}

/** Opens issue `key` at `size` on a fresh load, and waits until it draws every row the seed
 *  wrote. */
async function openTallRows(
  page: Page,
  seeded: TallRowsSeed,
  size: { height: number; width: number },
  where: string
): Promise<void> {
  await page.setViewportSize(size);
  await openIssue(page, seeded.key);
  await expect
    .poll(
      async () => {
        await unfoldEveryBranch(page);
        return nodeRows(page).count();
      },
      { message: `${where} the page never drew every row` }
    )
    .toBeGreaterThan(ROWS);
}

/** What the reader saw at the middle of the view after a jump, and for 60 frames after it. */
interface ViewRead {
  /** How many rows stood in view in the task of the jump. */
  readonly inView: number;
  /** The words at the point read at the middle of the view (`jumpAndWatch` says which) in the
   *  task of the jump, and after 60 frames. */
  readonly before: string | null;
  readonly after: string | null;
  /** The most the top of the element at that point moved in those frames. */
  readonly moved: number;
}

/** Jumps the page, as a fast drag of the scrollbar does, either so the middle of row `row`
 *  stands at the middle of the view or to `share` of the page's height, and reads in the same
 *  task, then for 60 frames, the words the reader reads at the middle of the view. The middle can
 *  fall on a row's byline or between two rows, so the point read is the middle of the line of
 *  words that spans the view's middle, or of the line in view nearest it. */
function jumpAndWatch(page: Page, to: { row: string } | { share: number }): Promise<ViewRead> {
  return page.evaluate(async (target): Promise<ViewRead> => {
    if ("row" in target) {
      const row = document.querySelector(`[data-node-row][data-node-id="${target.row}"]`);
      if (row == null) throw new Error(`the row of ${target.row} is not drawn`);
      const box = row.getBoundingClientRect();
      window.scrollTo(0, box.top + window.scrollY + box.height / 2 - window.innerHeight / 2);
    } else {
      window.scrollTo(0, document.documentElement.scrollHeight * target.share);
    }
    const inView = [...document.querySelectorAll("[data-node-row]")].filter((row) => {
      const box = row.getBoundingClientRect();
      return box.bottom > 0 && box.top < window.innerHeight;
    });
    const middle = window.innerHeight / 2;
    // How far a box stands from the middle of the view: 0 when it spans it.
    const distance = (box: DOMRect): number =>
      box.top > middle ? box.top - middle : box.bottom <= middle ? middle - box.bottom : 0;
    // The words box that spans the view's middle, else the one in view nearest it.
    const words = inView
      .map((row) => row.querySelector("[data-node-words]"))
      .filter((found) => found != null)
      .map((element) => ({ box: element.getBoundingClientRect(), element }))
      .sort((a, b) => distance(a.box) - distance(b.box))[0];
    // Its line that spans the middle, else its line nearest it, among the lines drawn inside both
    // the box and the view.
    let line: DOMRect | undefined;
    if (words !== undefined) {
      const walker = document.createTreeWalker(words.element, NodeFilter.SHOW_TEXT);
      const range = document.createRange();
      for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
        if ((node.textContent ?? "").trim() === "") continue;
        range.selectNodeContents(node);
        for (const rect of range.getClientRects()) {
          const inside =
            rect.width > 0 &&
            rect.top >= Math.max(words.box.top, 0) &&
            rect.bottom <= Math.min(words.box.bottom, window.innerHeight);
          if (inside && (line === undefined || distance(rect) < distance(line))) line = rect;
        }
      }
    }
    const x = line === undefined ? window.innerWidth / 2 : line.left + line.width / 2;
    const y = line === undefined ? middle : line.top + line.height / 2;
    const caretDocument = document as Document & {
      caretPositionFromPoint?: (
        x: number,
        y: number
      ) => { offsetNode: Node; offset: number } | null;
    };
    const wordsAtMiddle = (): string | null => {
      const position = caretDocument.caretPositionFromPoint?.(x, y);
      const range = position == null ? document.caretRangeFromPoint?.(x, y) : null;
      const node = position?.offsetNode ?? range?.startContainer;
      const offset = position?.offset ?? range?.startOffset ?? 0;
      if (node == null || node.nodeType !== Node.TEXT_NODE) return null;
      const text = node.textContent ?? "";
      return text.slice(Math.max(0, offset - 24), offset + 24);
    };
    const held = document.elementFromPoint(x, y);
    const top = held?.getBoundingClientRect().top ?? 0;
    const read = { before: wordsAtMiddle(), inView: inView.length };
    let moved = 0;
    for (let at = 0; at < 60; at += 1) {
      const next = Promise.withResolvers<void>();
      requestAnimationFrame(() => next.resolve());
      await next.promise;
      if (held?.isConnected)
        moved = Math.max(moved, Math.abs(held.getBoundingClientRect().top - top));
    }
    return { ...read, after: wordsAtMiddle(), moved };
  }, to);
}

/** Fails unless the jump landed on rows in view, with words under the middle of the view, and
 *  records a failure, without stopping the row, when those words or the element that holds them
 *  moved, so one run reports every jump. */
function expectViewHeld(read: ViewRead, where: string): void {
  expect(read.inView, `${where} the jump landed on no row in view`).toBeGreaterThan(0);
  expect(read.before, `${where} no words stand at the middle of the view`).not.toBeNull();
  expect
    .soft(
      read.after,
      `${where} the words at the middle of the view changed from "${read.before}" to "${read.after}"`
    )
    .toBe(read.before);
  expect
    .soft(read.moved, `${where} the words at the middle of the view moved ${read.moved} px`)
    .toBeLessThanOrEqual(1);
}

// As the root tester measured it on a copy of a real issue: after each jump, on a fresh load at
// each width, the words under the middle of the view and the element that holds them stay where
// they stood for 60 frames. Three jumps at each width: a landing at the middle of the tallest row,
// which crosses the view's top; a jump from the top to the page's middle; and a jump from the foot
// up to a third of the way down. It carries `@phone` for the reason the long-jump row of
// `e2e/issue-page-g.e2e.ts` gives: the phone project lays the page out at three device pixels to
// the CSS pixel.
// SLOW(>=2s): forty-five long rows seeded, three of them taller than the view, loaded six times at
// two widths, each load waited out until its rows are drawn.
test("the words at the middle of the view hold still after a long jump or a landing inside the tallest row @phone", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  await phase(browser, seedTallRows, async (page, seeded) => {
    for (const size of [DESKTOP, PHONE]) {
      const at = `at ${size.width} px`;

      await openTallRows(page, seeded, size, at);
      expectViewHeld(
        await jumpAndWatch(page, { row: seeded.tallest }),
        `${at}, a landing inside the tallest row:`
      );

      await openTallRows(page, seeded, size, at);
      expectViewHeld(
        await jumpAndWatch(page, { share: 0.5 }),
        `${at}, a jump from the top to the middle:`
      );

      await openTallRows(page, seeded, size, at);
      await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
      expectViewHeld(
        await jumpAndWatch(page, { share: 0.3 }),
        `${at}, a jump from the foot up to a third of the way down:`
      );
    }
  });
});
