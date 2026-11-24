// A record of the timers a program arms, for a test that holds the program's way to a fact to the
// delays the program chose on that way, rather than to how long the way took: a loaded machine
// stretches a program's time as much as a test's, so no verdict compares two readings of a clock
// (docs/testing.md, R2 and R4), while a delay a timer was armed with is a number the program
// chose, and a load cannot change it. A defect that makes the program wait - a timer of its own
// on the way, a sleep, a deadline - shows here with the delay it waits, whatever the machine does.
//
// It wraps `setTimeout`, `setInterval`, `Bun.sleep` and `AbortSignal.timeout` of the process it is
// installed in. A timer is the program's own when the first frame of the arming stack that names
// a file, past this module and past a compiled Bun executable's own bundle (Oh My Pi's host, which
// arms a timer for the plugin that called it), lies under one of the roots the test names, and
// is no test file by its path below that root. A library's timers (nats.js's pings, say), the
// host's own and the test's are left out, since that frame of theirs is the library's, none or
// the test's. An arming whose stack names no file at all is recorded as one the record cannot
// place, and a window that holds one is refused (`timersBetween`): a sum the record builds never
// leaves out a timer it could not read. One reading it cannot make: Bun's JavaScriptCore runs a
// call in tail position of a strict-mode function (`return setTimeout(...)`, or a `const` returned
// at once, which the transpiler folds into one) with that function's frame gone, so such an arming
// reads as its caller's; program code that arms a timer that way for a test's direct call is read
// as the test's and left out.

import { appendFileSync } from "node:fs";

/** Where the preload (`./timer-record-preload`) appends the record, one JSON line a step. */
export const TIMER_RECORD_FILE = "LEGION_TIMER_RECORD_FILE";
/** The roots of the program's own code, for the preload: absolute directories, `:`-separated. */
export const TIMER_RECORD_ROOTS = "LEGION_TIMER_RECORD_ROOTS";

/** How often the preload's writer stamps the record as alive (`{ alive, canary }` steps). The
 *  stamp is how a reader tells a record still being written from one whose writer stopped: a
 *  window of no timers reads the same in both, and only a later stamp shows the writer was there to
 *  write one. The period sets how soon a wait for a stamp ends, never a verdict. */
export const TIMER_RECORD_ALIVE_MS = 100;

/** The timer calls the record wraps. */
const WRAPPED_APIS = ["setTimeout", "setInterval", "Bun.sleep", "AbortSignal.timeout"] as const;
type WrappedApi = (typeof WRAPPED_APIS)[number];
/** The ways to a timer the canary goes, by the names it reports them: each wrapped call, and
 *  `node:timers/promises`, whose `setTimeout` keeps the global `setTimeout` it found when the
 *  module first loaded (measured on Bun 1.4.4), so a process that loaded it before the record holds
 *  a way to a timer the wrap never sees. */
export const TIMER_APIS = [...WRAPPED_APIS, "node:timers/promises"] as const;
export type TimerApi = (typeof TIMER_APIS)[number];

/** One step of the record: a timer the program armed, with the delay it chose and the line that
 *  armed it; a timer whose arming stack named no file, with its delay; that timer's callback
 *  starting (an interval's, at each tick; a sleep's, as it resolves; a deadline's, as its signal
 *  aborts); the preload's writer stamping the record as alive, with the timer calls whose wrap saw
 *  its canary (`probeCanary`); or a mark a test wrote into the record (`markRecord`). Timers are
 *  numbered in the order they were armed; stamps, in the order they were written. */
export type TimerStep =
  | { readonly timer: number; readonly delayMs: number; readonly from: string }
  | { readonly unplaced: number; readonly delayMs: number }
  | { readonly fired: number }
  | { readonly alive: number; readonly canary: readonly TimerApi[] }
  | { readonly mark: string };

/** A timer of the record that was armed and fired between two steps of a record it shares. */
export interface TimerOnTheWay {
  readonly delayMs: number;
  readonly from: string;
}

/** True while `probeCanary` calls the timer API: a wrap that sees a call then notes it as seen
 *  rather than recording a timer. */
let canaryCalling = false;
const canarySeen = new Set<WrappedApi>();
/** `node:timers/promises`, once the preload's writer has loaded it after the wrap. */
let timersPromises: { readonly setTimeout: (ms: number) => Promise<unknown> } | undefined;

/** Runs `call` as a canary, and returns the wrapped calls whose wrap saw it. */
function canaryThrough(call: () => void): ReadonlySet<WrappedApi> {
  canarySeen.clear();
  canaryCalling = true;
  try {
    call();
  } finally {
    canaryCalling = false;
  }
  return new Set(canarySeen);
}

/**
 * Calls each timer API the record wraps once, through the process's globals as they stand now,
 * and `node:timers/promises`' `setTimeout`, and returns the ways whose call the wrap saw. A record
 * whose wrap was undone, or never installed, still has its writer and its stamps, and records no
 * timer: its window reads 0 ms whatever the program did. Only a call through each way tells that
 * apart, since a wrap that is gone, or one a way to a timer goes around, sees no call. Each call
 * is cleared or ends at once.
 */
export function probeCanary(): TimerApi[] {
  const wrapped = canaryThrough(() => {
    globalThis.clearTimeout(globalThis.setTimeout(() => undefined, 0));
    globalThis.clearInterval(globalThis.setInterval(() => undefined, 1_000_000));
    void Bun.sleep(0);
    AbortSignal.timeout(0);
  });
  const seen: TimerApi[] = WRAPPED_APIS.filter((api) => wrapped.has(api));
  const promises = timersPromises;
  if (
    promises !== undefined &&
    canaryThrough(() => {
      void promises.setTimeout(0);
    }).has("setTimeout")
  ) {
    seen.push("node:timers/promises");
  }
  return seen;
}

const thisFile = new URL(import.meta.url).pathname;

/** Where a compiled Bun executable's own sources live in its frames: Oh My Pi's host, say. */
const COMPILED_BUNDLE = "/$bunfs/";

/**
 * The patterns `frameLocation` runs over a frame of a stack, text this module does not control:
 * a row of `timer-record.test.ts` holds each to a match linear in the text's length, as recheck's
 * automaton checker reads it.
 */
export const FRAME_PATTERNS = {
  /** `:line` or `:line:column` at the frame's end, before one `)` and any whitespace. */
  tail: /:(\d+)(?::\d+)?\)?\s*$/,
  /** The location after `file:`, when its path starts at its third `/`. */
  fileUrlPath: /^\/\/\/[^?]/,
  /** Where a location's path starts: its first `/` that a character other than `?` follows. */
  pathStart: /\/[^?]/,
} as const;

/**
 * The file and line of one frame of a stack, or undefined for a frame that names no file. A
 * module loaded with a query after its path (Oh My Pi loads an extension as `file.ts?mtime=…`, a
 * test imports `./envoy.ts?nats-stall`) names the file without it. A frame of one of the runtime's
 * own `node:` modules names no file either: `node:timers/promises`' `setTimeout` arms through the
 * global one, and its frame (`node:timers/promises:40:35`) lies between the wrap and the code that
 * called it, so it is passed over for that code's frame rather than read as a path of its own.
 * The frame is read from its end, at a cost linear in its length: a pattern that looked for the
 * path from every `/` of the frame took time quadratic in a frame of many `/` and no line after
 * them.
 */
function frameLocation(frame: string): { readonly file: string; readonly at: string } | undefined {
  const tail = FRAME_PATTERNS.tail.exec(frame);
  const line = tail?.[1];
  if (tail === null || line === undefined) return undefined;
  // The location runs to the tail's `:` from the last `(`, `)` or `:` before it: `file:`'s, for a
  // URL, and `node:`'s, for one of the runtime's own modules.
  const head = frame.slice(0, tail.index);
  const opened = Math.max(head.lastIndexOf("("), head.lastIndexOf(")"), head.lastIndexOf(":"));
  if (head.endsWith("node:", opened + 1)) return undefined;
  const location = head.slice(opened + 1);
  // Its path starts at the `/` after `file://`, else at the first `/` that `pathStart` finds, and
  // ends at the query's `?`.
  const start =
    head.endsWith("file:", opened + 1) && FRAME_PATTERNS.fileUrlPath.test(location)
      ? 2
      : location.search(FRAME_PATTERNS.pathStart);
  if (start === -1) return undefined;
  const query = location.indexOf("?", start);
  const file = location.slice(start, query === -1 ? location.length : query);
  return { file, at: `${file}:${line}` };
}

/** Whether `path`, a file's path below the root it lies under (from that root's `/`), is a
 *  test's rather than the program's. */
function isTestPath(path: string): boolean {
  return (
    /\.test\.[cm]?[jt]sx?$/.test(path) || path.includes("/__tests__/") || path.includes("/test/")
  );
}

/**
 * Whose timer the arming `stack` (an `Error`'s `stack`, its first line the message) armed: the
 * program's, with the line that armed it, when the first frame that names a file, past this
 * module and past a compiled bundle's frames, lies under one of `prefixes` (each ending in `/`)
 * and is no test file below it; `"foreign"` when that frame is another's, or when only a compiled
 * bundle's frames name a file (the host's own timer); `"unplaced"` when no frame names a file.
 */
export function placeArming(
  stack: string,
  prefixes: readonly string[]
): { readonly from: string } | "foreign" | "unplaced" {
  let bundled = false;
  for (const frame of stack.split("\n").slice(1)) {
    const location = frameLocation(frame);
    if (location === undefined || location.file === thisFile) continue;
    if (location.file.startsWith(COMPILED_BUNDLE)) {
      bundled = true;
      continue;
    }
    const prefix = prefixes.find((candidate) => location.file.startsWith(candidate));
    if (prefix === undefined) return "foreign";
    return isTestPath(location.file.slice(prefix.length - 1)) ? "foreign" : { from: location.at };
  }
  return bundled ? "foreign" : "unplaced";
}

/** The stack of the arming call running now, every frame of it: a host's frames between the
 *  arming and the plugin that called it must not push the plugin's frame past the limit. */
function armingStack(): string {
  const limit = Error.stackTraceLimit;
  Error.stackTraceLimit = Number.POSITIVE_INFINITY;
  try {
    return new Error().stack ?? "";
  } finally {
    Error.stackTraceLimit = limit;
  }
}

/**
 * Installs the record in this process until the returned function is called: from then on each
 * timer whose arming code lies under one of `roots` (absolute directories) is handed to `write`
 * as it is armed, and again as its callback starts, and each arming whose stack names no file is
 * handed to it as unplaced. `write` runs synchronously inside the arming call, so a test that
 * appends its own steps to the same record reads one order of both.
 */
export function recordTimers(
  roots: readonly string[],
  write: (step: TimerStep) => void
): () => void {
  const prefixes = roots.map((root) => (root.endsWith("/") ? root : `${root}/`));
  const original = {
    setTimeout: globalThis.setTimeout,
    setInterval: globalThis.setInterval,
    sleep: Bun.sleep,
    abortTimeout: AbortSignal.timeout,
  };
  let armed = 0;
  /** Records the arming running now, when it is the program's or cannot be placed, and returns
   *  its number. A canary call (`probeCanary`) is noted as seen through `api` and not recorded. */
  const arm = (api: WrappedApi, delay: unknown): number | undefined => {
    if (canaryCalling) {
      canarySeen.add(api);
      return undefined;
    }
    const place = placeArming(armingStack(), prefixes);
    if (place === "foreign") return undefined;
    armed += 1;
    const delayMs = typeof delay === "number" && Number.isFinite(delay) ? Math.max(0, delay) : 0;
    write(
      place === "unplaced"
        ? { unplaced: armed, delayMs }
        : { timer: armed, delayMs, from: place.from }
    );
    return armed;
  };
  const firing =
    (timer: number, handler: (...args: unknown[]) => unknown) =>
    (...args: unknown[]): unknown => {
      write({ fired: timer });
      return handler(...args);
    };
  const wrapped = (api: WrappedApi, scheduler: typeof setTimeout | typeof setInterval) =>
    ((handler: unknown, delay?: unknown, ...args: unknown[]) => {
      const timer = typeof handler === "function" ? arm(api, delay) : undefined;
      const callback =
        timer === undefined ? handler : firing(timer, handler as (...args: unknown[]) => unknown);
      return (scheduler as (...args: unknown[]) => unknown)(callback, delay, ...args);
    }) as unknown;
  globalThis.setTimeout = wrapped("setTimeout", original.setTimeout) as typeof setTimeout;
  globalThis.setInterval = wrapped("setInterval", original.setInterval) as typeof setInterval;
  Bun.sleep = ((until: number | Date) => {
    const timer = typeof until === "number" ? arm("Bun.sleep", until) : undefined;
    const sleeping = original.sleep(until);
    return timer === undefined ? sleeping : sleeping.then(() => write({ fired: timer }));
  }) as typeof Bun.sleep;
  AbortSignal.timeout = (delay: number): AbortSignal => {
    const timer = arm("AbortSignal.timeout", delay);
    const signal = original.abortTimeout.call(AbortSignal, delay);
    if (timer !== undefined) {
      signal.addEventListener("abort", () => write({ fired: timer }), { once: true });
    }
    return signal;
  };
  return () => {
    globalThis.setTimeout = original.setTimeout;
    globalThis.setInterval = original.setInterval;
    Bun.sleep = original.sleep;
    AbortSignal.timeout = original.abortTimeout;
  };
}

/** The preload a process takes the record through, as `BUN_OPTIONS` names it. */
const PRELOAD_FILE = "timer-record-preload.ts";
/** The variable a compiled Bun program reads its runtime options from, the preload among them. */
const BUN_OPTIONS = "BUN_OPTIONS";

/**
 * Removes the record's two variables, and any `--preload` of `timer-record-preload.ts` from
 * `BUN_OPTIONS` (the variable itself once nothing else is left in it), from this process's
 * environment, which every child it starts inherits.
 */
function forgetRecordEnvironment(): void {
  delete process.env[TIMER_RECORD_FILE];
  delete process.env[TIMER_RECORD_ROOTS];
  const options = process.env[BUN_OPTIONS];
  if (options === undefined) return;
  const kept: string[] = [];
  const words = options.split(/\s+/).filter((word) => word !== "");
  for (let index = 0; index < words.length; index += 1) {
    const word = words[index] ?? "";
    const next = words[index + 1] ?? "";
    if (word === "--preload" && next.endsWith(PRELOAD_FILE)) {
      index += 1;
    } else if (!(word.startsWith("--preload=") && word.endsWith(PRELOAD_FILE))) {
      kept.push(word);
    }
  }
  if (kept.length === 0) delete process.env[BUN_OPTIONS];
  else process.env[BUN_OPTIONS] = kept.join(" ");
}

/**
 * Installs the record as the preload does, when `env` names a file (`TIMER_RECORD_FILE`) and the
 * program's roots (`TIMER_RECORD_ROOTS`): each step is appended to the file as one JSON line. A
 * write that fails is dropped, so the record never changes what the program does. The writer also
 * stamps the record as alive every `TIMER_RECORD_ALIVE_MS`, with the timer calls whose wrap saw
 * its canary at that moment (`probeCanary`; `stampAfter` reads the stamps). The stamp runs on the
 * process's own `setInterval`, taken before the record wraps it, so it is never one of the
 * program's timers; the interval is unref'd, so it keeps no process alive.
 *
 * Each step it writes carries this process's id (`pid`), and once it has read them it removes
 * both variables, and its own `--preload` from `BUN_OPTIONS`, from `process.env`: a child the
 * program starts inherits none of them, so it writes nothing into the record, and a child that is
 * handed them anyway writes steps under its own id, which a reader that takes one process's steps
 * (`stepsOf`) leaves out.
 *
 * The writer is `@legion/envoy-client/json-line-record`'s `jsonLineRecord` written again, and the
 * copy stays: that one shared form sits in `@legion/envoy-client` because the program seams that
 * write it ship there, and a shipped package cannot import this private one. This package depends
 * on no workspace package, and every other Bun package of `packages/` devDepends on it. An import
 * of `jsonLineRecord` here would make it depend on `@legion/envoy-client`, and through that on
 * `@legion/contracts` and `@legion/cipher-client`: a cycle with each of the three. The writer could
 * be shared only from a package of its own, published as a runtime dependency of
 * `@legion/envoy-client`, for one write that drops its failure.
 */
export function recordTimersFromEnvironment(env: Record<string, string | undefined>): void {
  const file = env[TIMER_RECORD_FILE];
  const roots = (env[TIMER_RECORD_ROOTS] ?? "").split(":").filter((root) => root !== "");
  if (file === undefined || file === "" || roots.length === 0) return;
  forgetRecordEnvironment();
  const realSetInterval = globalThis.setInterval;
  const pid = process.pid;
  const write = (step: TimerStep): void => {
    try {
      appendFileSync(file, `${JSON.stringify({ ...step, pid })}\n`);
    } catch {
      // The test's record only: a write it cannot make changes nothing the program does.
    }
  };
  recordTimers(roots, write);
  // Loaded here, after the wrap, and never by a static import, which would load it before the
  // wrap: it keeps the global `setTimeout` it finds when it first loads. In a process that loaded
  // it before the record, this returns that module, its timers go around the wrap, and the canary
  // says so.
  void import("node:timers/promises").then((module) => {
    timersPromises = module;
  });
  let alive = 0;
  realSetInterval(() => {
    alive += 1;
    write({ alive, canary: probeCanary() });
  }, TIMER_RECORD_ALIVE_MS).unref();
}

/** Whether `step`, read from a record it may share with other steps, is a timer's arming. */
function isArming(step: unknown): step is Extract<TimerStep, { timer: number }> {
  return typeof step === "object" && step !== null && "timer" in step && "delayMs" in step;
}

/** Whether `step` is the arming of a timer the record could not place. */
function isUnplaced(step: unknown): step is Extract<TimerStep, { unplaced: number }> {
  return typeof step === "object" && step !== null && "unplaced" in step && "delayMs" in step;
}

/** Whether `step` is a timer's callback starting. */
function isFiring(step: unknown): step is Extract<TimerStep, { fired: number }> {
  return typeof step === "object" && step !== null && "fired" in step;
}

/**
 * Appends a mark named `mark` to the record `file`, from the test's own process: the record is
 * appended to whole lines at a time by both, so a step the program wrote after the mark lies after
 * it in the file. A test marks the moments its window runs between, so it reads the window from a
 * later read of the record rather than from whatever an earlier read returned.
 */
export function markRecord(file: string, mark: string): void {
  appendFileSync(file, `${JSON.stringify({ mark } satisfies TimerStep)}\n`);
}

/** The index of the mark named `mark` in the record `steps`, or -1 when it holds none. */
export function markIndex(steps: readonly unknown[], mark: string): number {
  return steps.findIndex(
    (step) => typeof step === "object" && step !== null && "mark" in step && step.mark === mark
  );
}

/**
 * The first stamp of the record's writer in `steps` past index `index`, or undefined: that the
 * writer was alive, and the reader saw its writes, after the step at `index`. A record whose writer
 * stopped, or a read that returns an old copy, holds none however long one waits.
 */
export function stampAfter(
  steps: readonly unknown[],
  index: number
): Extract<TimerStep, { alive: number }> | undefined {
  return steps
    .slice(Math.max(0, index + 1))
    .find(
      (step): step is Extract<TimerStep, { alive: number }> =>
        typeof step === "object" &&
        step !== null &&
        "alive" in step &&
        "canary" in step &&
        Array.isArray(step.canary)
    );
}

/**
 * The timers of `steps` armed after the step at index `after` whose callback started before the
 * step at index `before`: the program's waits that can lie between the two, each with the delay
 * the program chose. A timer the program cleared, or one still pending at `before`, delayed
 * nothing on the way and is not among them. Throws where such a timer is one the record could
 * not place, since a sum without it could only read less than the way waited.
 */
export function timersBetween(
  steps: readonly unknown[],
  after: number,
  before: number
): TimerOnTheWay[] {
  const fired = new Set<number>();
  for (const step of steps.slice(0, Math.max(0, before))) {
    if (isFiring(step)) fired.add(step.fired);
  }
  const window = steps.slice(after + 1, Math.max(after + 1, before));
  const unplaced = window.filter(isUnplaced).filter((step) => fired.has(step.unplaced));
  if (unplaced.length > 0) {
    throw new Error(
      `the timer record could not place ${unplaced.length} timer(s) that ran on the way, whose ` +
        `arming stacks named no file: ${JSON.stringify(unplaced)}`
    );
  }
  return window
    .filter(isArming)
    .filter((step) => fired.has(step.timer))
    .map(({ delayMs, from }) => ({ delayMs, from }));
}

/**
 * The steps of the record `steps` that process `pid`'s writer wrote, and every mark a test wrote
 * into it, in order: what a record shared with another writer says of that one process. A step
 * with no `pid` is none of `pid`'s.
 */
export function stepsOf(steps: readonly unknown[], pid: number): unknown[] {
  return steps.filter(
    (step) =>
      typeof step === "object" &&
      step !== null &&
      ("mark" in step || ("pid" in step && step.pid === pid))
  );
}
