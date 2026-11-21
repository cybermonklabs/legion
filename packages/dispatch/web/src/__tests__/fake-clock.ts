import { jest } from "bun:test";
import { turn as realTurn, turnsUntil } from "@legion/test-fixtures/fake-clock";
import { act } from "@testing-library/react";

/**
 * Moves the fake clock `ms` on in steps of at most `step`, letting each timer's continuation and
 * React's updates run as it fires, and then always runs one last round of 0 ms. That round fires,
 * inside the same wait, what the last step's continuations scheduled at zero delay, whether `ms`
 * divides by `step` or not. It moves the clock 1 ms: Bun's fake clock runs a zero-delay timer
 * 1 ms after it is set, and `jest.advanceTimersByTime(0)` moves it 1 ms. So `tick(n)` leaves the
 * clock `n` + 1 ms on, for every `n` of 0 or more; a negative span runs the last round alone.
 * A test that waits a product timer out installs the fake clock - through `onFakeClock` below,
 * or `jest.useFakeTimers()` of its own - and moves it here rather than waiting for real time; one
 * jump would run every timer of the window at the same instant, so a wait that must really cross
 * a window - and a `Date.now()` the test reads after it - would measure the test rather than the
 * product. Every test that installs the fake clock restores the real one in its `finally` or its
 * file's `afterEach` (`realClock` below): a fake clock left behind stops the timers of every test
 * after it.
 */
export async function tick(ms: number, step = 500): Promise<void> {
  if (step <= 0) throw new RangeError("tick's step must be above zero; use jump for a single jump");
  for (let left = ms; left > 0; left -= step) await round(Math.min(step, left));
  await round(0);
}

/** One round of `tick`: `jest.advanceTimersByTime(ms)` inside `act` (1 ms for an `ms` of 0),
 *  then 20 microtask turns for the promises the move released. */
async function round(ms: number): Promise<void> {
  await act(async () => {
    jest.advanceTimersByTime(ms);
    for (let turn = 0; turn < 20; turn += 1) await Promise.resolve();
  });
}

/** Moves the fake clock `ms` on in one jump, letting every promise the move released run, and
 *  then runs `tick`'s last round of 0 ms, which moves the clock 1 ms more: for a wait that
 *  crosses one timer of the product, where no part of the span needs a round of its own. So
 *  `jump(ms)` leaves the clock `ms` + 1 ms on, and `jump(0)` is that last round alone. That round
 *  fires, inside the same wait, whatever the jump's continuations scheduled at zero delay, so a
 *  product that should wait a timer of its own but follows up at once is caught: the retry row
 *  of `useDwell.test.tsx` fails if a failed write is retried without a new dwell. */
export async function jump(ms: number): Promise<void> {
  await tick(ms, Math.max(ms, 1));
}

/** Restores the real clock a test installed `jest.useFakeTimers()` for, first running whatever a
 *  zero-delay timer left on the fake clock would do: one left behind holds up the next test's
 *  `waitFor`, which moves the clock that test installed rather than the real one. That last step
 *  runs inside `act`, since a write waiting on such a timer continues into React's state as it
 *  fires, and a test rendering a component would otherwise update outside `act`. */
export function realClock(): void {
  act(() => jest.advanceTimersByTime(1));
  jest.useRealTimers();
}

/** Lets every promise a write chained run as far as it goes without the clock moving. */
export async function settle(): Promise<void> {
  for (let turn = 0; turn < 30; turn += 1) await Promise.resolve();
}

/**
 * Lets the real event loop turn, each turn inside `act`, until `done` answers true, and moves no
 * clock but what `each` moves. Bun's fake clock holds `setTimeout` and not the real `setImmediate`
 * (and `Date.now` reads the fake clock), so what reaches a page on real turns - a
 * `BroadcastChannel` message in happy-dom, a `fake-indexeddb` keep - arrives here while every
 * product timer stays where the row left it. A row waits here rather than in `waitFor`, which
 * moves a held clock 50 ms each time it polls, when it must show something came without a timer:
 * how many polls a step takes depends on how loaded the machine is, so a clock `waitFor` moved
 * says how often it polled and not what the product waited for. The wait is `turnsUntil`'s
 * (`@legion/test-fixtures/fake-clock`): it takes no count of turns, ends at the row's deadline
 * naming `what` and what `state` says, and runs `each` before each turn.
 */
export async function onRealTurns(
  what: string,
  done: () => boolean,
  options: { readonly each?: () => void; readonly state?: () => string } = {}
): Promise<void> {
  await turnsUntil(what, done, { ...options, turn: () => act(realTurn) });
}

/** Runs `body` on the fake clock, so a wait inside it - a backoff, a send's deadline, a key's
 *  whole life - waits no real time, and brings the real clock back through `realClock` however
 *  `body` ends. Wrap the whole body when every wait in the row needs the clock, and wrap one
 *  wait alone when a single wait crosses a timer, leaving the row's render and its assertions
 *  on the real clock; a row that installs the clock itself restores it with `realClock`. */
export async function onFakeClock(body: () => Promise<void>): Promise<void> {
  jest.useFakeTimers();
  try {
    await body();
  } finally {
    realClock();
  }
}
