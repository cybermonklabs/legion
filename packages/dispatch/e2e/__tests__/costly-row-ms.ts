/** The per-test limit of the rows of `e2e/__tests__/` that measure above about 1 s under the
 *  calibrated load and have no limit of their own file's: the ones a direct `bun test <file>` run
 *  of their file could put near Bun's unwritten 5 s default. `psql-session.test.ts`'s
 *  overlapping-call row is one, and holds `SELECT pg_sleep(3)` open, so it costs at least 3 s by
 *  construction. One call site is there for another reason, which the criterion alone would not
 *  explain: a row well under that second takes this limit where it shares its neighbour's work
 *  and would otherwise be the one row of a pair deciding on a budget nobody chose for it.
 *  Paying for a real process is not the criterion: `harness-signin.test.ts` and
 *  `round-robin-proxy.test.ts` spawn processes of their own and carry no limit at all.
 *
 *  Which rows take this limit is what its call sites say, and this comment lists no more of them
 *  than the one example: a list of that shape was falsified twice in this module, while it was
 *  `spawn-test-ms.ts` and before `e7cb5310` renamed it, and once in `packages/dispatch/AGENTS.md`.
 *  dispatch://INFRA-3259 is the check that would keep such a list true for this
 *  lane, as `e2e/tools/check-slow-rows.ts` keeps the Playwright suite's row reasons true; it is
 *  filed and cannot start yet.
 *
 * `bun test` collects only `*.test.ts` and its kin, so a plain `.ts` module under `__tests__/`
 * runs nowhere — the same thing `e2e/__tests__/playwright-listing.ts` and
 * `e2e/__tests__/fixtures/` rely on.
 */

/** The per-test limit of every row that criterion covers.
 *
 *  What it bounds is a direct `bun test <file>` run of those files, where Bun's unwritten 5 s
 *  default would otherwise apply. In the lane, `bun test --parallel --timeout 60000 e2e/__tests__`
 *  (this package's `test` script, from cybermonklabs/legion#2894), an explicit per-test limit
 *  wins over that flag, so these rows run at 90 s there rather than at the lane's 60 s. In either
 *  shape this number is a hang guard rather than a sized limit.
 *
 *  Measured on the fast pod shape (2 CPUs, gVisor, under 6 busy loops, 3 x nproc), 8 runs of the
 *  whole lane: the worst reading of the rows this limit covers is 3.15 s, by
 *  `psql-session.test.ts`'s overlapping-call row, whose own 3 s sleep is most of it; then 1.18 s
 *  by `"psql reads its own stack's database"`, and the rest under 0.3 s. 90 s is about 29 times
 *  that worst reading. The pull request's own per-row table gives that same row 3.05 s from the
 *  same eight runs, and no surviving record says which run each figure was read from, so this
 *  comment keeps the higher of the two: a hang guard sized against the smaller would be the one
 *  mistake that matters here. A third reading, taken when that pair was found to disagree: 12
 *  runs of `psql-session.test.ts` alone, under the same load, give the row 3.02 s worst and
 *  3.01 s mean. That is the file-alone shape, not a lane-loaded one, so it is no evidence that
 *  the row got faster and does not belong beside the two figures above.
 *
 *  The slow pod shape runs about 2.15 times slower: dispatch://INFRA-1505's Verification
 *  preamble records the ratio at code that does not differ - a 335 s suite against 156 s. That
 *  figure is inferred from that source and measured by nobody. Only the part of a reading that
 *  follows the machine is scaled by it: the 3.15 s row is a fixed 3 s `pg_sleep` plus about
 *  0.15 s of work, so about 3.3 s there, while the 1.18 s row follows the machine throughout,
 *  so about 2.5 s there.
 *
 *  It is kept at 90 s rather than re-sized down because rows of this class spike. The readings
 *  that sized this number came from rows main has since deleted - `harness-lanes.test.ts` and
 *  `visual-run-lanes.test.ts`, removed by cybermonklabs/legion#3435 - one of which went from
 *  1.5-2.7 s to 9.46 s on the fast shape, about 3.5 to 6 times. A row of this class reading
 *  1.18 s today is about 2.5 s on the slow shape, half of a direct run's 5 s default: that
 *  default would leave it about twice its cost, under this tree's bar of three. A row this
 *  limit cuts off costs about 90 s. */
export const COSTLY_ROW_MS = 90_000;
