import type { Reporter, TestCase, TestResult } from "@playwright/test/reporter";

import { recordError, recordRow } from "./run-outcome";

/** Tells `e2e/run-outcome.ts` how each row ended, so `e2e/global-teardown.ts` removes a green
 *  run's stack logs with its run directory and keeps every other run's. It reports nothing and
 *  prints nothing: the run's own reporters do that, and this one is beside them in
 *  `e2e/playwright.config.ts`.
 *
 *  Every row is recorded, the ones that passed included, because "no row was recorded" is the
 *  answer that keeps the logs: a run this reporter never loaded for, and a run that died before
 *  its first row, must not read as green (`e2e/run-outcome.ts` says why that is the way round).
 *
 *  Two readings of a failure, because they answer different questions. `test.outcome()` is the
 *  row's own verdict with its retries taken in, and `unexpected` is "it failed and nothing
 *  excused it"; that is the one a green run never sees. `result.status === "interrupted"` is the
 *  row a Ctrl-C or a global timeout cut off, which has no verdict at all and whose logs are the
 *  only account of what its servers were doing when it stopped.
 *
 *  AND A FAILURE THAT BELONGS TO NO ROW REACHES `onError` AND NOTHING ELSE. The path this was
 *  measured on is a WORKER-SCOPED FIXTURE whose teardown throws: its worker reports it as a
 *  teardown error, and the runner hands every one of those to `reporter.onError` with the
 *  worker's info and to nothing else (node_modules/.bun/playwright@1.64.1/node_modules/
 *  playwright/lib/runner/index.js:5882-5892, `worker.on("teardownErrors", …)`). A run of one
 *  passing row with such a fixture printed `1 passed`, `1 error was not a part of any test`
 *  and exited 1. The runner has two more senders of the same kind: a worker's own process
 *  error (:5893-5896) and an error left over once there is no remaining row to fail with it
 *  (:6073-6077, `_massSkipTestsFromRemaining`). The run exits non-zero, and a reporter that
 *  only read rows would have let the teardown delete the stack logs of a red run - which is
 *  the one thing `e2e/run-outcome.ts` exists to prevent.
 *
 *  An `afterAll` that throws AFTER THE LAST ROW of its file is NOT one of them, however it
 *  reads: at 1.64.1 it runs inside that last row and `onTestEnd` reports it. Measured twice,
 *  with a throwing `afterAll` and with a worker that `process.exit(1)`s in one test: both were
 *  reported on the last row. `lib/runner/index.js:267-281` is not one of them either - that is
 *  `TeleReporterReceiver._onError`, which replays a report a `--reporter=blob` leg wrote, not
 *  the path a CLI run takes. */
export default class FailureReporter implements Reporter {
  onError(): void {
    recordError();
  }

  onTestEnd(test: TestCase, result: TestResult): void {
    recordRow(test.outcome() === "unexpected" || result.status === "interrupted");
  }
}
