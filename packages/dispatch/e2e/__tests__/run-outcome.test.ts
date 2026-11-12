/** What `e2e/global-teardown.ts` asks before it removes a run's stack logs, and the one thing
 *  that must not go wrong with it: a run nobody reported on must not read as a run that passed.
 *
 *  A `--reporter` on the command line REPLACES the configured reporter list rather than adding
 *  to it (`e2e/run-outcome.ts` cites the line), so a leg that names any reporter of its own
 *  loads `e2e/failure-reporter.ts` only by naming it too: `--reporter=list,blob` records no row
 *  here, and the shard and alone legs of `.github/workflows/dispatch-browser.yaml` therefore
 *  run `--reporter=list,blob,./e2e/failure-reporter.ts`. A module that answered "did a row
 *  fail" off the reporter alone answers NO for a leg that loaded none, however red it was, and
 *  the teardown then deletes the logs its reader had come for. A `globalSetup` that threw
 *  before the first row is the same silence from the other end.
 *
 *  The three cases of the first test are the whole of the decision a run's ROWS make, and they
 *  run in one function on purpose: the module holds ONE tally per process, which is what lets a
 *  reporter in the runner and a hook in the runner agree, so a case that wanted a fresh tally
 *  would be testing some other module. Each step only ever adds to what the one before it
 *  recorded. The error that belongs to no row is the second test, because a tally this one has
 *  already failed could not show that the error is what failed it.
 */
import { expect, test } from "bun:test";

import type * as RunOutcome from "../run-outcome";
import { recordRow, runPassed } from "../run-outcome";

test("a run has passed only once a row was recorded and no recorded row failed", () => {
  // No row recorded. The reporter never loaded, or the run died in `globalSetup`: the logs are
  // the only account of what happened, so this is NOT a pass.
  expect(
    runPassed(),
    "A run that recorded no row must not read as green: `--reporter=list,blob` loads none of " +
      "this config's reporters, and deleting that leg's stack logs is the one failure this " +
      "module exists to prevent."
  ).toBe(false);

  // A row recorded, and it passed. This is the run whose megabytes of stack logs nobody opens.
  recordRow(false);
  expect(runPassed()).toBe(true);

  // A row recorded that failed. One is enough, whatever the rows around it did, and a row that
  // failed and passed on its retry is recorded here as a failure too.
  recordRow(true);
  expect(runPassed()).toBe(false);
});

test("an error that belongs to no row fails a run whose every row passed", async () => {
  // A tally of its own, because the test above has failed this process's for good: the module
  // is imported a second time under a query, which Bun answers with a second instance of it.
  // The green assertion below is also the check that it IS a second instance - the first reads
  // red from here on, so a shared one would fail that line rather than pass this test vacuously.
  // The specifier is built rather than written because nothing static can name a second
  // instance of one module, which is the whole of what this line is for.
  const fresh = (await import(`../run-outcome?${"a-second-tally"}`)) as typeof RunOutcome;
  fresh.recordRow(false);
  expect(fresh.runPassed(), "the import under a query answered with the first tally").toBe(true);

  // A worker-scoped fixture whose teardown threw, or another failure the runner has no row to
  // put on: the run fails and no row ends, so `e2e/failure-reporter.ts`'s `onError` is the only
  // thing that reports it. Without it the teardown reads this run as green and deletes the
  // stack logs that are the whole account of why it failed.
  fresh.recordError();
  expect(
    fresh.runPassed(),
    "A run whose only failure reached `onError` must not read as green: a failure that belongs " +
      "to no row still fails the run, and the logs of that run are what its reader came for."
  ).toBe(false);
});
