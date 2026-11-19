import { afterEach, beforeEach } from "bun:test";
import { writeSync } from "node:fs";
import path from "node:path";

/**
 * The preload `per-test-bound.ts` passes to the `bun test` it runs: it tells that script, on the
 * pipe at the file descriptor `LEGION_TEST_BOUND_FD` names, when each test begins and ends, with
 * the test's file and the time. A preload's hooks are the whole run's, the outermost scope's, so
 * the time between them holds the test's own `beforeEach` and `afterEach` hooks. `Bun.main` names
 * the file whose test is running. The write is synchronous, so the event is out before the test
 * goes on, even when its thread then blocks.
 */

const descriptor = Number(process.env.LEGION_TEST_BOUND_FD);
if (!Number.isSafeInteger(descriptor) || descriptor < 3) {
  throw new Error(
    "test/per-test-bound-preload.ts is loaded without the script that reads it: run `bun run test` (test/per-test-bound.ts)"
  );
}

const directory = process.cwd();

function tell(event: string): void {
  try {
    writeSync(
      descriptor,
      `${event}\t${Date.now()}${event === "start" ? `\t${path.relative(directory, Bun.main)}` : ""}\n`
    );
  } catch (error) {
    throw new Error(
      "test/per-test-bound.ts no longer reads this run's events, so no per-test bound holds this test",
      { cause: error }
    );
  }
}

beforeEach(() => tell("start"));
afterEach(() => tell("end"));
