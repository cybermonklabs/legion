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

test("a hold ends one frame after the document draws, not before that frame and not in it", async () => {
  // The test holds the frames itself, so it says when the next frame is painted. A release on a
  // bare timeout, with no frame before it, would end the hold while that frame is still waiting.
  const frames: FrameRequestCallback[] = [];
  const requestFrame = globalThis.requestAnimationFrame;
  globalThis.requestAnimationFrame = (callback) => {
    frames.push(callback);
    return frames.length;
  };
  try {
    jest.useFakeTimers();
    holdUntilContentDraws();
    const drawn = whenContentDrawn();
    expect(await settled(drawn)).toBe(false);
    contentDrawn();
    // Every timeout short of the hold's own limit runs, and no frame: the hold is still on.
    jest.advanceTimersByTime(HOLD_LIMIT_MS - 1);
    expect(await settled(drawn)).toBe(false);
    expect(frames).toHaveLength(1);

    frames[0]?.(performance.now());
    // The frame itself does not end the hold: the task after its paint does.
    expect(await settled(drawn)).toBe(false);
    jest.advanceTimersByTime(0);
    expect(await settled(drawn)).toBe(true);
  } finally {
    globalThis.requestAnimationFrame = requestFrame;
  }
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
