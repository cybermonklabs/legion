import type { Reporter, TestCase, TestResult } from "@playwright/test/reporter";

import { recordRow } from "./run-outcome";

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
 *  only account of what its servers were doing when it stopped. */
export default class FailureReporter implements Reporter {
  onTestEnd(test: TestCase, result: TestResult): void {
    recordRow(test.outcome() === "unexpected" || result.status === "interrupted");
  }
}
