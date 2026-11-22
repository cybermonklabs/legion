import { afterEach, expect, mock, test } from "bun:test";
import { holdDeadlines } from "./held-deadlines";
import { settled } from "./wait";

afterEach(() => {
  mock.restore();
});

/** Settles once `signal` aborts. */
function aborted(signal: AbortSignal): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  if (signal.aborted) resolve();
  else signal.addEventListener("abort", () => resolve(), { once: true });
  return promise;
}

test("a deadline of the held length waits for the row, and one of another length is the real one", async () => {
  const deadlines = holdDeadlines(60_000);
  const heldOne = AbortSignal.timeout(60_000);
  const realOne = AbortSignal.timeout(1);
  expect(deadlines.held).toHaveLength(1);
  // The real one ends on its own; the held one stays open until the row ends it.
  await settled("the real 1 ms deadline", aborted(realOne));
  expect(heldOne.aborted).toBe(false);
  deadlines.end();
  expect(heldOne.aborted).toBe(true);
  expect((heldOne.reason as DOMException).name).toBe("TimeoutError");
});

test("end leaves later deadlines of the held length held, and letGo makes them the real one", async () => {
  const deadlines = holdDeadlines(1);
  const first = AbortSignal.timeout(1);
  deadlines.end();
  const second = AbortSignal.timeout(1);
  expect(first.aborted).toBe(true);
  expect(deadlines.held).toHaveLength(2);
  expect(second.aborted).toBe(false);
  deadlines.letGo();
  expect(second.aborted).toBe(true);
  const later = AbortSignal.timeout(1);
  expect(deadlines.held).toHaveLength(2);
  await settled("the deadline armed after the hold was let go", aborted(later));
  expect((later.reason as DOMException).name).toBe("TimeoutError");
});

test("restore puts the real AbortSignal.timeout back", () => {
  const deadlines = holdDeadlines(5);
  deadlines.restore();
  AbortSignal.timeout(5);
  expect(deadlines.held).toHaveLength(0);
});
