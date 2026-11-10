/** Whether any row of this run failed, for the one decision `e2e/global-teardown.ts` cannot make
 *  for itself.
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
 *  teardown: `e2e/failure-reporter.ts` sets this flag, and the teardown reads it. The runner
 *  loads the reporter, the hook and this module in one process, so one module holds one flag.
 *
 *  A row that failed and passed on its retry sets it too, and the logs of that run are kept. The
 *  run exits 0, so the wrapper would have deleted them; keeping them is the better answer, since
 *  a flaky row is exactly the one whose cause is in a server's log rather than in its trace. A
 *  row a Ctrl-C cut off sets it as well, for the same reason. */
let failed = false;

/** Called once per row that did not pass (`e2e/failure-reporter.ts`). */
export function recordFailedRow(): void {
  failed = true;
}

/** Whether any row of this run failed, read by `e2e/global-teardown.ts`. */
export function anyRowFailed(): boolean {
  return failed;
}
