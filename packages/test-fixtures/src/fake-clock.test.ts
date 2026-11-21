import { afterEach, beforeEach, expect, jest, test } from "bun:test";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { turn, turnsUntil } from "./fake-clock";
import { temporaryDirectories } from "./temporary-directories";

const makeDirectory = temporaryDirectories("test-fixtures-fake-clock");

beforeEach(() => {
  jest.useFakeTimers();
});

// A fake clock left behind stops the timers of every file the same worker runs after this one.
afterEach(() => {
  jest.useRealTimers();
});

test("real time passes on the fake clock through Atomics.waitAsync, which the row's deadline reads", async () => {
  // The premise of `./deadline`'s real-time alarm, which `turnsUntil` ends by: a timeout of
  // `Atomics.waitAsync` ends while the fake clock stands still, and a `setTimeout` of the same
  // length does not. The wait is the runtime's own, measured, not a wait for a fact.
  let timer = false;
  setTimeout(() => {
    timer = true;
  }, 50);
  const date = Date.now();
  const monotonic = performance.now();
  const started = Bun.nanoseconds();
  const waited = (
    Atomics as unknown as {
      readonly waitAsync: (
        array: Int32Array,
        index: number,
        value: number,
        timeoutMs: number
      ) => { readonly value: Promise<string> };
    }
  ).waitAsync(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 50);
  expect(await waited.value).toBe("timed-out");
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
  await turnsUntil("answered", () => answered);
  expect(fired).toBe(false);
  expect(Date.now()).toBe(date);
});

test("a wait takes each turn through the turn it is given", async () => {
  let answered = false;
  void turn()
    .then(turn)
    .then(() => {
      answered = true;
    });
  let taken = 0;
  await turnsUntil("answered", () => answered, {
    turn: async () => {
      taken += 1;
      await turn();
    },
  });
  expect(answered).toBe(true);
  expect(taken).toBeGreaterThan(0);
});

test("a wait refuses a duration, as an argument or an option", async () => {
  // @ts-expect-error: no wait takes a duration.
  await expect(turnsUntil("answered", () => true, {}, 100)).rejects.toThrow(
    "No wait takes a duration"
  );
  await expect(turnsUntil("answered", () => true, { boundMs: 100 } as never)).rejects.toThrow(
    "No wait takes a duration"
  );
});

// A wait that never ends fails at its row's deadline, read in real time while the row's fake
// clock stands still. The row runs in a `bun test` of its own under a guard of 2400 ms, as
// `row-deadline.test.ts`'s rows do: the deadline is 1000 ms (the least margin) before the guard,
// 1400 ms after the row's mark. The row spends 1100 ms before its fake clock starts, so a wait
// that counted from its own start would outlast the guard and leave only Bun's "timed out" to
// read. The verdict is what the child printed: none reads a clock.
test("a wait that never ends fails at its row's deadline, naming what it waited for", () => {
  const work = makeDirectory("run");
  const fixtures = import.meta.dir;
  writeFileSync(
    join(work, "stuck.test.ts"),
    `import { jest, test } from "bun:test";
import { turnsUntil } from ${JSON.stringify(join(fixtures, "fake-clock.ts"))};
import { span } from ${JSON.stringify(join(fixtures, "wait.ts"))};
test("a row whose fact never comes", async () => {
  await span(1_100, "the row's time before its fake clock starts, which the wait must not be given back");
  jest.useFakeTimers();
  try {
    await turnsUntil("the row's fact", () => false, { state: () => "no answer" });
  } finally {
    jest.useRealTimers();
  }
});
`
  );
  const run = Bun.spawnSync(
    [
      process.execPath,
      "test",
      "--timeout=2400",
      "--preload",
      join(fixtures, "row-deadline.ts"),
      "./stuck.test.ts",
    ],
    { cwd: work, env: process.env }
  );
  const output = `${run.stdout.toString()}${run.stderr.toString()}`;
  expect(output).toContain(
    "turnsUntil gave up waiting for the row's fact, 1000 ms before the row's guard of 2400 ms " +
      "(bun test --timeout) (no answer)"
  );
  expect(output).not.toContain("timed out");
  expect(run.exitCode).toBe(1);
});
