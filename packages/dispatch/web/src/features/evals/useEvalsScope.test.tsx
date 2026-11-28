import { expect, jest, test } from "bun:test";
import { renderHook } from "@testing-library/react";

import { jump, realClock } from "../../__tests__/fake-clock";
import type { EvalsUrlState } from "./evals-url";
import { useEvalsScope } from "./useEvalsScope";

const PRESET: EvalsUrlState = {
  brush: null,
  compare: { kind: "previous" },
  repos: [],
  window: { kind: "preset", preset: "28d" },
};

test("a tab left open past a UTC midnight moves its default compare range with the window, and its timer goes with it", async () => {
  jest.useFakeTimers();
  try {
    jest.setSystemTime(new Date("2026-11-23T23:59:00Z"));
    const { result, unmount } = renderHook(() => useEvalsScope("ACME", PRESET));
    // The 28 days to now touch 29 whole UTC days, from 2026-10-26; the range before is as long.
    const before = { from: "2026-09-27T00:00:00.000Z", to: "2026-10-26T00:00:00.000Z" };
    expect(result.current.compare).toEqual(before);
    const scope = result.current;

    // A second before midnight: the same days, and the same scope, so nothing subscribes again.
    await jump(59_000);
    expect(result.current).toBe(scope);

    // Past midnight the server's window starts a day later, and so does the range before it.
    await jump(1_000);
    expect(result.current.compare).toEqual({
      from: "2026-09-28T00:00:00.000Z",
      to: "2026-10-27T00:00:00.000Z",
    });
    expect(result.current.window).toEqual({ preset: 28 });

    // The tab holds one timer, to the next midnight, and drops it when it goes.
    const held = jest.getTimerCount();
    unmount();
    expect(jest.getTimerCount()).toBe(held - 1);
  } finally {
    realClock();
  }
});

test("a custom compare range and compare off do not move at midnight", async () => {
  jest.useFakeTimers();
  try {
    jest.setSystemTime(new Date("2026-11-23T23:59:00Z"));
    const custom: EvalsUrlState = {
      ...PRESET,
      compare: { from: "2026-10-01T00:00:00Z", kind: "custom", to: "2026-10-08T00:00:00Z" },
    };
    const off: EvalsUrlState = { ...PRESET, compare: { kind: "off" } };
    const { result } = renderHook(() => [
      useEvalsScope("ACME", custom),
      useEvalsScope("ACME", off),
    ]);
    await jump(120_000);
    expect(result.current[0]?.compare).toEqual({
      from: "2026-10-01T00:00:00Z",
      to: "2026-10-08T00:00:00Z",
    });
    expect(result.current[1]?.compare).toBeNull();
  } finally {
    realClock();
  }
});
