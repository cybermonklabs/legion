import { afterEach, expect, jest, test } from "bun:test";

import {
  contentDrawn,
  HOLD_LIMIT_MS,
  holdUntilContentDraws,
  whenContentDrawn,
} from "./content-drawn";

/** Whether `promise` has settled once every microtask queued so far has run. */
async function settled(promise: Promise<void>): Promise<boolean> {
  let done = false;
  void promise.then(() => {
    done = true;
  });
  for (let turn = 0; turn < 20; turn += 1) await Promise.resolve();
  return done;
}

afterEach(async () => {
  jest.useRealTimers();
  // A hold one test left on would hold every test after it: end it, and wait for its frame.
  contentDrawn();
  await whenContentDrawn();
});

test("with no hold, the shell's extras start at once", async () => {
  expect(await settled(whenContentDrawn())).toBe(true);
});

test("a hold ends one frame after the document draws, not in the same task", async () => {
  holdUntilContentDraws();
  const drawn = whenContentDrawn();
  expect(await settled(drawn)).toBe(false);
  contentDrawn();
  expect(await settled(drawn)).toBe(false);
  await drawn;
});

test("the reader's first key or pointer press ends a hold", async () => {
  holdUntilContentDraws();
  const afterKey = whenContentDrawn();
  window.dispatchEvent(new KeyboardEvent("keydown", { key: "/" }));
  await afterKey;

  holdUntilContentDraws();
  const afterPointer = whenContentDrawn();
  window.dispatchEvent(new Event("pointerdown"));
  await afterPointer;
});

test("a hold that nothing ends, as for a document that never connects, ends at its limit", async () => {
  jest.useFakeTimers();
  holdUntilContentDraws();
  const drawn = whenContentDrawn();
  jest.advanceTimersByTime(HOLD_LIMIT_MS - 1);
  expect(await settled(drawn)).toBe(false);
  jest.advanceTimersByTime(1);
  expect(await settled(drawn)).toBe(true);
});
