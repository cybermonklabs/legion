/** The planted controls of e2e/__tests__/fixed-waits.test.ts: one wait of every shape the rule
 *  has an answer for, so proving the check looks needs no edit of a real spec. Without them a
 *  check that finds nothing in the suite would read the same whether it looked or not.
 *
 * Its name ends `.fixture.ts` and not `.e2e.ts` because e2e/playwright.config.ts says
 * `testDir: "."` and `testMatch: /.*\.e2e\.ts/`: Playwright walks the `e2e` directory
 * recursively, so a `.e2e.ts` file here would join the real suite and run. `bun test` collects
 * only `*.test.ts` and its kin, so this file runs nowhere; the test reads its text and parses it.
 * `suiteFiles` skips this whole directory for the same reason: the waits below are written to be
 * refused, and the rule is about the suite.
 *
 * The timers import is bound to a name no spec uses, because the rule reads the IMPORT and not
 * the word `sleep`: a call of `waitOut` below is a wait, and the file's own `sleep`, which comes
 * from nowhere near `node:timers/promises`, is not.
 *
 * The lines are asserted in the test, so a wait added here goes at the END of the function below.
 */

import { setTimeout as waitOut } from "node:timers/promises";
import type { Page } from "@playwright/test";

/** A `sleep` of this file's own, so the control below calls one the rule must leave alone. */
async function sleep(milliseconds: number): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  globalThis.setTimeout(resolve, milliseconds);
  await promise;
}

export async function plantedWaits(page: Page): Promise<void> {
  // The sweep runs on a clock of its own and the page shows nothing while it does, so this
  // window is the measurement rather than a guess at one.
  await page.waitForTimeout(100);

  await page.waitForTimeout(200);

  // wait
  await page.waitForTimeout(300);

  // A reason far enough above the call that the next reader of the call does not see it, which
  // is the whole of what this rule is about.

  await page.waitForTimeout(400);

  /* A block comment is a comment: what matters is that a reason ends on the line above the
     call, not which way it was written. */
  await page.waitForTimeout(500);

  await page.waitForTimeout(600); // A reason trailing the call, where the rule does not read it.

  // A reason above a statement is not above a call written on a later line of it, so neither of
  // the paced waits below stands under one.
  const paced: Promise<void>[] = [100, 200, 300].map((milliseconds) =>
    page.waitForTimeout(milliseconds)
  );
  await Promise.all(paced);

  // 900
  await page.waitForTimeout(900);

  // The other spelling, under a reason: a wait on node's timers is the same wait as the ones
  // above, and this one says what it is here for.
  await waitOut(1_000);

  await waitOut(1_100);

  await sleep(1_200);
}
