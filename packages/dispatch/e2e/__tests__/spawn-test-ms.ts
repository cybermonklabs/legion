/** The one per-test limit the rows of `e2e/__tests__/` that pay for a real process read:
 *  the spawning rows of `harness-stacks.test.ts`, and `psql-session.test.ts`'s overlapping-call
 *  row, which holds `SELECT pg_sleep(3)` open and so costs at least 3 s by construction.
 *
 * `bun test` collects only `*.test.ts` and its kin, so a plain `.ts` module under `__tests__/`
 * runs nowhere — the same thing `e2e/__tests__/playwright-listing.ts` and
 * `e2e/__tests__/fixtures/` rely on.
 */

/** The per-test limit of every row named above.
 *
 *  Measured on the fast pod shape (2 CPUs, gVisor, under 6 busy loops, 3 x nproc), 20 runs at
 *  this head: the worst reading of the rows this limit covers is 3.15 s, by
 *  `psql-session.test.ts`'s overlapping-call row, whose own 3 s sleep is most of it; then 1.18 s
 *  by `"psql reads its own stack's database"`, and the rest under 0.3 s.
 *
 *  The readings that sized this number came from rows main has since deleted -
 *  `harness-lanes.test.ts` and `visual-run-lanes.test.ts`, removed by cybermonklabs/legion#3435.
 *  On the fast shape their worst readings were 9.46 s and 12.78 s. For the slow pod shape: about
 *  27.5 s, inferred, because dispatch://INFRA-1505's Verification preamble records the ratio at
 *  code that does not differ - a 335 s suite against 156 s, about 2.15 times. That figure is
 *  inferred from that source and measured by nobody.
 *
 *  So for the rows it covers today the number is a hang guard rather than a sized limit: 90 s is
 *  about 76 times their worst reading. It is kept at 90 s rather than re-sized down because the
 *  deleted rows were of this same class and spiked from 1.5-2.7 s to 9.46 s, about 3.5 to 6
 *  times, so a row of this class reading 1.18 s today can read past Bun's unwritten 5 s default
 *  on a bad run, and on the inferred slow shape its 2.5 s is only twice that default, under this
 *  tree's bar of three. A row this limit cuts off costs about 90 s. */
export const SPAWN_TEST_MS = 90_000;
