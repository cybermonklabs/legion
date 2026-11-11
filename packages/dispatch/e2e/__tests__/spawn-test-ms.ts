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

/** The per-test limit of every row in those two files that spawns a process.
 *
 *  Measured on the fast pod shape, each the worst reading of its run: 9.46 s (implementer, this
 *  issue's first pod, 2 CPUs, 8 runs under 6 busy loops, 3 x nproc), 12.78 s (reviewer, the
 *  replacement pod) and 3.90 s (implementer, that same replacement pod, 8 runs). The limit
 *  follows the worst reading rather than the most recent: the 3.90 s reading came last, and a
 *  limit sized from it would go red the next time a pod reads as the reviewer's did.
 *
 *  Inferred for the slow pod shape: about 27.5 s. Pods of the same core count do not read alike,
 *  and dispatch://INFRA-1505's Verification preamble records the ratio at code that does not
 *  differ - a 335 s suite on one pod against 156 s on this shape, about 2.15 times - so the
 *  12.78 s reading above is about 27.5 s there. That figure is inferred from that source and
 *  measured by nobody; a real reading on that shape replaces it, and then this number is sized
 *  from the reading rather than from the inference.
 *
 *  90 s is three times the inferred 27.5 s, which is this tree's own bar applied to the worst
 *  shape known to exist. It is deliberately generous, because this limit is no row's subject: a
 *  number too large costs a slower report of a true hang, a number too small costs a flake that
 *  ejects a pull request from the merge queue, and the two are not comparable.
 *
 *  So it is two things at once, and a reader should not take it for one. On the slow shape it is
 *  a limit sized from measurement, through the inference above. On this pod it is seven times
 *  the worst reading ever taken here, which is past any sizing: here it is a hang guard, and a
 *  row it cuts off reads about 90 s. Bun's unwritten 5 s default is under every reading above
 *  but the 3.90 s one, which is why a busy machine read as a defect of whatever change was
 *  under test. */
export const SPAWN_TEST_MS = 90_000;
