import { expect, test } from "bun:test";

import {
  holdsNoMoment,
  inWindow,
  lastDayOf,
  rangeDays,
  rangeValue,
  readWindow,
  toEndOfDay,
} from "./closed-window";

const HOUR = 3_600_000;
const DAY = 24 * HOUR;
const NOW = Date.parse("2026-11-17T12:00:00Z");

test("f.closed=<N>d is the last N times 24 hours up to now, rolling", () => {
  expect(readWindow("7d", NOW)).toEqual({ from: NOW - 7 * DAY, to: null, rolling: true });
  expect(readWindow("1d", NOW)).toEqual({ from: NOW - DAY, to: null, rolling: true });
  expect(inWindow(new Date(NOW - 7 * DAY).toISOString(), readWindow("7d", NOW))).toBe(true);
  expect(inWindow(new Date(NOW - 7 * DAY - 1).toISOString(), readWindow("7d", NOW))).toBe(false);
  // No days, and more days than a time can hold, are no window.
  expect(readWindow("0d", NOW)).toBeUndefined();
  expect(readWindow("1000000000d", NOW)).toBeUndefined();
});

// k9: the list route reads `closed_since` only from 0000-01-01T00:00:00Z on, since an RFC 3339
// time writes its year in four digits. A count that reaches back past it holds every close, as no
// window does, so no read sends the route a time it refuses (INVALID_CLOSED_SINCE).
test("f.closed=<N>d reaching back past the first time the list route reads is no window", () => {
  const first = Date.parse("0000-01-01T00:00:00Z");
  const furthest = Math.floor((NOW - first) / DAY);
  const window = readWindow(`${furthest}d`, NOW);
  expect(window).toEqual({ from: NOW - furthest * DAY, to: null, rolling: true });
  expect(new Date(window?.from ?? 0).toISOString()).toBe("0000-01-01T12:00:00.000Z");
  expect(readWindow(`${furthest + 1}d`, NOW)).toBeUndefined();
  expect(readWindow("750000d", NOW)).toBeUndefined();
});

// k9: a range whose `to` is at or before its `from` holds no moment. Done lists no row for it and
// asks the route nothing, which refuses it (INVALID_CLOSED_WINDOW). The window stays as the
// address names it, so its chip shows it.
test("f.closed=<from>..<to> whose to is at or before its from holds no moment", () => {
  const backwards = readWindow("2026-11-10..2026-11-03", NOW);
  expect(backwards).toEqual({
    from: Date.parse("2026-11-10T00:00:00Z"),
    to: Date.parse("2026-11-03T00:00:00Z"),
    rolling: false,
  });
  expect(holdsNoMoment(backwards)).toBe(true);
  for (const at of ["2026-11-02T12:00:00Z", "2026-11-05T12:00:00Z", "2026-11-11T12:00:00Z"])
    expect({ at, held: inWindow(at, backwards) }).toEqual({ at, held: false });
  expect(holdsNoMoment(readWindow("2026-11-03..2026-11-03", NOW))).toBe(true);
  expect(holdsNoMoment(readWindow("2026-11-03T10:00:00Z..2026-11-03T09:59:59.999Z", NOW))).toBe(
    true
  );
  // A window of one moment holds it; open ends, a day count and no window hold moments too.
  expect(holdsNoMoment(readWindow("2026-11-03T10:00:00Z..2026-11-03T10:00:00.001Z", NOW))).toBe(
    false
  );
  for (const value of ["2026-11-03..2026-11-04", "2026-11-10..", "..2026-11-03", "7d", "all"])
    expect({ value, empty: holdsNoMoment(readWindow(value, NOW)) }).toEqual({
      value,
      empty: false,
    });
});

test("f.closed=<from>..<to> takes UTC dates or RFC 3339 times in Z, from included and to left out, either end open", () => {
  const week = readWindow("2026-11-03..2026-11-10", NOW);
  expect(week).toEqual({
    from: Date.parse("2026-11-03T00:00:00Z"),
    to: Date.parse("2026-11-10T00:00:00Z"),
    rolling: false,
  });
  expect(inWindow("2026-11-03T00:00:00Z", week)).toBe(true);
  expect(inWindow("2026-11-09T23:59:59Z", week)).toBe(true);
  expect(inWindow("2026-11-10T00:00:00Z", week)).toBe(false);
  expect(inWindow("2026-11-02T23:59:59Z", week)).toBe(false);
  expect(inWindow(null, week)).toBe(false);

  expect(readWindow("2026-11-03T10:30:00Z..2026-11-04T08:00:00.500Z", NOW)).toEqual({
    from: Date.parse("2026-11-03T10:30:00Z"),
    to: Date.parse("2026-11-04T08:00:00.500Z"),
    rolling: false,
  });
  expect(readWindow("2026-11-03..", NOW)).toEqual({
    from: Date.parse("2026-11-03T00:00:00Z"),
    to: null,
    rolling: false,
  });
  expect(readWindow("..2026-11-10", NOW)).toEqual({
    from: null,
    to: Date.parse("2026-11-10T00:00:00Z"),
    rolling: false,
  });
  expect(inWindow("2020-01-01T00:00:00Z", readWindow("..2026-11-10", NOW))).toBe(true);
});

test("f.closed=all, and a value the grammar cannot read, are no window", () => {
  for (const value of [
    "all",
    "..",
    "",
    "7",
    "yesterday",
    "2026-02-30..",
    "2026-11-03T10:30:00+01:00..",
    "2026-11-03T10:30:00..",
    "2026-11-03...2026-11-04",
  ]) {
    expect({ value, window: readWindow(value, NOW) }).toEqual({ value, window: undefined });
  }
  expect(readWindow(null, NOW)).toBeUndefined();
  expect(inWindow(null, undefined)).toBe(true);
  expect(inWindow("2026-11-03T00:00:00Z", undefined)).toBe(true);
});

test("the picker includes the To day it shows: 11/03 to 11/09 writes f.closed=2026-11-03..2026-11-10 and reads back the same", () => {
  const written = rangeValue("2026-11-03", "2026-11-09");
  expect(written).toBe("2026-11-03..2026-11-10");
  const window = readWindow(written, NOW);
  expect(window).toEqual({
    from: Date.parse("2026-11-03T00:00:00Z"),
    to: Date.parse("2026-11-03T00:00:00Z") + 7 * DAY,
    rolling: false,
  });
  expect(rangeDays("2026-11-03..2026-11-10")).toEqual({ from: "2026-11-03", to: "2026-11-09" });

  expect(toEndOfDay("2026-11-09")).toBe("2026-11-10");
  expect(toEndOfDay("2026-11-30")).toBe("2026-12-01");
  expect(toEndOfDay("2026-12-31")).toBe("2027-01-01");
  expect(toEndOfDay("")).toBe("");
  expect(lastDayOf("2026-11-10")).toBe("2026-11-09");
  expect(lastDayOf("2026-11-09T12:00:00Z")).toBe("2026-11-09");
  expect(lastDayOf("2026-11-10T00:00:00Z")).toBe("2026-11-09");
  expect(lastDayOf("")).toBe("");
  expect(lastDayOf("soon")).toBe("");

  // Either end may be left out, in both directions.
  expect(rangeValue("2026-11-03", "")).toBe("2026-11-03..");
  expect(rangeValue("", "2026-11-09")).toBe("..2026-11-10");
  expect(rangeValue("", "")).toBeUndefined();
  // The picker takes no To before its From: its window would hold no moment.
  expect(rangeValue("2026-11-10", "2026-11-03")).toBeUndefined();
  expect(rangeValue("2026-11-10", "2026-11-09")).toBeUndefined();
  expect(rangeValue("2026-11-10", "2026-11-10")).toBe("2026-11-10..2026-11-11");
  expect(rangeDays("2026-11-03..")).toEqual({ from: "2026-11-03", to: "" });
  expect(rangeDays("..2026-11-10")).toEqual({ from: "", to: "2026-11-09" });
  expect(rangeDays("2026-11-03T10:30:00Z..2026-11-04T08:00:00Z")).toEqual({
    from: "2026-11-03",
    to: "2026-11-04",
  });
  expect(rangeDays("7d")).toBeUndefined();
  expect(rangeDays("all")).toBeUndefined();
});
