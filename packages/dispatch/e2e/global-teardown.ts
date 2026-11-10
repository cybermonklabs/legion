/** Playwright's `globalTeardown` (`e2e/playwright.config.ts`): it stops the stacks
 *  `e2e/global-setup.ts` started, whatever the run did. A run that started none - the stubbed
 *  layer alone, or a run against servers a person started - stops nothing here, since the pool
 *  holds nothing to stop.
 *
 *  It runs on a Ctrl-C as well as at the end of a run, which is what keeps an interrupted run
 *  from leaving a Postgres and a Go server serving loopback ports.
 *
 *  Whether this run's stack logs are kept is the one decision this hook cannot read from
 *  Playwright, which hands a teardown the configuration alone and runs it before it asks a
 *  reporter for the run's status. `e2e/run-outcome.ts` is where the answer comes from, and says
 *  why. What is kept is each stack's log directory, the only record of what its servers said,
 *  which a run of more than one stack writes and a run of one does not have at all; the data
 *  directories, which are the bulk of what the run wrote, go either way (`e2e/harness-pool.ts`). */
import { stopStacks } from "./harness-pool";
import { anyRowFailed } from "./run-outcome";

export default async function globalTeardown(): Promise<void> {
  await stopStacks(anyRowFailed());
}
