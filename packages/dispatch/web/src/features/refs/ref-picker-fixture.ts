import { test } from "bun:test";
import { act } from "@testing-library/react";

import { jump } from "../../__tests__/fake-clock";
import { SEARCH_DEBOUNCE_MS } from "./useRefPicker";

/**
 * Lets the reference picker's search debounce run out inside act. `useRefPicker` copies the
 * query into the one it searches through `useSettled`, a timer of `SEARCH_DEBOUNCE_MS` that every
 * key or caret move changing the query sets again, a door opening or closing included, and the
 * draft's pills read the draft after the same span (`draft-pills.tsx`). A wait as long as the
 * debounce, begun after the last key, ends after the timer that key set, and whatever that timer
 * starts - the search, the read it asks for, React's update - lands inside the same act.
 *
 * On the real clock that is a wait of real time. On the fake clock a row installed
 * (`jest.useFakeTimers()`, `__tests__/fake-clock.ts`), it is a jump of the fake clock across the
 * debounce, since a real wait would never fire the faked timer.
 */
export async function settleRefPickerSearch(clock: "real" | "fake" = "real"): Promise<void> {
  if (clock === "fake") {
    await jump(SEARCH_DEBOUNCE_MS);
    return;
  }
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, SEARCH_DEBOUNCE_MS));
  });
}

/**
 * A row of a file that mounts the reference picker: `bun:test`'s own, but its body ends by
 * letting the picker's search debounce run out inside act (`settleRefPickerSearch`). Most rows
 * assert a state that does not wait for the debounce's copy of the query. A timer still set when
 * a body ends fires in the turn Bun gives the event loop between the body and its `afterEach`,
 * outside act, and the preload's act rule fails the row on the update React reports
 * (`__tests__/setup.ts`); a wait in an `afterEach` comes too late, since that turn lies before
 * it. It shows only under CPU load, where the body ends before the timer fires.
 */
export function refPickerTest(name: string, body: () => Promise<void>): void {
  test(name, async () => {
    try {
      await body();
    } finally {
      await settleRefPickerSearch();
    }
  });
}
