import type { Page } from "@playwright/test";

import { createComment, createIssue, createProject } from "./api";
import {
  DESKTOP,
  longWords,
  nodeRow,
  nodeRows,
  openIssue,
  PHONE,
  phase,
  plannerActor,
  rowMore,
  unfoldEveryBranch,
} from "./issue-page-g";
import { expect, test } from "./test";

/**
 * Part G of the new issue page, decision 261: no cut changes a row the reader can see. A slice
 * cuts only rows wholly outside the view, and a row in view keeps its length until it lies wholly
 * outside it. The row of decision 260 that a slice holds is not enough for this: when the held
 * row is taller than the view, its top stays and its cut takes away the words the reader was
 * reading, and when it stands wholly in view, its cut moves every row under it. So this row reads
 * what the reader sees - the words under the middle of the view, and the top of the element that
 * holds them - for 60 frames after each jump, and allows no change at all.
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
 * planner's: most of 160 words, which a row cuts at six lines, and three of 1,500 to 2,000 words,
 * which are taller than the view at both widths when drawn whole. Every word is told apart by its
 * row and its place, so the words under one point of the view name where the reader is.
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

/** Opens issue `key` at `size` on a fresh load, waits until it draws every row the seed wrote,
 *  and until the rows drawn at load are cut, so no cut of theirs lands during a reading. */
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
  await expect(
    rowMore(nodeRow(page, seeded.rows[0] ?? "")),
    `${where} the first row was not cut`
  ).toHaveCount(1);
}

/** What the reader saw at the middle of the view after a jump, and for 60 frames after it. */
interface ViewRead {
  /** How many rows stood in view, uncut, in the task of the jump: rows no batch had measured. */
  readonly uncutInView: number;
  /** The words under the middle of the view in the task of the jump, and after 60 frames. */
  readonly before: string | null;
  readonly after: string | null;
  /** The most the top of the element under the middle of the view moved in those frames. */
  readonly moved: number;
}

/** Jumps the page, as a fast drag of the scrollbar does, either so the middle of row `row`
 *  stands at the middle of the view or to `share` of the page's height, and reads in the same
 *  task, then for 60 frames, what stands at the middle of the view. */
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
    const words = inView
      .map((row) => row.querySelector("[data-node-words]"))
      .find((found) => found != null);
    const wordsBox = words?.getBoundingClientRect();
    const x = wordsBox == null ? window.innerWidth / 2 : wordsBox.left + wordsBox.width / 2;
    const y = window.innerHeight / 2;
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
    const read = {
      before: wordsAtMiddle(),
      uncutInView: inView.filter(
        (row) =>
          row.querySelector("[data-node-words]") != null &&
          row.querySelector("[data-node-more]") == null
      ).length,
    };
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

/** Fails unless the jump landed on rows no batch measured, with words under the middle of the
 *  view, and records a failure, without stopping the row, when those words or the element that
 *  holds them moved, so one run reports every jump. */
function expectViewHeld(read: ViewRead, where: string): void {
  expect(
    read.uncutInView,
    `${where} the jump landed on no row a batch had not measured`
  ).toBeGreaterThan(0);
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

// Decision 261, as the root tester measured it on a copy of a real issue: after each jump, on a
// fresh load at each width, the words under the middle of the view and the element that holds them
// stay where they stood for 60 frames. Three jumps at each width: a landing at the middle of the
// tallest row, which crosses the view's top and is the row decision 260 holds; a jump from the top
// to the page's middle; and a jump from the foot up to a third of the way down. It carries `@phone`
// for the reason the decision-252 row gives: the phone project lays the page out at three device
// pixels to the CSS pixel.
// SLOW(>=2s): forty-five long rows seeded, three of them taller than the view, loaded six times at
// two widths, each load waited out until its rows are drawn and the rows at load are cut.
test("no cut moves the words at the middle of the view after a long jump or a landing inside the tallest row @phone", async ({
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
      // At the foot the last row is in view, which decision 261 keeps whole, so the foot's batch
      // is waited out on the row lying wholly above the view, which it cuts.
      const above = await page.evaluate((rows) => {
        window.scrollTo(0, document.documentElement.scrollHeight);
        return (
          [...rows]
            .reverse()
            .find(
              (id) =>
                (document
                  .querySelector(`[data-node-row][data-node-id="${id}"]`)
                  ?.getBoundingClientRect().bottom ?? 0) < 0
            ) ?? null
        );
      }, seeded.rows);
      expect(above, `${at} no row lies wholly above the view at the foot`).not.toBeNull();
      await expect(
        rowMore(nodeRow(page, above ?? "")),
        `${at} the row wholly above the view at the foot was not cut`
      ).toHaveCount(1);
      expectViewHeld(
        await jumpAndWatch(page, { share: 0.3 }),
        `${at}, a jump from the foot up to a third of the way down:`
      );
    }
  });
});
