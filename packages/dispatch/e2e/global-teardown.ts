/** Playwright's `globalTeardown` (`e2e/playwright.config.ts`): it stops the stacks
 *  `e2e/global-setup.ts` started, whatever the run did. A run that started none - the stubbed
 *  layer alone, a deployed run, a run against servers a person started - stops nothing here,
 *  since the pool holds nothing to stop.
 *
 *  It runs on a Ctrl-C as well as at the end of a run, which is what keeps an interrupted run
 *  from leaving a Postgres and a Go server serving loopback ports.
 *
 *  The logs are kept, and this is the one place that decision is not the run's status: Playwright
 *  computes the exit status AFTER its teardown tasks (`lib/cli/testActions.js:92` reads what
 *  `runAllTests` returns, and the teardown runs inside that call), so a teardown hook cannot tell
 *  a green run from a red one. What is kept is each stack's log directory, the only record of
 *  what its servers said, which a run of more than one stack writes and a run of one does not
 *  have at all; the data directories, which are the bulk of what the run wrote, go either way
 *  (`e2e/harness-pool.ts`). */
import { stopStacks } from "./harness-pool";

export default async function globalTeardown(): Promise<void> {
  await stopStacks(true);
}
