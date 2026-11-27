import type { Page } from "@playwright/test";

import { nextFrames } from "./frames";
import {
  DESKTOP,
  LONG_ROWS,
  nodeRow,
  nodeRows,
  openIssue,
  PHONE,
  phase,
  rowMore,
  seedLongRows,
  unfoldEveryBranch,
} from "./issue-page-g";
import { expect, test } from "./test";

// What this file's init script keeps on the page's window.
declare global {
  interface Window {
    /** The long row in the first view at each frame from the first it drew its words in. */
    __firstViewFrames?: FirstViewFrame[];
  }
}

/**
 * Part G of the new issue page, decisions 252, 260 and 263: where a long row is cut as the page
 * opens and as it jumps, and what that cut leaves of the rows in view. Every row seeds the
 * conversation of long rows `seedLongRows` writes and reads the page through
 * `e2e/issue-page-g.ts`: a long jump into the middle of the page, and one that lands on the
 * reader's own rows, keep the first row wholly in view where it stood as the row above it is
 * cut; and a long row in the first view is drawn cut on the first frame it is drawn in. The rest
 * of part G's rows are `e2e/issue-page-g.e2e.ts`'s, and decision 261's is
 * `e2e/issue-page-g-view.e2e.ts`'s.
 *
 * The rows are on the real stack: the page draws what the server stored and indexed, and the new
 * page is the reader's own preference (`withIssuePage`).
 */

/** What a jump read in the task that made it, before the page could measure any row it landed
 *  on. */
interface Jump {
  /** The top of the words of the row the jump put across the view's top. */
  readonly crossingTop: number;
  /** Whether that row or `first` was drawn cut already. */
  readonly cut: boolean;
  /** The first row wholly in view, and its top. */
  readonly first: string | null;
  readonly top: number;
  /** Every row in view, by node id, top to bottom. */
  readonly inView: readonly string[];
}

/** Opens issue `key` at `size` and waits until it draws every long row `seedLongRows` wrote,
 *  with every branch open. */
async function openLongRows(
  page: Page,
  key: string,
  size: { height: number; width: number },
  where: string
): Promise<void> {
  await page.setViewportSize(size);
  await openIssue(page, key);
  await expect
    .poll(
      async () => {
        await unfoldEveryBranch(page);
        return nodeRows(page).count();
      },
      { message: `${where} the page never drew every long row` }
    )
    .toBeGreaterThan(LONG_ROWS);
}

/** Jumps the page, as a drag of the scrollbar does, to 40 px above the foot of the words of row
 *  `crossing`, and reads where the rows stand in the same task. */
function jumpAbove(page: Page, crossing: string): Promise<Jump> {
  return page.evaluate((id): Jump => {
    const row = document.querySelector(`[data-node-row][data-node-id="${id}"]`);
    const words = row?.querySelector("[data-node-words]");
    if (row == null || words == null) throw new Error(`the row of ${id} is not drawn`);
    window.scrollTo(0, words.getBoundingClientRect().bottom + window.scrollY - 40);
    const boxes = [...document.querySelectorAll("[data-node-row]")].map(
      (drawn) => [drawn, drawn.getBoundingClientRect()] as const
    );
    const first = boxes.find(([, box]) => box.top >= 0 && box.bottom <= window.innerHeight);
    return {
      crossingTop: words.getBoundingClientRect().top,
      cut: [row, first?.[0]].some((drawn) => drawn?.querySelector("[data-node-more]") != null),
      first: first?.[0].getAttribute("data-node-id") ?? null,
      inView: boxes
        .filter(([, box]) => box.bottom > 0 && box.top < window.innerHeight)
        .map(([drawn]) => drawn.getAttribute("data-node-id") ?? ""),
      top: first?.[1].top ?? Number.NaN,
    };
  }, crossing);
}

/** Waits until row `above`, which a jump left above the view, draws its cut, and two frames
 *  more, and fails unless the first row wholly in view that `jump` read stands within a pixel of
 *  where it stood, and unless it and row `crossing`, which a jump left across the view's top,
 *  are still drawn whole: a row in view keeps its length (decision 261). */
async function expectFirstHeld(
  page: Page,
  rows: { readonly above: string; readonly crossing: string },
  jump: Jump,
  where: string
): Promise<void> {
  await expect(
    rowMore(nodeRow(page, rows.above)),
    `${where} the row above the view was not cut`
  ).toHaveCount(1);
  await nextFrames(page);
  const first = nodeRow(page, jump.first ?? "");
  const top = await first.evaluate((row) => row.getBoundingClientRect().top);
  expect(
    Math.abs(top - jump.top),
    `${where} the first row wholly in view moved from ${jump.top} px to ${top} px`
  ).toBeLessThanOrEqual(1);
  await expect(rowMore(first), `${where} the first row wholly in view was cut`).toHaveCount(0);
  await expect(
    rowMore(nodeRow(page, rows.crossing)),
    `${where} the row across the view's top was cut`
  ).toHaveCount(0);
}

/** The planner's row that holds the page's middle, of the rows `rows` names. */
async function middleRow(page: Page, rows: readonly string[], where: string): Promise<string> {
  const middle = await page.evaluate(
    (ids) =>
      ids.find((id) => {
        const words = document.querySelector(
          `[data-node-row][data-node-id="${id}"] [data-node-words]`
        );
        const half = document.documentElement.scrollHeight / 2;
        return words !== null && words.getBoundingClientRect().bottom + window.scrollY >= half;
      }) ?? null,
    rows
  );
  expect(middle, `${where} no long row holds the page's middle`).not.toBeNull();
  return middle ?? "";
}

// Decision 252: a long jump into the middle of the page - a fast drag of the scrollbar - lands
// on rows no batch measured, and the browser's own scroll anchoring holds the view only while
// the rows that shrink are outside it. At each width the row scrolls a conversation of long rows
// to its foot and waits there until the rows above the view are cut, then jumps to 40 px above
// the foot of the words of the row that holds the page's middle: that row crosses the view's
// top, and the one under it is the first row wholly in view. Once the row above them draws its
// cut, the first row wholly in view stands where it stood, within a pixel, and both rows in view
// are still whole (decision 261). It carries `@phone` because the phone project lays the page
// out at three device pixels to the CSS pixel, where the scroll that holds the row snaps to a
// device pixel.
// SLOW(>=2s): sixty-six long rows seeded, loaded at two widths, scrolled to their foot and to
// their middle, each scroll waited out until the rows above the view are cut.
test("a long jump into the middle of the page keeps the first row wholly in view where it stood as the rows there are cut @phone", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedLongRows, async (page, seeded) => {
    for (const size of [DESKTOP, PHONE]) {
      const where = `at ${size.width} px`;
      await openLongRows(page, seeded.key, size, where);
      await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
      await expect
        .poll(
          () =>
            page.evaluate(() =>
              [...document.querySelectorAll("[data-node-row]")].some((row) => {
                const box = row.getBoundingClientRect();
                return box.bottom <= 0 && row.querySelector("[data-node-more]") !== null;
              })
            ),
          { message: `${where} no row above the view at the page's foot was cut` }
        )
        .toBe(true);
      const crossing = await middleRow(page, seeded.rows, where);
      const jump = await jumpAbove(page, crossing);
      const at = seeded.rows.indexOf(crossing);
      expect(
        jump,
        `${where} the jump did not land with a whole row crossing the view's top above the first row wholly in view`
      ).toMatchObject({ cut: false, first: seeded.rows[at + 1] });
      expect(jump.crossingTop, `${where} the crossing row's words start in view`).toBeLessThan(0);
      await expectFirstHeld(page, { above: seeded.rows[at - 1] ?? "", crossing }, jump, where);
    }
  });
});

// Decision 260: the row a slice holds is the first row wholly in view of any kind, not only one
// that can be cut. At each width the row jumps from the page's top to 40 px above the foot of the
// words of the planner's row right above the reader's own run of long rows, which no cut ever
// shortens: the planner's row crosses the view's top, and the reader's rows alone stand in view
// under it. Once the planner's row above it draws its cut, the reader's first row stands where
// it stood, within a pixel, and both rows are whole. It carries `@phone` for the reason the row
// above gives.
// SLOW(>=2s): sixty-six long rows seeded, loaded at two widths, and each jump into them waited
// out until the planner's row above the view is cut.
test("a long jump that lands on the reader's own rows keeps the first of them where it stood as the row above them is cut @phone", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedLongRows, async (page, seeded) => {
    const mine = seeded.own[0] ?? "";
    const above = seeded.rows[seeded.rows.indexOf(seeded.aboveOwn) - 1] ?? "";
    for (const size of [DESKTOP, PHONE]) {
      const where = `at ${size.width} px`;
      await openLongRows(page, seeded.key, size, where);
      // The rows drawn at load are cut before the jump, so no cut of theirs lands after it.
      await expect(
        rowMore(nodeRow(page, seeded.rows[0] ?? "")),
        `${where} the first row was not cut`
      ).toHaveCount(1);
      const jump = await jumpAbove(page, seeded.aboveOwn);
      expect(
        jump,
        `${where} the jump did not land with the planner's whole row across the view's top`
      ).toMatchObject({ cut: false, first: mine });
      expect(jump.crossingTop, `${where} the planner's row's words start in view`).toBeLessThan(0);
      expect(
        jump.inView,
        `${where} a row other than the reader's own stands in view under the planner's`
      ).toEqual([seeded.aboveOwn, ...seeded.own.slice(0, jump.inView.length - 1)]);
      await expectFirstHeld(page, { above, crossing: seeded.aboveOwn }, jump, where);
    }
  });
});

/** What a row read of the long row in the first view at each frame from the first it drew its
 *  words in, from a script that runs before the page's own. */
interface FirstViewFrame {
  readonly cut: boolean;
  readonly height: number;
  readonly top: number;
  readonly view: number;
}

// Decision 263: a row that stands in view as the page opens is cut before the page paints it, so
// the reader never sees it whole and it never shrinks in view. At each width a script that runs
// before the page's own reads the planner's long statement, the body's first row, which a check
// nothing proves keeps open in an outline folded to what is open, at each animation frame from
// the first one it is drawn in: it is cut on that first frame, and its height is the same 60
// frames later. The row is drawn from the spec's blocks before the coverage read brings its
// words, and a row with no words has nothing to cut, so it is read from the first frame it draws
// its words in: CI run 44547033390 read it at 390 px with none for two frames, and then whole,
// 472 px tall, for the 59 after. It carries `@phone` because the phone's first view is another
// set of rows, laid out at three device pixels to the CSS pixel.
// SLOW(>=2s): sixty-six long rows seeded, and the page loaded at two widths, each watched over
// 61 frames.
test("a long row in the first view is drawn cut on the first frame it is drawn in, and its height holds 60 frames later @phone", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedLongRows, async (page, seeded) => {
    await page.addInitScript((id) => {
      const frames: FirstViewFrame[] = [];
      window.__firstViewFrames = frames;
      const read = () => {
        const row = document.querySelector(`[data-node-row][data-node-id="${id}"]`);
        const words = row?.querySelector("[data-node-words] [data-author-text]");
        if (row !== null && (words?.textContent ?? "").trim() !== "") {
          const box = row.getBoundingClientRect();
          frames.push({
            cut: row.querySelector("[data-node-more]") !== null,
            height: box.height,
            top: box.top,
            view: window.innerHeight,
          });
        }
        if (frames.length <= 60) requestAnimationFrame(read);
      };
      requestAnimationFrame(read);
    }, seeded.statement);
    for (const size of [DESKTOP, PHONE]) {
      const where = `at ${size.width} px`;
      await page.setViewportSize(size);
      await openIssue(page, seeded.key);
      await page.waitForFunction(() => (window.__firstViewFrames?.length ?? 0) > 60);
      // The wait above saw more than 60 frames recorded, and the script stops recording once it
      // holds 61, so one read holds every frame the checks below read.
      const frames = await page.evaluate(() => window.__firstViewFrames ?? []);
      const [first] = frames;
      if (first === undefined) throw new Error(`${where} the row never drew its words`);
      expect(first.top, `${where} the row was drawn outside the first view`).toBeLessThan(
        first.view
      );
      expect(first.cut, `${where} the row was drawn whole on the first frame it was drawn in`).toBe(
        true
      );
      expect(
        frames.map((frame) => frame.height).filter((height) => height !== first.height),
        `${where} the row's height changed in the 60 frames after its first`
      ).toEqual([]);
    }
  });
});
