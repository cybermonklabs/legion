/**
 * A hang guard for a test file whose rows run under a command that sets no measured `--timeout`.
 * `docs/testing.md` ("A Bun test waits for its fact, and its command sets its one guard") holds
 * the rule: a command sets one guard, its `--timeout`, measured and noted in its package's `"//"`,
 * and a row or a hook sets none of its own. Some commands set no measured guard: envoy-client's
 * `test` (`bun test --timeout 30000`), and an issue's check that runs a file with a bare
 * `bun test <file>`, which bounds each row and hook at Bun's 5 s default. A row that spawns a
 * child or waits on I/O passes such a bound on a loaded machine.
 *
 * In a file under such a command, `setDefaultTimeout(HANG_GUARD_MS)` above its first row sets the
 * guard for every row and hook registered after it, in that file alone. It decides no verdict: it
 * is far above any run seen and under a CI job's limit, so a row that never ends still fails,
 * named by Bun ("this test timed out after 120000ms"). The rule is the command's, not the
 * package's: a file whose rows run under a command that sets a measured `--timeout` must not set
 * it, since a file's default wins over the command's and would cut that guard. A file that still
 * sets it there is an exception its package's `"//"` note names.
 */
export const HANG_GUARD_MS = 120_000;
