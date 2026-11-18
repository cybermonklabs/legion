import { afterEach, beforeAll, beforeEach } from "bun:test";
import path from "node:path";

/**
 * The bound on each test of this suite, held by a process of its own (`per-test-bound-watch.ts`),
 * which names the test that runs past it and ends the run. `bunfig.toml` preloads this file into
 * every `bun test` run in this directory.
 *
 * Bun's own per-test timeout is a timer on the thread that runs the test, so it fires only while
 * that thread is free. Measured on Bun 1.4.4: a test blocked in a synchronous read of a FIFO, in
 * `Atomics.wait` or in a loop, and a test file whose module body awaits a promise that never
 * settles, run until something outside kills them, and a `bun test` that is sent SIGINT or SIGTERM
 * prints nothing of the test it was running. Where Bun's timer does fire, a test's own timeout
 * argument wins over `--timeout`, and this suite gives its real-Oh-My-Pi tests up to 600 s each. A
 * test that never ends in CI therefore runs until the job's own limit cancels the job, and that log
 * does not name it.
 *
 * The watch is told when each file begins and when each test begins and ends, and judges by its
 * own clock: a test still running {@link PER_TEST_BOUND_MS} after it began, or no test beginning
 * that long after the last one ended, is reported with the file, the test's place in it and the
 * processes under `bun test`, and every one of them is killed. The time between `beforeEach` and
 * `afterEach` here holds the test's own hooks too, since these hooks are the outermost scope's.
 */
export const PER_TEST_BOUND_MS = 480_000;

/** The bound in force: `LEGION_TEST_BOUND_MS` when it is set, which `per-test-bound.test.ts`
 * sets to run its control in seconds, and {@link PER_TEST_BOUND_MS} otherwise. */
const boundMs = Number(process.env.LEGION_TEST_BOUND_MS ?? PER_TEST_BOUND_MS);
if (!Number.isSafeInteger(boundMs) || boundMs <= 0) {
  throw new Error(`LEGION_TEST_BOUND_MS must be a positive integer of ms, not ${boundMs}`);
}

const watch = Bun.spawn(
  [process.execPath, path.join(import.meta.dir, "per-test-bound-watch.ts"), String(process.pid), String(boundMs)],
  { stdin: "pipe", stdout: "inherit", stderr: "inherit" }
);
// `bun test` exits when its tests are done whatever it has spawned; the watch ends on its own
// once this end of the pipe closes.
watch.unref();

/** Sends one event to the watch, flushed before the test or hook goes on, so the watch has it
 * even when the thread blocks right after. */
function tell(event: string): void {
  watch.stdin.write(`${event}\n`);
  watch.stdin.flush();
}

/** Each test's place in its file, counting from 1 the tests that run there. */
let place = 0;

beforeAll(() => {
  place = 0;
  tell(`file\t${path.relative(process.cwd(), Bun.main)}`);
});
beforeEach(() => {
  place += 1;
  tell(`start\t${place}`);
});
afterEach(() => tell(`end\t${place}`));
