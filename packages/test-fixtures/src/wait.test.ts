import { expect, jest, test } from "bun:test";
import { quiet, settled, span, timeLeft, until } from "./wait";

// What each wait does inside a row of its own. Where a wait ends against a row's guard is
// `row-deadline.test.ts`'s, which runs rows under a guard of their own.

const NO_DURATION = "No wait takes a duration: each ends at its row's deadline";
const ADVANCE = "Advance the fake clock to the moment the fact happens";

test("until resolves with the first truthy value its probe returns", async () => {
  const readings = [undefined, 0, "", "the fourth reading"];
  let reads = 0;
  const held = await until("the fourth reading", () => readings[reads++]);
  expect(held).toBe("the fourth reading");
  expect(reads).toBe(4);
});

test("until takes a probe that returns a promise", async () => {
  let reads = 0;
  const held = await until("the second answer", async () => (++reads === 2 ? reads : null));
  expect(held).toBe(2);
});

test("a probe that throws ends the wait at once with its own error", async () => {
  let reads = 0;
  const wait = until("a fact the exited process made impossible", () => {
    reads += 1;
    throw new Error("the process exited with 3");
  });
  await expect(wait).rejects.toThrow("the process exited with 3");
  expect(reads).toBe(1);
});

test("settled resolves with the promise's value and rejects with its reason", async () => {
  expect(await settled("the answer", Promise.resolve("answered"))).toBe("answered");
  await expect(settled("the refusal", Promise.reject(new Error("refused")))).rejects.toThrow(
    "refused"
  );
});

test("every wait refuses a duration in its types and at run time", async () => {
  // Each line below is what a caller that still passes a bound writes; `tsc` refuses each one
  // (the directive fails `bun run typecheck` when a signature starts accepting the argument), and
  // the call itself throws for a file `tsc` does not check.
  // @ts-expect-error until takes no duration
  const untilWithin = until("a fact", () => true, 5_000);
  await expect(untilWithin).rejects.toThrow(
    `until(what, probe) was given 1 argument more than it takes (5000). ${NO_DURATION}`
  );
  // @ts-expect-error settled takes no duration
  const settledWithin = settled("a promise", Promise.resolve(1), 5_000);
  await expect(settledWithin).rejects.toThrow(`settled(what, promise) was given 1 argument`);
  // @ts-expect-error span takes nothing after its why
  const spanWithin = span(1, "a span", 5_000);
  await expect(spanWithin).rejects.toThrow(`span(ms, why) was given 1 argument`);
  // @ts-expect-error quiet takes nothing after its why
  const quietWithin = quiet("an event", noEvents(), 1, "a span", 5_000);
  await expect(quietWithin).rejects.toThrow(`quiet(what, events, ms, why) was given 1 argument`);
  // @ts-expect-error timeLeft takes no argument
  expect(() => timeLeft(5_000)).toThrow(`timeLeft() was given 1 argument more than it takes`);
});

test("every wait refuses to run under fake timers and says to advance the fake clock", async () => {
  jest.useFakeTimers();
  try {
    await expect(until("a fact", () => true)).rejects.toThrow(ADVANCE);
    await expect(settled("a promise", Promise.resolve(1))).rejects.toThrow(ADVANCE);
    await expect(span(1, "a span")).rejects.toThrow(ADVANCE);
    await expect(quiet("an event", noEvents(), 1, "a span")).rejects.toThrow(ADVANCE);
    expect(() => timeLeft()).toThrow(ADVANCE);
  } finally {
    jest.useRealTimers();
  }
});

test("a wait refuses a what that names nothing, and a span a why that says nothing", async () => {
  await expect(until(" ", () => true)).rejects.toThrow("until needs a what");
  await expect(settled("", Promise.resolve(1))).rejects.toThrow("settled needs a what");
  await expect(span(1, "")).rejects.toThrow("span needs a why");
  await expect(quiet("an event", noEvents(), 1, " ")).rejects.toThrow("quiet needs a why");
  await expect(span(-1, "a span")).rejects.toThrow("span's span must be a number");
});

test("a span that would end past the row's deadline is refused at once", async () => {
  const past = timeLeft() + 1_000;
  await expect(span(past, "a span longer than the row has")).rejects.toThrow(
    `span: a span of ${past} ms (a span longer than the row has) would end after the deadline`
  );
  await expect(quiet("an event", noEvents(), past, "a span longer")).rejects.toThrow(
    `quiet: a span of ${past} ms (a span longer) would end after the deadline`
  );
});

test("quiet fails naming the value a source yields within its span", async () => {
  async function* reminders(): AsyncGenerator<string> {
    yield "a second reminder";
  }
  await expect(
    quiet("a second reminder", reminders(), 1_000, "the hook sends from a timer of its own")
  ).rejects.toThrow(
    "quiet: a second reminder happened within the 1000 ms span (the hook sends from a timer of " +
      'its own): "a second reminder"'
  );
});

test("quiet holds a source that yields nothing for its span, and ends its iteration", async () => {
  // A span is a real span: this row exercises the one function that holds the clock on purpose.
  const gate = Promise.withResolvers<void>();
  let ended = false;
  async function* afterTheGate(): AsyncGenerator<string> {
    try {
      await gate.promise;
      yield "an event after the span";
    } finally {
      ended = true;
    }
  }
  await quiet("an event", afterTheGate(), 20, "the source yields only once the test opens it");
  gate.resolve();
  await until("the source's iteration to end", () => ended);
});

test("quiet holds a source that ends without yielding", async () => {
  await quiet("an event", noEvents(), 1_000, "a source that ends at once yields nothing");
});

test("timeLeft is the time before the row's deadline", () => {
  const first = timeLeft();
  expect(first).toBeGreaterThan(0);
  expect(timeLeft()).toBeLessThanOrEqual(first);
});

/** A source that ends without yielding. */
async function* noEvents(): AsyncGenerator<never> {
  yield* [];
}
