import { realTimeDeadline } from "./deadline";

// Waits for a test on Bun's fake clock (`jest.useFakeTimers`), which holds every timer the code
// sets and every clock it reads: `setTimeout`, `AbortSignal.timeout`, `Date.now`,
// `performance.now`, `process.hrtime` and `Bun.nanoseconds` alike. A row on it moves the clock
// itself, with `jest.advanceTimersByTime`, so what it reads of the code's timing is the code's
// own, to the millisecond, however loaded the machine; and real I/O (a socket, a file, a child
// process) still runs, on turns of the event loop, which the fake clock leaves alone. Bun moves
// the clock a millisecond even for `advanceTimersByTime(0)`, so nothing here advances it: a row
// that needs a timer the code set for 0 ms to run moves the clock that millisecond itself.

/** One turn of the real event loop: every promise the code has chained settles inside it, and I/O
 *  that has completed is read in. */
export function turn(): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setImmediate(resolve);
  return promise;
}

/** What {@link turnsUntil} is told beside its condition. None is a bound. */
export interface TurnsUntilOptions {
  /** What the row can say, at its deadline, of where things stand, after `what`. */
  readonly state?: () => string;
  /** Done before each turn, such as moving the fake clock a millisecond for a wait on a timer of
   *  the code's; nothing by default. */
  readonly each?: () => void;
  /** How one turn is taken, {@link turn} by default: a React row takes each turn inside `act`, so
   *  what the turn set going renders, inside `act`, before `done` reads it. */
  readonly turn?: () => Promise<void>;
}

const OPTIONS: readonly string[] = ["state", "each", "turn"];

/**
 * Turns of the real event loop until `done` holds, the shape of `@legion/test-fixtures/wait`'s
 * `until` on the fake clock. No clock moves unless `each` moves it, so what a row waits on here is
 * real I/O and the code's own reaction to it. The wait takes no bound of its own: it ends at its
 * row's deadline, as every wait of `./wait` does, read in real time (`./deadline`'s
 * `realTimeDeadline`), since the fake clock holds every clock the row could read; `done` is read
 * after every turn before the deadline is. A wait that would not end fails with `turnsUntil gave
 * up waiting for <what>, <deadline>` and what `state` says. Its last parameter, as `./wait`'s,
 * takes nothing: a planted duration fails its type, and throws at run time, as an option other
 * than `state`, `each` and `turn` does.
 */
export async function turnsUntil(
  what: string,
  done: () => boolean,
  options: TurnsUntilOptions = {},
  ...extra: never[]
): Promise<void> {
  const unknown = Object.keys(options).filter((name) => !OPTIONS.includes(name));
  if (extra.length > 0 || unknown.length > 0 || typeof done !== "function") {
    throw new TypeError(
      `turnsUntil(what, done, { state, each, turn }) was given ${Bun.inspect([...unknown, ...extra])} ` +
        "beside what it takes, or a done that is no function. No wait takes a duration: each " +
        "ends at its row's deadline, which the command's --timeout sets (@legion/test-fixtures/wait)."
    );
  }
  if (typeof what !== "string" || what.trim() === "") {
    throw new TypeError("turnsUntil needs a what that names the fact it waits for");
  }
  const deadline = realTimeDeadline("turnsUntil");
  for (;;) {
    if (done()) return;
    if (deadline.passed()) {
      const state = options.state === undefined ? "" : ` (${options.state()})`;
      throw new Error(`turnsUntil gave up waiting for ${what}, ${deadline.source}${state}`);
    }
    options.each?.();
    await (options.turn ?? turn)();
  }
}
