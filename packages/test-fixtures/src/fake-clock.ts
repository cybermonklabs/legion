// Waits for a test on Bun's fake clock (`jest.useFakeTimers`), which holds every timer the code
// sets and every clock it reads: `setTimeout`, `AbortSignal.timeout`, `Date.now`,
// `performance.now`, `process.hrtime` and `Bun.nanoseconds` alike. A row on it moves the clock
// itself, with `jest.advanceTimersByTime`, so what it reads of the code's timing is the code's
// own, to the millisecond, however loaded the machine; and real I/O (a socket, a file, a child
// process) still runs, on turns of the event loop, which the fake clock leaves alone. Bun moves
// the clock a millisecond even for `advanceTimersByTime(0)`, so nothing here advances it: a row
// that needs a timer the code set for 0 ms to run moves the clock that millisecond itself.

/** `Atomics.waitAsync` (ES2024), which the `lib` of the packages that use this module does not
 *  declare: the one timer Bun's fake clock does not hold, so a wait it ends is real time. */
const waitAsync = (
  Atomics as unknown as {
    readonly waitAsync: (
      array: Int32Array,
      index: number,
      value: number,
      timeoutMs: number
    ) => { readonly async: boolean; readonly value: Promise<string> | string };
  }
).waitAsync;

/** One turn of the real event loop: every promise the code has chained settles inside it, and I/O
 *  that has completed is read in. */
export function turn(): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setImmediate(resolve);
  return promise;
}

/** Settles once `ms` of real time have passed, on the fake clock as on the real one. */
export function realTime(ms: number): Promise<void> {
  const waited = waitAsync(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
  return waited.async ? (waited.value as Promise<string>).then(() => undefined) : Promise.resolve();
}

/** What {@link turnsUntil} is told beside its condition. */
export interface TurnsUntilOptions {
  /** How much real time may pass before the wait fails: a bound that names a hang, never a
   *  measure of the code. 30 s by default. */
  readonly boundMs?: number;
  /** What the row can say at the bound of where things stand, after `what`. */
  readonly state?: () => string;
  /** Done before each turn, such as moving the fake clock a millisecond for a wait on a timer of
   *  the code's; nothing by default. */
  readonly each?: () => void;
}

/**
 * Turns of the real event loop until `done` holds. No clock moves unless `each` moves it, so what
 * a row waits on here is real I/O and the code's own reaction to it. Real time bounds the wait
 * only to name a hang (`TurnsUntilOptions.boundMs`, read on `Atomics.waitAsync`, 30 s by default,
 * far past anything a loaded machine holds up); `done` is read after every turn before the bound
 * is. A wait that would not end fails with `still not <what> after <n> s of real time`.
 */
export async function turnsUntil(
  done: () => boolean,
  what: string,
  options: TurnsUntilOptions = {}
): Promise<void> {
  const boundMs = options.boundMs ?? 30_000;
  let expired = false;
  void realTime(boundMs).then(() => {
    expired = true;
  });
  for (;;) {
    if (done()) return;
    if (expired) {
      const state = options.state === undefined ? "" : ` (${options.state()})`;
      throw new Error(`still not ${what} after ${boundMs / 1000} s of real time${state}`);
    }
    options.each?.();
    await turn();
  }
}
