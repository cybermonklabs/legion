/** What `e2e/global-teardown.ts` asks before it removes a run's stack logs, and the one thing
 *  that must not go wrong with it: a run nobody reported on must not read as a run that passed.
 *
 *  A `--reporter` on the command line REPLACES the configured reporter list rather than adding
 *  to it (`e2e/run-outcome.ts` cites the line), so a leg that names any reporter of its own
 *  loads `e2e/failure-reporter.ts` only by naming it too: `--reporter=list,blob` records no row
 *  here, and every leg of `.github/workflows/dispatch-browser.yaml` that writes a blob report -
 *  the shard, alone, embedder and two-process ones - therefore runs
 *  `--reporter=list,blob,./e2e/failure-reporter.ts`. A module that answered "did a row
 *  fail" off the reporter alone answers NO for a leg that loaded none, however red it was, and
 *  the teardown then deletes the logs its reader had come for. A `globalSetup` that threw
 *  before the first row is the same silence from the other end.
 *
 *  The three cases of the first test are the whole of the decision a run's ROWS make, and they
 *  run in one function on purpose: the module holds ONE tally per instance, and the runner loads
 *  one instance per process, which is what lets a reporter in the runner and a hook in the runner
 *  agree, so a case that wanted a fresh tally would be testing some other module. Each step only
 *  ever adds to what the one before it recorded. The error that belongs to no row is the second
 *  test, because a tally this one has already failed could not show that the error is what
 *  failed it.
 *
 *  Each test takes its tally from an import under a query of its own, and every query carries a
 *  key drawn once per evaluation of this file. Bun keeps one instance per specifier for the life
 *  of the process, and `bun test --rerun-each` evaluates this file again in that same process: a
 *  static import, or a fixed query, hands the second run the tally the first run left failed.
 */
import { expect, test } from "bun:test";
import { randomUUID } from "node:crypto";

import type * as RunOutcome from "../run-outcome";

/** This evaluation of the file's key, so that a second run of it names instances of its own. */
const evaluation = randomUUID();

/** An instance of the module that no other test and no earlier run of this file has recorded
 *  into. The specifier is built rather than written because nothing static can name a second
 *  instance of one module, which is the whole of what this function is for. */
async function tally(name: string): Promise<typeof RunOutcome> {
  return (await import(`../run-outcome?${name}-${evaluation}`)) as typeof RunOutcome;
}

test("a run has passed only once a row was recorded and no recorded row failed", async () => {
  const { recordRow, runPassed } = await tally("the-first-tally");

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
  // A tally of its own, because the test above leaves its own failed for good: the module is
  // imported under a second query, which Bun answers with a second instance of it. The rows run
  // in random order, and in either order a shared instance fails one of them rather than passing
  // this test vacuously: after the test above it reads red at the green assertion below, and
  // before it the error this test records leaves it red at the test above's second assertion.
  const fresh = await tally("a-second-tally");
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
