/** The one per-test limit the spawning rows of `e2e/__tests__/harness-lanes.test.ts` and
 *  `e2e/__tests__/visual-run-lanes.test.ts` read.
 *
 * It lives here rather than in either file because both hold rows of the same shape - a real
 * process, whose cost follows the machine - and a copy in each is what drifted: the two carried
 * the same figure under comments quoting different measurements, so neither said what the limit
 * was sized from.
 *
 * `bun test` collects only `*.test.ts` and its kin, so a plain `.ts` module under `__tests__/`
 * runs nowhere — the same thing `e2e/__tests__/playwright-listing.ts` and
 * `e2e/__tests__/fixtures/` rely on.
 */

/** The per-test limit of every row in those two files that spawns a process. Three readings of
 *  those rows, each the worst of its run: 9.46 s, measured by the implementer on this issue's
 *  first pod (2 CPUs, 8 runs under 6 busy loops, 3 x nproc); 12.78 s, measured by the reviewer on
 *  the replacement pod; 3.90 s, measured by the implementer on that same replacement pod, 8 runs.
 *  The 12.78 s reading is the one this limit is sized from - 40 s is about 3.1 times it - because
 *  the limit follows the worst reading seen rather than the most recent: the 3.90 s reading came
 *  last, and a limit sized from it would go red the next time a pod reads as the reviewer's did.
 *  It is a limit sized from measurement rather than a hang guard, though a row that hangs is
 *  ended by it. Bun's unwritten 5 s default is under the 9.46 s and the 12.78 s readings, so a
 *  busy machine read as a defect of whatever change was under test. */
export const SPAWN_TEST_MS = 40_000;
