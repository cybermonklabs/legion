/** the rows draw the tree read the server answers for each seeded issue, compared with the
 *  recorded rows */

import {
  DECISION_SEEDS,
  expectSameRows,
  expectSameRowsDecisions,
  expectSameRowsStampedTitle,
  SAME_ROWS_A_SEEDS,
  SAME_ROWS_MS,
} from "./issue-page-t-rows";
import { test } from "./test";

/**
 * Row 1 of the rows that draw the new issue page from the tree read, "the outline drawn from the
 * tree read holds the rows the reads drew", for the first nine of `SEEDS` in their order
 * (`SAME_ROWS_A_SEEDS`), each seed's decision clauses and its author of an agent's decision rows
 * of their own; the last five are in `e2e/issue-page-t-same-rows-b.e2e.ts`. The head of
 * `e2e/issue-page-t-rows.ts` says what the rows read, what their gates are and why the seeds
 * split there.
 */

for (const [name, seed] of SAME_ROWS_A_SEEDS) {
  const title = `the outline drawn from the tree read holds the rows the reads drew: ${name}`;
  // c-same-rows on one seed, but the author of an agent's decision, which is the `@owed` row's
  // below (`ReadingsPart`).
  // SLOW(>=2s): a whole seeded issue, read as it opens and again with every branch opened and
  // every row's body read in turn, each reading taken twice until it holds still.
  test(title, async ({ browser }) => {
    test.setTimeout(SAME_ROWS_MS);
    await expectSameRows(browser, name, seed);
  });

  if (DECISION_SEEDS.has(name)) {
    // c-same-rows' decision clauses on one seed, owed on `decisionNodesGate`.
    // SLOW(>=2s): a whole seeded issue, read as it opens and again with every branch opened,
    // each reading taken twice until it holds still.
    test(`${title}, its decisions as nodes of the tree read @owed`, async ({ browser }) => {
      test.setTimeout(SAME_ROWS_MS);
      await expectSameRowsDecisions(browser, name, seed);
    });

    // c-same-rows' author of an agent's decision on one seed, owed on `sessionTitleGate`: the
    // title its session stamped, on its row's line and in its body, as the fixture recorded it.
    // SLOW(>=2s): a whole seeded issue, read as it opens and again with every branch opened and
    // every row's body read in turn, each reading taken twice until it holds still.
    test(`${title}, an agent's decision by the title its session stamped @owed`, async ({
      browser,
    }) => {
      test.setTimeout(SAME_ROWS_MS);
      await expectSameRowsStampedTitle(browser, name, seed);
    });
  }
}
