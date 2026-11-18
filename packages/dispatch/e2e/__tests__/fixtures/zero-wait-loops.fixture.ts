/** The planted controls of e2e/__tests__/zero-wait-loops.test.ts: loops whose number of turns is
 *  one zero-wait read of the page's elements, which decide what a row does as a branch does.
 *
 * `locator.all()` answers the elements that match at the instant it is read, and waits for none,
 * as `count()` does. A loop over that answer, read before the page has drawn them, runs no turn at
 * all, and a row whose assertions stand inside the loop then passes without one of them run.
 *
 * Its name ends `.fixture.ts` for the reasons `zero-wait-probes.fixture.ts` gives: Playwright and
 * `bun test` collect neither, and `suiteFiles` leaves `__tests__` out.
 *
 * The lines are asserted in the test, so a probe added here goes at the END of the file.
 */

import type { Locator } from "@playwright/test";

/** One loop per shape that is refused, in this order: a `for...of` over `all()`, a `const` bound
 *  to `all()` that a later `for...of` walks, and a `for...of` over `allTextContents()`. */
export async function plantedLoopRefusals(rows: Locator): Promise<string[]> {
  for (const row of await rows.all()) await row.click();

  const items = await rows.all();
  for (const item of items) await item.click();

  const texts: string[] = [];
  for (const text of await rows.allTextContents()) texts.push(text);
  return texts;
}

/** The same loop as the first, under a reason of eight words or more, which is kept. */
export async function plantedLoopKept(rows: Locator): Promise<void> {
  // The count above settled the rows, so one read of them is enough here.
  for (const row of await rows.all()) await row.click();
}
