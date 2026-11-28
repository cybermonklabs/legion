import { expect, test } from "bun:test";

import {
  compareRange,
  customRange,
  hasMalformedWindowParams,
  liveWindow,
  MAX_WINDOW_DAYS,
  readCompare,
  readWindow,
  windowDays,
} from "./window";

test("the URL's window: from and to are a custom range, window names a preset, and anything else is the last 28 days", () => {
  expect(
    readWindow(new URLSearchParams("from=2024-06-01T00:00:00Z&to=2024-06-02T00:00:00Z&window=7d"))
  ).toEqual({ kind: "custom", from: "2024-06-01T00:00:00Z", to: "2024-06-02T00:00:00Z" });
  expect(readWindow(new URLSearchParams("window=14d"))).toEqual({ kind: "preset", preset: "14d" });
  expect(readWindow(new URLSearchParams(""))).toEqual({ kind: "preset", preset: "28d" });
  expect(readWindow(new URLSearchParams("window=1y"))).toEqual({ kind: "preset", preset: "28d" });
  expect(readWindow(new URLSearchParams("from=2024-06-01T00:00:00Z"))).toEqual({
    kind: "preset",
    preset: "28d",
  });
});

test("a malformed custom window falls back to the default and tells the page to drop its parameters", () => {
  const malformed = new URLSearchParams("from=not-a-date&to=also-not-a-date");
  expect(readWindow(malformed)).toEqual({ kind: "preset", preset: "28d" });
  expect(hasMalformedWindowParams(malformed)).toBe(true);

  const inverted = new URLSearchParams("from=2026-10-11T00:00:00Z&to=2026-10-10T00:00:00Z");
  expect(readWindow(inverted)).toEqual({ kind: "preset", preset: "28d" });
  expect(hasMalformedWindowParams(inverted)).toBe(true);
});

test("a preset subscribes by its days, which Dispatch keeps ending at the present; a custom range by its ends", () => {
  expect(liveWindow({ kind: "preset", preset: "7d" })).toEqual({ preset: 7 });
  expect(liveWindow({ kind: "preset", preset: "84d" })).toEqual({ preset: 84 });
  expect(
    liveWindow({ kind: "custom", from: "2026-01-01T00:00:00.000Z", to: "2026-01-08T00:00:00.000Z" })
  ).toEqual({ from: "2026-01-01T00:00:00.000Z", to: "2026-01-08T00:00:00.000Z" });
});

test("a custom range takes whole UTC days, its last day included, up to the server's 84", () => {
  expect(MAX_WINDOW_DAYS).toBe(84);
  expect(customRange("2026-01-01", "2026-01-07")).toEqual({
    from: "2026-01-01T00:00:00.000Z",
    to: "2026-01-08T00:00:00.000Z",
  });
  const longest = customRange("2026-01-01", "2026-03-25");
  expect("refused" in longest).toBe(false);
  expect(customRange("2026-01-01", "2026-03-26")).toEqual({
    refused: "The window can span at most 84 days; this range spans 85.",
  });
  expect(customRange("2026-01-07", "2026-01-01")).toEqual({
    refused: "The window's end must not come before its start.",
  });
  expect(customRange("", "2026-01-01")).toEqual({
    refused: "Choose both a start and an end date.",
  });
});

test("the URL's compare range: compare_from and compare_to are a custom range, compare=off is none, and anything else is the previous range", () => {
  expect(
    readCompare(
      new URLSearchParams(
        "compare_from=2026-01-01T00:00:00Z&compare_to=2026-01-08T00:00:00Z&compare=off"
      )
    )
  ).toEqual({ kind: "custom", from: "2026-01-01T00:00:00Z", to: "2026-01-08T00:00:00Z" });
  expect(readCompare(new URLSearchParams("compare=off"))).toEqual({ kind: "off" });
  expect(readCompare(new URLSearchParams(""))).toEqual({ kind: "previous" });
  expect(readCompare(new URLSearchParams("compare=never"))).toEqual({ kind: "previous" });
  expect(
    readCompare(
      new URLSearchParams("compare_from=2026-01-08T00:00:00Z&compare_to=2026-01-01T00:00:00Z")
    )
  ).toEqual({ kind: "previous" });
});

test("a window covers the whole UTC days Dispatch counts: a preset ending inside a day covers one day more than it names", () => {
  const now = Date.parse("2026-11-23T15:30:00Z");
  expect(windowDays({ kind: "preset", preset: "28d" }, now)).toEqual({
    from: "2026-10-26T00:00:00.000Z",
    to: "2026-11-24T00:00:00.000Z",
  });
  expect(
    windowDays({ kind: "custom", from: "2026-01-01T00:00:00Z", to: "2026-01-08T00:00:00Z" }, now)
  ).toEqual({ from: "2026-01-01T00:00:00.000Z", to: "2026-01-08T00:00:00.000Z" });
  expect(
    windowDays({ kind: "custom", from: "2026-01-01T06:00:00Z", to: "2026-01-07T20:00:00Z" }, now)
  ).toEqual({ from: "2026-01-01T00:00:00.000Z", to: "2026-01-08T00:00:00.000Z" });
});

test("the default compare range is the days just before the window, as many as it covers and at most 84; off is none and a custom range is its own", () => {
  const now = Date.parse("2026-11-23T15:30:00Z");
  const custom = {
    kind: "custom",
    from: "2026-01-15T00:00:00Z",
    to: "2026-01-22T00:00:00Z",
  } as const;
  expect(compareRange({ kind: "previous" }, custom, now)).toEqual({
    from: "2026-01-08T00:00:00.000Z",
    to: "2026-01-15T00:00:00.000Z",
  });
  // 29 whole days for the 28-day preset, the 29 before them.
  expect(compareRange({ kind: "previous" }, { kind: "preset", preset: "28d" }, now)).toEqual({
    from: "2026-09-27T00:00:00.000Z",
    to: "2026-10-26T00:00:00.000Z",
  });
  const longest = compareRange({ kind: "previous" }, { kind: "preset", preset: "84d" }, now);
  expect(longest).toEqual({ from: "2026-06-08T00:00:00.000Z", to: "2026-08-31T00:00:00.000Z" });
  expect(Date.parse(longest?.to ?? "") - Date.parse(longest?.from ?? "")).toBe(
    MAX_WINDOW_DAYS * 24 * 60 * 60 * 1000
  );
  expect(compareRange({ kind: "off" }, custom, now)).toBeNull();
  expect(
    compareRange(
      { kind: "custom", from: "2025-01-01T00:00:00Z", to: "2025-02-01T00:00:00Z" },
      custom,
      now
    )
  ).toEqual({ from: "2025-01-01T00:00:00Z", to: "2025-02-01T00:00:00Z" });
});
