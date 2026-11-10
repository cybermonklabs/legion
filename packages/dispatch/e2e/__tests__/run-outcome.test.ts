/** What `e2e/global-teardown.ts` asks before it removes a run's stack logs, and the one thing
 *  that must not go wrong with it: a run nobody reported on must not read as a run that passed.
 *
 *  A `--reporter` on the command line REPLACES the configured reporter list rather than adding
 *  to it (`e2e/run-outcome.ts` cites the line), so `--reporter=list,blob` - which is what the
 *  shard and alone legs of `.github/workflows/dispatch-browser.yaml` ran before this change -
 *  loads `e2e/failure-reporter.ts` not at all and records no row here. A module that answered
 *  "did a row fail" answered NO for such a leg however red it was, and the teardown deleted the
 *  logs its reader had come for. A `globalSetup` that threw before the first row is the same
 *  silence from the other end.
 *
 *  The three cases below are the whole of the decision, and they run in one function on purpose:
 *  the module holds ONE tally per process, which is what lets a reporter in the runner and a
 *  hook in the runner agree, so a case that wanted a fresh tally would be testing some other
 *  module. Each step only ever adds to what the one before it recorded. */
import { expect, test } from "bun:test";

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
