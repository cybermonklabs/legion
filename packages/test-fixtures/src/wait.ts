import { jest } from "bun:test";
import { type Deadline, deadlineFor } from "./deadline";

// The one wait of the Bun tests. A test waits here for what it cannot make happen itself, and no
// function takes a duration for a wait for a fact: each wait ends at its row's deadline, which the
// command's one guard sets, early enough that its own failure prints before Bun's timeout would.
//
// The deadline (`./deadline`) is the newest mark of the row-deadline preload plus the command's
// `--timeout`, less a margin of `max(1000 ms, guard / 20)`. Every `bun test` command in this
// repository loads that preload (`--preload @legion/test-fixtures/row-deadline`), which marks
// the start of each row, the end of each row and the start of a run: so every wait of a row, the
// second included, ends before the row's guard, and so does a wait in a `beforeAll`. Outside
// `bun test` (a proof's Bun driver) the deadline is `LEGION_PROOF_DEADLINE_MS`; with neither, a
// wait refuses to start.
//
// Each function's last parameter, `...extra: never[]`, takes nothing: its type refuses a planted
// duration, and at run time anything passed there throws, so a duration planted in a file `tsc`
// does not check fails too. Each refuses to wait under `jest.useFakeTimers()`: Bun's fake timers
// hold `Date.now()`, `performance.now()` and every timer still until the test advances them, so a
// wait that polled there would never end, and a wait that moved the fake clock would take the
// test's clock from the test.

/** How often `until` asks its probe again. It is this module's own; no caller sets it. */
const PACE_MS = 10;

/** The longest delay a timer takes: Bun, as Node does, fires a longer one after 1 ms. */
const LONGEST_TIMER_MS = 2 ** 31 - 1;

/** A value `if` takes as true. */
export type Truthy<T> = Exclude<T, false | 0 | 0n | "" | null | undefined>;

/**
 * Waits until `probe` returns a truthy value, and resolves with it. `probe` may return a promise.
 * The wait asks at once, then at this module's pace; past its deadline it asks once more and
 * fails, naming `what` and the last value `probe` returned. A probe that throws, or whose promise
 * rejects, ends the wait at once with that error: a probe that reads a report which makes the fact
 * impossible (a process that exited) throws, rather than leave the wait to run to its deadline.
 */
export async function until<T>(
  what: string,
  probe: () => T | PromiseLike<T>,
  ...extra: never[]
): Promise<Truthy<Awaited<T>>> {
  refuseExtra("until(what, probe)", extra);
  if (typeof probe !== "function") throw new TypeError("until's probe must be a function");
  const deadline = begin("until", what);
  let last = "the probe had returned nothing yet";
  for (;;) {
    const reading = await beforeDeadline(Promise.resolve(probe()), deadline);
    if (reading === undefined) {
      throw gaveUp("until", what, deadline, `the probe's promise had not settled; ${last}`);
    }
    if (isTruthy(reading.value)) return reading.value;
    last = `last read: ${Bun.inspect(reading.value)}`;
    // A clock the test held while the probe ran would hold this wait's own timer still.
    refuseFakeTimers("until");
    const left = deadline.at - performance.now();
    if (left <= 0) throw gaveUp("until", what, deadline, last);
    await Bun.sleep(Math.min(PACE_MS, left));
  }
}

/**
 * Waits for `promise` to settle, and resolves with its value or rejects with its reason. Past its
 * deadline it fails, naming `what`.
 */
export async function settled<T>(
  what: string,
  promise: PromiseLike<T>,
  ...extra: never[]
): Promise<T> {
  refuseExtra("settled(what, promise)", extra);
  const deadline = begin("settled", what);
  const outcome = await beforeDeadline(Promise.resolve(promise), deadline);
  if (outcome === undefined) {
    throw gaveUp("settled", what, deadline, "the promise had not settled");
  }
  return outcome.value;
}

/**
 * Holds the test for `ms`, for the reason `why`: the one named span of a wait that proves an
 * absence. A test proves that something does not happen from the state that would make it happen,
 * or by waiting for the effect of a later action; `why` says why neither reaches the code. A span
 * that would end past the row's deadline is refused at once.
 */
export async function span(ms: number, why: string, ...extra: never[]): Promise<void> {
  refuseExtra("span(ms, why)", extra);
  requireSpan("span", ms, why);
  const deadline = begin("span", why);
  refuseSpanPastDeadline("span", ms, why, deadline);
  await Bun.sleep(ms);
}

/**
 * Holds the test for `ms`, for the reason `why`, and fails, naming `what` and the value, when
 * `events` yields within it; a source that ends without yielding is quiet. `quiet` iterates
 * `events` itself and ends that iteration when it returns, so it takes a source of its own: an
 * iterator that hands a value to quiet hands it to no one else.
 */
export async function quiet<T>(
  what: string,
  events: AsyncIterable<T>,
  ms: number,
  why: string,
  ...extra: never[]
): Promise<void> {
  refuseExtra("quiet(what, events, ms, why)", extra);
  requireSpan("quiet", ms, why);
  const deadline = begin("quiet", what);
  refuseSpanPastDeadline("quiet", ms, why, deadline);
  const iterator = events[Symbol.asyncIterator]();
  const spanEnds = Promise.withResolvers<undefined>();
  const timer = setTimeout(spanEnds.resolve, ms, undefined);
  try {
    const first = await Promise.race([iterator.next(), spanEnds.promise]);
    if (first !== undefined && first.done !== true) {
      throw new Error(
        `quiet: ${what} happened within the ${ms} ms span (${why}): ${Bun.inspect(first.value)}`
      );
    }
  } finally {
    clearTimeout(timer);
    // Not awaited: an async generator suspended in the read above runs its `return` only after
    // that read settles, which a quiet source may never do.
    void iterator.return?.();
  }
}

/**
 * Milliseconds left before the current deadline, 0 once it has passed, and at most the longest
 * delay a timer takes, so it can be handed to one: under `bun test --timeout 0` there is no
 * deadline, and that is what it returns.
 */
export function timeLeft(...extra: never[]): number {
  refuseExtra("timeLeft()", extra);
  refuseFakeTimers("timeLeft");
  const now = performance.now();
  return Math.min(LONGEST_TIMER_MS, Math.max(0, deadlineFor(now, "timeLeft").at - now));
}

function isTruthy<T>(value: T): value is Truthy<T> {
  return Boolean(value);
}

function refuseExtra(signature: string, extra: readonly unknown[]): void {
  if (extra.length === 0) return;
  throw new TypeError(
    `${signature} was given ${extra.length} argument${extra.length === 1 ? "" : "s"} more than ` +
      `it takes (${extra.map((argument) => Bun.inspect(argument)).join(", ")}). No wait takes a ` +
      "duration: each ends at its row's deadline, which the command's --timeout sets " +
      "(@legion/test-fixtures/wait)."
  );
}

function refuseFakeTimers(caller: string): void {
  if (!jest.isFakeTimers()) return;
  throw new Error(
    `${caller} refuses to wait under jest.useFakeTimers(): the fake clock holds Date.now(), ` +
      "performance.now() and every timer still, so the wait would never reach its deadline. " +
      "Advance the fake clock to the moment the fact happens (jest.advanceTimersByTime), then " +
      "read it."
  );
}

function requireSpan(caller: string, ms: number, why: string): void {
  if (typeof ms !== "number" || !Number.isFinite(ms) || ms < 0) {
    throw new TypeError(
      `${caller}'s span must be a number of milliseconds, not ${Bun.inspect(ms)}`
    );
  }
  if (typeof why !== "string" || why.trim() === "") {
    throw new TypeError(
      `${caller} needs a why: which proof from state, or from a later action, does not reach ` +
        "the code"
    );
  }
}

/** Checks what a wait is for and that it may wait, and returns the deadline it ends by. */
function begin(caller: string, what: string): Deadline {
  if (typeof what !== "string" || what.trim() === "") {
    throw new TypeError(`${caller} needs a what that names the fact it waits for`);
  }
  refuseFakeTimers(caller);
  return deadlineFor(performance.now(), caller);
}

function refuseSpanPastDeadline(caller: string, ms: number, why: string, deadline: Deadline): void {
  const left = deadline.at - performance.now();
  if (ms <= left) return;
  throw new Error(
    `${caller}: a span of ${ms} ms (${why}) would end after the deadline, ${deadline.source}: ` +
      `${Math.max(0, Math.floor(left))} ms are left`
  );
}

/** `promise`'s value, or undefined when the deadline passes before it settles. */
async function beforeDeadline<T>(
  promise: Promise<T>,
  deadline: Deadline
): Promise<{ readonly value: T } | undefined> {
  const left = deadline.at - performance.now();
  // A deadline further off than a timer reaches is none this process will see.
  if (left > LONGEST_TIMER_MS) return { value: await promise };
  const passed = Promise.withResolvers<undefined>();
  const timer = setTimeout(passed.resolve, Math.max(0, left), undefined);
  try {
    return await Promise.race([promise.then((value) => ({ value })), passed.promise]);
  } finally {
    clearTimeout(timer);
  }
}

function gaveUp(caller: string, what: string, deadline: Deadline, detail: string): Error {
  return new Error(`${caller} gave up waiting for ${what}, ${deadline.source}; ${detail}`);
}
