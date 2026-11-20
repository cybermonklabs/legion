import { afterEach, beforeEach } from "bun:test";

/**
 * One thing a row or a hook waits for: what it is, and the bound it must end within, in
 * milliseconds. A part with no bound of its own takes `Number.POSITIVE_INFINITY`: then only the
 * clock it runs on stops it (temporary-directories.ts's removal is one).
 */
export interface Wait {
  readonly part: string;
  readonly boundMs: number;
}

/**
 * What spawnWithin leaves between a process's kill and the end of the clock it runs on, in
 * milliseconds: bun's own 5 s. A process that would outlast its row's limit is killed this long
 * before it, so the row fails naming the process, never on bun's own timeout.
 */
export const ROW_MARGIN_MS = 5_000;

/**
 * A clock spawnWithin counts from: when it started, its limit, and what that limit is, in the
 * words a failure names it by.
 */
interface Clock {
  readonly started: number;
  readonly limitMs: number;
  readonly of: string;
}

/** The running row's clock (fileLimit), or a fixture hook's own (onHookClock); none between. */
let clock: Clock | undefined;

/**
 * The limit each test file states through fileLimit, by the file's path. Bun runs every file of
 * a package in one process and evaluates this module once, so one limit kept for the process
 * would reach the next file, which may state none; `Bun.main` names the file Bun runs, when the
 * file is evaluated and in each of its rows and hooks.
 */
const stated = new Map<string, number>();

/**
 * States `limitMs` as the calling file's limit, for every row and hook: the file passes the
 * answer to `setDefaultTimeout`. It registers, in the file's scope, the hooks that start each
 * row's clock at that limit and stop it after the row, so spawnWithin gives a process no more
 * than its row has left: a process that would outlast the row's limit is killed ROW_MARGIN_MS
 * before it, and the row fails naming that process, never on bun's own timeout. A hook that runs
 * between rows (a describe's beforeAll or afterAll) runs on no row's clock, so its processes get
 * their own bounds, not what an earlier row left. The fixture's hooks run on a clock of their own
 * at this limit (onHookClock).
 */
export function fileLimit(limitMs: number): number {
  stated.set(Bun.main, limitMs);
  beforeEach(() => {
    clock = { started: performance.now(), limitMs, of: `its row's limit of ${limitMs} ms` };
  });
  afterEach(() => {
    clock = undefined;
  });
  return limitMs;
}

/**
 * Runs `run` on a clock of its own, started now at the limit the running file states
 * (fileLimit), and then puts back the clock it found. A file that states no limit gives it no
 * clock, and then only a part's own bound stops a process, or, for a part with none, the CI
 * job's own timeout.
 *
 * It is for a hook of temporary-directories.ts. Bun times each hook apart from its row, and the
 * fixture's afterEach can run after fileLimit's own has stopped the row's clock (Bun runs an
 * inner scope's afterEach hooks first, and a scope's in the order they were registered), so a
 * hook's process is held to the file's limit from the hook's start, wherever the hook runs.
 */
export function onHookClock<T>(run: () => T): T {
  const found = clock;
  const limitMs = stated.get(Bun.main);
  clock =
    limitMs === undefined
      ? undefined
      : { started: performance.now(), limitMs, of: `the file's limit of ${limitMs} ms` };
  try {
    return run();
  } finally {
    clock = found;
  }
}

/**
 * `Bun.spawnSync(command, options)` for `wait`: the process is killed at `wait.boundMs`, or where
 * the clock it runs on leaves it less, and then the row or hook fails naming `wait.part`. With no
 * clock and no bound of its own, nothing here stops the process.
 */
export function spawnWithin(
  wait: Wait,
  command: string[],
  options: {
    readonly cwd?: string;
    readonly env?: Record<string, string | undefined>;
    readonly stdout?: "pipe";
    readonly stderr?: "pipe";
  } = {}
) {
  const running = clock;
  const left =
    running === undefined
      ? Number.POSITIVE_INFINITY
      : Math.floor(running.limitMs - (performance.now() - running.started) - ROW_MARGIN_MS);
  const timeout = Math.max(1, Math.min(wait.boundMs, left));
  const result = Bun.spawnSync(command, {
    ...options,
    stdout: "pipe",
    stderr: "pipe",
    ...(Number.isFinite(timeout) ? { timeout, killSignal: "SIGKILL" as const } : {}),
  });
  if (!result.exitedDueToTimeout) return result;
  if (running === undefined || timeout === wait.boundMs) {
    throw new Error(`${wait.part} did not end within its bound of ${wait.boundMs} ms`);
  }
  // A part with a bound of its own that the clock cut short names the limit that left it out; a
  // part with none had only the clock, so the clock alone is what it did not end within.
  const why = Number.isFinite(wait.boundMs)
    ? ": the limit counts less than the parts the row waits for"
    : "";
  throw new Error(`${wait.part} did not end within the ${timeout} ms ${running.of} left it${why}`);
}
