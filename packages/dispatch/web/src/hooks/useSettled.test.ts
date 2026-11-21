import { expect, jest, test } from "bun:test";
import { act, renderHook } from "@testing-library/react";

import { realClock } from "../__tests__/fake-clock";
import { useSettled } from "./useSettled";

// A value is the settled one only once it has held still for the whole wait: each new value
// starts the wait again, so a run of values settles once, on the last of them.
test("a value settles once it has held still for the wait", () => {
  jest.useFakeTimers();
  const { rerender, result, unmount } = renderHook(({ value }) => useSettled(value, 100), {
    initialProps: { value: "a" },
  });
  try {
    expect(result.current).toBe("a");
    rerender({ value: "ab" });
    act(() => jest.advanceTimersByTime(60));
    rerender({ value: "abc" });
    act(() => jest.advanceTimersByTime(60));
    expect(result.current).toBe("a");
    act(() => jest.advanceTimersByTime(40));
    expect(result.current).toBe("abc");
  } finally {
    realClock();
    unmount();
  }
});

// A wait of zero lets a value through in the render that brings it, and the next value that has
// to wait starts from it: it never shows a value from before the one let through.
test("a wait of zero settles a value at once, and the next wait starts from it", () => {
  jest.useFakeTimers();
  const { rerender, result, unmount } = renderHook(
    ({ value }) => useSettled(value, value === "" ? 0 : 100),
    { initialProps: { value: "CORE-1" } }
  );
  try {
    rerender({ value: "" });
    expect(result.current).toBe("");
    rerender({ value: "x" });
    expect(result.current).toBe("");
    act(() => jest.advanceTimersByTime(100));
    expect(result.current).toBe("x");
  } finally {
    realClock();
    unmount();
  }
});
