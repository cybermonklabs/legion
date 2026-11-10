import type { Reporter, TestCase, TestResult } from "@playwright/test/reporter";

import { recordFailedRow } from "./run-outcome";

/** Tells `e2e/run-outcome.ts` that a row did not pass, so `e2e/global-teardown.ts` keeps this
 *  run's stack logs and a green run's are removed with its run directory. It reports nothing and
 *  prints nothing: the run's own reporters do that, and this one is beside them in
 *  `e2e/playwright.config.ts`.
 *
 *  Two readings, because they answer different questions. `test.outcome()` is the row's own
 *  verdict with its retries taken in, and `unexpected` is "it failed and nothing excused it";
 *  that is the one a green run turns on. `result.status === "interrupted"` is the row a Ctrl-C
 *  or a global timeout cut off, which has no verdict at all and whose logs are the only account
 *  of what its servers were doing when it stopped. */
export default class FailureReporter implements Reporter {
  onTestEnd(test: TestCase, result: TestResult): void {
    if (test.outcome() === "unexpected" || result.status === "interrupted") recordFailedRow();
  }
}
