import { afterEach, beforeEach } from "bun:test";
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
 * test that never ends in CI therefore holds the step until the job's own limit cancels the job:
 * nothing in the log fails, and a job cancelled at its limit can lose its log altogether.
 *
 * The watch is told, at each test, which file it is in and when it begins and ends, and judges by its
 * own clock: a test still running {@link PER_TEST_BOUND_MS} after it began, or no test beginning
 * that long after the last one ended, is reported with the file, the test's place in it and the
 * processes under `bun test`, and every one of them is killed. The time between `beforeEach` and
 * `afterEach` here holds the test's own hooks too, since these hooks are the outermost scope's.
 */
/**
 * Twice the slowest correct test, taken up to 80 s from its 77.4 s. The slowest is
 * `extensions/legion-stall-watchdog-omp.test.ts`'s third, "a call the abort does not end", which
 * took 73.2 s to 77.4 s in the pi-legion CI job's own runs of this suite at nine commits of
 * 2026-11-18; the next is the same file's eighth, at 41.4 s. With the suite's own 12 to 13 minutes,
 * a run that one test holds up ends inside the job's 28-minute limit, failed and with its log.
 */
export const PER_TEST_BOUND_MS = 160_000;

/** The bound in force: `LEGION_TEST_BOUND_MS` when it is set, which `per-test-bound.test.ts`
 * sets to run its control in seconds, and {@link PER_TEST_BOUND_MS} otherwise. */
const boundMs = Number(process.env.LEGION_TEST_BOUND_MS ?? PER_TEST_BOUND_MS);
if (!Number.isSafeInteger(boundMs) || boundMs <= 0) {
  throw new Error(`LEGION_TEST_BOUND_MS must be a positive integer of ms, not ${boundMs}`);
}

const watch = Bun.spawn(
  [
    process.execPath,
    path.join(import.meta.dir, "per-test-bound-watch.ts"),
    String(process.pid),
    String(boundMs),
  ],
  { stdin: "pipe", stdout: "inherit", stderr: "inherit" }
);
// `bun test` exits when its tests are done whatever it has spawned; the watch ends on its own
// once this end of the pipe closes.
watch.unref();

/** Sends one event to the watch, flushed before the test or hook goes on, so the watch has it
 * even when the thread blocks right after. A watch that has exited holds no bound, so every test
 * after it fails here rather than running unbounded. */
function tell(event: string): void {
  if (watch.exitCode !== null || watch.signalCode !== null) {
    throw new Error(
      `the per-test bound's watch (test/per-test-bound-watch.ts) has exited (${watch.signalCode ?? `status ${watch.exitCode}`}), so no bound holds this test`
    );
  }
  watch.stdin.write(`${event}\n`);
  watch.stdin.flush();
}

/** The test file the last test ran in, and that test's place in it, counting from 1 the tests
 * that ran there. A hook of a preload is the whole run's, not a file's: its `beforeAll` runs once,
 * before the first file. So the file is read at each test, from `Bun.main`, which names the file
 * whose test is running. */
let file = "";
let place = 0;

beforeEach(() => {
  if (Bun.main !== file) {
    file = Bun.main;
    place = 0;
    tell(`file\t${path.relative(process.cwd(), file)}`);
  }
  place += 1;
  tell(`start\t${place}`);
});
afterEach(() => tell(`end\t${place}`));
