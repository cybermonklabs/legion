import { test } from "bun:test";
import { cleanup } from "@testing-library/react";

import { jump } from "../../__tests__/fake-clock";
import { SEARCH_DEBOUNCE_MS } from "./useRefPicker";

/**
 * Moves the fake clock across the reference picker's search debounce inside act (`jump`).
 * `useRefPicker` copies the query into the one it searches through `useSettled`, a timer of
 * `SEARCH_DEBOUNCE_MS` that every key or caret move changing the query sets again, a door opening
 * or closing included, and the draft's pills read the draft after the same span
 * (`draft-pills.tsx`). Call this inside `onFakeClock` or after `jest.useFakeTimers()`, with the
 * key or edit that arms the timer on the same fake clock, so the jump fires that timer and lands
 * whatever it starts - the search, the read it asks for, React's update - inside act.
 */
export async function settleRefPickerSearch(): Promise<void> {
  await jump(SEARCH_DEBOUNCE_MS);
}

/**
 * A row of a file that mounts the reference picker: `bun:test`'s own, but its body ends by
 * unmounting what the row rendered (`cleanup`). Most rows assert a state that does not wait for
 * `useRefPicker`'s search debounce (`useSettled`). A timer still set when a body ends fires in the
 * turn Bun gives the event loop between the body and its `afterEach`, outside act, and the
 * preload's act rule fails the row on the update React reports (`__tests__/setup.ts`); a `cleanup`
 * in an `afterEach` comes too late, since that turn lies before it. Unmounting in the body's own
 * `finally` runs `useSettled`'s effect cleanup and clears the timer at once, with no wait.
 */
export function refPickerTest(name: string, body: () => Promise<void>): void {
  test(name, async () => {
    try {
      await body();
    } finally {
      cleanup();
    }
  });
}
