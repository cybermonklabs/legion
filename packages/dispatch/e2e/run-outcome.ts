/** Whether this run PASSED, for the one decision `e2e/global-teardown.ts` cannot make for
 *  itself: whether to keep this run's stack logs or remove them with the run directory.
 *
 *  The stacks' logs are the only record of what their servers said, and a failed run is read from
 *  them; a green run's are megabytes nobody opens, which the wrapper this suite replaced deleted
 *  with the run directory. Playwright gives a `globalTeardown` hook the configuration and nothing
 *  else, and it runs the teardown BEFORE it asks a reporter for the run's status: `taskRunner.run`
 *  calls `cleanup()`, which is the teardown tasks, and `finishTaskRun` calls `reporter.onEnd`
 *  after it (node_modules/.bun/playwright@1.64.1/node_modules/playwright/lib/runner/
 *  index.js:6320-6323, :6498-6501). So the status cannot reach the teardown from there.
 *
 *  What does reach it is a reporter's `onTestEnd`, which fires as each row ends, long before the
 *  teardown, and its `onError`, which fires for a failure that belongs to no row at all:
 *  `e2e/failure-reporter.ts` records both here, and the teardown reads the answer. The runner
 *  loads the reporter, the hook and this module in one process, so one module holds one tally.
 *
 *  WHY THE QUESTION IS "DID THIS RUN PASS" AND NOT "DID A ROW FAIL". A `--reporter` on the
 *  command line REPLACES the configured reporter list; it does not add to it. The resolved
 *  `reporter` is `[...takeFirst(configCLIOverrides.reporter, resolveReporters(userConfig.reporter,
 *  configDir), [[defaultReporter]]), ...configCLIOverrides.additionalReporters ?? []]`
 *  (node_modules/.bun/playwright@1.64.1/node_modules/playwright/lib/common/index.js:523), and
 *  `takeFirst` hands back the command line's whole list the moment there is one; only
 *  `--add-reporter` reaches that second half. So a run given `--reporter=list,blob` loads no
 *  reporter of this config's, nothing records a row here, and a module that answered "did a row
 *  fail" would answer NO for a run whose every row failed - and the teardown would delete the red
 *  leg's stack logs, which are the one thing a reader of that leg came for. A run that died in
 *  `globalSetup` reports no row either, for a different reason and with the same cost.
 *
 *  So the tally is turned around, and silence is a failure: a run has passed only when at least
 *  one row was RECORDED and none of the recorded rows failed. A run nothing reported keeps its
 *  logs, which costs one directory under TMPDIR and loses nothing. The precise answer is bought
 *  back where it pays: `.github/workflows/dispatch-browser.yaml` names `./e2e/failure-reporter.ts`
 *  in both of its `--reporter` lists, so a green CI leg records its rows and deletes its own logs.
 *
 *  A row that failed and passed on its retry is a failure here, and the logs of that run are
 *  kept. The run exits 0, so the wrapper would have deleted them; keeping them is the better
 *  answer, since a flaky row is exactly the one whose cause is in a server's log rather than in
 *  its trace. A row a Ctrl-C cut off counts the same way, for the same reason. */

/** Whether any row at all reached this module, which is what tells a reported run from a silent
 *  one, and whether any failure did - a row's own, or one that belonged to no row. */
let recorded = false;
let failed = false;

/** Called once per row as it ends (`e2e/failure-reporter.ts`), with that row's verdict. */
export function recordRow(rowFailed: boolean): void {
  recorded = true;
  if (rowFailed) failed = true;
}

/** Called for a failure that belongs to no row (`e2e/failure-reporter.ts`'s `onError`): the
 *  error of a worker that died, or of an `afterAll` that failed after the last row. It does NOT
 *  set `recorded`, which answers "did a row reach this module" and nothing else; a failure is
 *  enough on its own, since a run that recorded one has not passed whatever its rows did. */
export function recordError(): void {
  failed = true;
}

/** Whether this run passed: a row was recorded, and nothing recorded a failure. Read by
 *  `e2e/global-teardown.ts`, which keeps the stack logs of every run that did not. */
export function runPassed(): boolean {
  return recorded && !failed;
}
