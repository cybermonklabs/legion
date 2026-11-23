import { existsSync, readFileSync } from "node:fs";

// The deadline every wait of `./wait` ends by, and the marks `./row-deadline` takes to set it. This
// module is no export of the package: a test of another package reaches it only through those two
// and `./fake-clock`.

/** Bun's own `--timeout` when a `bun test` command line sets none (`bun test --help`). */
const BUN_DEFAULT_GUARD_MS = 5_000;

/** What the process's own command line says about the runner it is. */
export interface Runner {
  /** Whether the process is `bun test`, or one of the workers `bun test --parallel` starts. */
  readonly inBunTest: boolean;
  /** That command's `--timeout` in milliseconds, Bun's default when it sets none; 0 is no guard. */
  readonly guardMs: number;
}

/** When a wait gives up, and the words its failure says that with. */
export interface Deadline {
  /** The moment on `performance.now()`'s clock; Infinity under `bun test --timeout 0`. */
  readonly at: number;
  readonly source: string;
}

/**
 * The runner a command line names. The command is the first argument after the program that is
 * no flag: `bun test …`, and `bun test --test-worker …` for a `--parallel` worker. The guard is
 * the last `--timeout=<ms>` or `--timeout <ms>`; a worker's command line carries it as the first
 * form whether or not the person's command set it (measured on Bun 1.4.4).
 */
export function runnerFrom(args: readonly string[]): Runner {
  const inBunTest = args.slice(1).find((arg) => !arg.startsWith("-")) === "test";
  let timeout: string | undefined;
  for (const [index, arg] of args.entries()) {
    if (arg === "--timeout") timeout = args[index + 1];
    else if (arg.startsWith("--timeout=")) timeout = arg.slice("--timeout=".length);
  }
  if (timeout === undefined) return { inBunTest, guardMs: BUN_DEFAULT_GUARD_MS };
  const guardMs = Number(timeout);
  if (!Number.isInteger(guardMs) || guardMs < 0) {
    throw new Error(`the command line's --timeout is ${JSON.stringify(timeout)}, not milliseconds`);
  }
  return { inBunTest, guardMs };
}

/**
 * A process's command line as `ps` prints it, for a host with no `/proc` (macOS). `-ww` asks for
 * the whole line, which a `ps` that sizes its output to a terminal would otherwise cut. `ps` is
 * the program asked: its test hands it a stand-in, since procps-ng's `ps -p` reads every process
 * on the machine (docs/testing.md, "A test that looks for a process it started").
 */
export function psCommandLine(pid: number, ps = "ps"): string[] {
  const run = Bun.spawnSync([ps, "-ww", "-o", "args=", "-p", String(pid)]);
  if (run.exitCode !== 0) {
    throw new Error(
      `could not read the command line of process ${pid}: \`${ps} -ww -o args= -p ${pid}\` ` +
        `exited ${run.exitCode}: ${run.stderr.toString().trim()}`
    );
  }
  return run.stdout.toString().trim().split(/\s+/);
}

/** This process's command line: `/proc/self/cmdline`, or `ps` where the host has no `/proc`. */
function ownCommandLine(): string[] {
  // `Bun.argv` and `process.argv` hold neither `test` nor `--timeout` under `bun test`: they name
  // the test file being run (measured on Bun 1.4.4).
  if (!existsSync("/proc/self/cmdline")) return psCommandLine(process.pid);
  return readFileSync("/proc/self/cmdline", "utf8")
    .split("\0")
    .filter((arg) => arg !== "");
}

let runner: Runner | undefined;

/** The newest mark `./row-deadline` took, on `performance.now()`'s clock. */
let newestMark: number | undefined;

/**
 * A real-time alarm: `passed` turns true, and `reached` resolves, once its delay has gone by in
 * real time. Bun's fake clock (`jest.useFakeTimers()`) holds every clock a row could read,
 * `performance.now()`, `Date.now()`, `process.hrtime` and `Bun.nanoseconds()` alike, and every
 * timer the row sets (measured on Bun 1.4.4), but not the timeout of `Atomics.waitAsync`, which the
 * alarm waits on. A pending alarm holds no process open. An alarm woken early (a later mark's)
 * never passes, and its `reached` never resolves.
 */
interface Alarm {
  readonly cell: Int32Array;
  passed: boolean;
  readonly reached: Promise<void>;
}

/** `Atomics.waitAsync` (ES2024)'s result: settled at the call, or a promise of how it ended. */
type WaitAsyncResult =
  | { readonly async: false; readonly value: string }
  | { readonly async: true; readonly value: Promise<string> };

/** `Atomics.waitAsync`, cast because the `lib` of the packages that use this module does not
 *  declare it (ES2024). */
const atomics = Atomics as unknown as {
  readonly waitAsync: (
    array: Int32Array,
    index: number,
    value: number,
    timeoutMs: number
  ) => WaitAsyncResult;
};

/** An alarm that passes once `ms` of real time have gone by. */
function alarmAfter(ms: number): Alarm {
  const cell = new Int32Array(new SharedArrayBuffer(4));
  const reached = Promise.withResolvers<void>();
  const alarm: Alarm = { cell, passed: false, reached: reached.promise };
  const waited = atomics.waitAsync(cell, 0, 0, Math.max(0, ms));
  if (!waited.async) {
    if (waited.value === "timed-out") {
      alarm.passed = true;
      reached.resolve();
    }
    return alarm;
  }
  void waited.value.then((outcome) => {
    if (outcome !== "timed-out") return;
    alarm.passed = true;
    reached.resolve();
  });
  return alarm;
}

/** The alarm of the newest mark's deadline, armed when the mark was taken; none under
 *  `bun test --timeout 0`. */
let markAlarm: Alarm | undefined;

/** Records now as the moment Bun last started the guard that the next wait runs under, and arms
 *  that guard's deadline in real time. The previous mark's alarm is woken, so it ends at once. */
export function markRow(): void {
  newestMark = performance.now();
  if (markAlarm !== undefined) Atomics.notify(markAlarm.cell, 0);
  const { at } = deadlineFor(newestMark, "the row-deadline preload");
  markAlarm = Number.isFinite(at) ? alarmAfter(at - newestMark) : undefined;
}

/**
 * The row's deadline as a wait under Bun's fake clock reads it (`./fake-clock`'s `turnsUntil`):
 * whether it has passed in real time, a promise that resolves when it does, and the words a
 * failure says that with. It is the newest mark's deadline, which the mark armed in real time when
 * it was taken, before the row's fake clock stood still; with no mark it counts from the call.
 * Under `bun test --timeout 0` it never passes. Outside `bun test` no fake clock runs, so it throws.
 */
export function realTimeDeadline(caller: string): {
  readonly passed: () => boolean;
  readonly reached: Promise<void>;
  readonly source: string;
} {
  runner ??= runnerFrom(ownCommandLine());
  if (!runner.inBunTest) {
    throw new Error(`${caller} waits on Bun's fake clock, which only \`bun test\` runs`);
  }
  const { at, source } = deadlineFor(newestMark ?? 0, caller);
  const never = new Promise<void>(() => undefined);
  if (!Number.isFinite(at)) return { passed: () => false, reached: never, source };
  const alarm = newestMark === undefined ? alarmAfter(at) : markAlarm;
  return { passed: () => alarm?.passed === true, reached: alarm?.reached ?? never, source };
}

/**
 * The deadline of a wait that starts at `start` (on `performance.now()`'s clock), `caller` being
 * the wait's name for the refusal.
 *
 * Under `bun test` it is the newest mark plus the command's guard, less a margin of
 * `max(1000 ms, guard / 20)` in which the wait's failure is reported and the row's own cleanups
 * run before Bun's timeout would fire. With no mark (no row-deadline preload) the wait's own start
 * is the mark. Outside `bun test` it is `LEGION_PROOF_DEADLINE_MS`, epoch milliseconds that a
 * proof's script sets for its Bun drivers, and with neither there is no deadline to end by, so it
 * throws.
 */
export function deadlineFor(start: number, caller: string): Deadline {
  runner ??= runnerFrom(ownCommandLine());
  if (runner.inBunTest) {
    const guard = runner.guardMs;
    if (guard === 0) {
      return { at: Number.POSITIVE_INFINITY, source: "with no deadline (bun test --timeout 0)" };
    }
    const margin = Math.max(1_000, guard / 20);
    const counted =
      newestMark === undefined
        ? ", counted from the wait's own start because no row-deadline preload took a mark"
        : "";
    return {
      at: (newestMark ?? start) + guard - margin,
      source: `${margin} ms before the row's guard of ${guard} ms (bun test --timeout)${counted}`,
    };
  }
  const proof = process.env["LEGION_PROOF_DEADLINE_MS"];
  if (proof === undefined || proof === "") {
    throw new Error(
      `${caller} has no deadline to end by: this process is not \`bun test\`, and ` +
        "LEGION_PROOF_DEADLINE_MS (epoch milliseconds, which a proof's script sets for its Bun " +
        "drivers) is not set"
    );
  }
  const deadline = new Date(Number(proof));
  if (Number.isNaN(deadline.getTime())) {
    throw new Error(`LEGION_PROOF_DEADLINE_MS is ${JSON.stringify(proof)}, not epoch milliseconds`);
  }
  return {
    at: deadline.getTime() - performance.timeOrigin,
    source: `at the proof's deadline ${deadline.toISOString()} (LEGION_PROOF_DEADLINE_MS)`,
  };
}
