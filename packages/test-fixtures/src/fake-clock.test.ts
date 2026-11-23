import { afterEach, beforeEach, expect, jest, test } from "bun:test";
import { realTime, turn, turnsUntil } from "./fake-clock";

beforeEach(() => {
  jest.useFakeTimers();
});

// A fake clock left behind stops the timers of every file the same worker runs after this one.
afterEach(() => {
  jest.useRealTimers();
});

test("real time passes on the fake clock, which holds the timers and the clocks a row could read", async () => {
  // The premise of the module: a bound read on `realTime` ends while the fake clock stands still,
  // and a `setTimeout` of the same length does not.
  let timer = false;
  setTimeout(() => {
    timer = true;
  }, 50);
  const date = Date.now();
  const monotonic = performance.now();
  const started = Bun.nanoseconds();
  await realTime(50);
  expect(timer).toBe(false);
  expect(Date.now()).toBe(date);
  expect(performance.now()).toBe(monotonic);
  expect(Bun.nanoseconds()).toBe(started);
});

test("a wait returns once its condition holds, and moves no clock", async () => {
  let answered = false;
  void turn()
    .then(turn)
    .then(() => {
      answered = true;
    });
  let fired = false;
  setTimeout(() => {
    fired = true;
  }, 0);
  const date = Date.now();
  await turnsUntil(() => answered, "answered");
  expect(fired).toBe(false);
  expect(Date.now()).toBe(date);
});

test("a wait that never ends fails at its real-time bound, naming what it waited for", async () => {
  const failure = await turnsUntil(() => false, "answered", {
    boundMs: 100,
    state: () => "no answer",
  }).then(
    () => "ended",
    (error: unknown) => (error instanceof Error ? error.message : String(error))
  );
  expect(failure).toBe("still not answered after 0.1 s of real time (no answer)");
});
