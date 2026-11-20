/** Playwright's `globalTeardown` (`e2e/playwright.config.ts`): it stops the stacks and the
 *  stubbed layer's static file server that `e2e/global-setup.ts` started, whatever the run did. A
 *  run that started no stack - the stubbed layer alone, or a run against servers a person started
 *  - stops no stack here, since the pool holds nothing to stop. Playwright runs this hook even
 *  when the setup hook failed part of the way (it queues each teardown before it runs the setup,
 *  node_modules/.bun/playwright@1.64.1/node_modules/playwright/lib/runner/index.js:6340-6341), so
 *  a server the setup started before its failure is stopped here too.
 *
 *  It runs on a Ctrl-C as well as at the end of a run, which is what keeps an interrupted run
 *  from leaving a Postgres and a Go server serving loopback ports.
 *
 *  Whether this run's stack logs are kept is the one decision this hook cannot read from
 *  Playwright, which hands a teardown the configuration alone and runs it before it asks a
 *  reporter for the run's status. `e2e/run-outcome.ts` is where the answer comes from, and says
 *  why it answers "did this run pass" rather than "did a row fail": the logs are kept unless the
 *  run SHOWED that it passed, so a run no reporter of ours loaded for keeps them. What is kept
 *  is each stack's log directory, the only record of what its servers said, which a run of more
 *  than one stack writes and a run of one does not have at all; the data directories, which are
 *  the bulk of what the run wrote, go either way (`e2e/harness-pool.ts`).
 *
 *  It also removes the manifest `e2e/global-setup.ts` wrote, because a second setup in this same
 *  process must read the environment the first one found. Playwright's UI mode is that process:
 *  a re-run tears the stacks down and starts them again without leaving the runner
 *  (node_modules/.bun/playwright@1.64.1/node_modules/playwright/lib/runner/index.js:6896-6915),
 *  and a manifest left behind would name the stopped stacks' ports and databases. */
import { stopStacks } from "./harness-pool";
import { stacksVariable } from "./harness-ports";
import { runPassed } from "./run-outcome";
import { stopStubServer } from "./stub/server";

export default async function globalTeardown(): Promise<void> {
  await Promise.all([stopStacks(!runPassed()), stopStubServer()]);
  delete process.env[stacksVariable];
}
