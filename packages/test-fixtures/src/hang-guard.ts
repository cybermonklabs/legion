/**
 * A hang guard for the test files of a package whose test command sets no measured `--timeout`.
 * `docs/testing.md` ("A Bun test waits for its fact, and its command sets its one guard") holds
 * the rule: a command sets one guard, its `--timeout`, measured and noted in its package's `"//"`,
 * and a row or a hook sets none of its own. A package that sets none has no such guard, and an
 * issue's check runs its files with a bare `bun test <file>`, which bounds each row and hook at
 * Bun's 5 s default: a row that spawns a child or waits on I/O passes that on a loaded machine.
 *
 * In such a file, `setDefaultTimeout(HANG_GUARD_MS)` above its first row sets the guard for every
 * row and hook registered after it, in that file alone. It decides no verdict: it is far above any
 * run seen and under a CI job's limit, so a row that never ends still fails, named by Bun ("this
 * test timed out after 120000ms"). Not for a package whose command sets a measured `--timeout`
 * (pi-envoy's 250 s, pi-legion's 300 s, test-fixtures' 30 s): a file's default wins over the
 * command's, so it would cut that guard.
 */
export const HANG_GUARD_MS = 120_000;
