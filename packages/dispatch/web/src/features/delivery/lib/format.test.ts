import { expect, test } from "bun:test";
import { formatDue, formatDuration, formatMinutes, formatRate, pct } from "./format";

test("formatDuration: one decimal under ten minutes, whole minutes under three hours, then hours", () => {
  expect(formatDuration(4.62)).toBe("4.6m");
  expect(formatDuration(93.4)).toBe("93m");
  expect(formatDuration(179.4)).toBe("179m");
  expect(formatDuration(192)).toBe("3.2h");
  expect(formatDuration(null)).toBe("n/a");
});

test("formatMinutes: whole minutes under an hour, one-decimal hours under a day, then days", () => {
  expect(formatMinutes(45)).toBe("45m");
  expect(formatMinutes(59.6)).toBe("60m");
  expect(formatMinutes(210)).toBe("3.5h");
  expect(formatMinutes(3024)).toBe("2.1d");
  expect(formatMinutes(null)).toBe("n/a");
});

// R3-C1: a toned figure takes decimals past its usual ones where they would print it on the other
// side of its target (`lib/target.ts`'s `judgeFigure`); each bucket keeps its unit.
test("formatDuration and formatMinutes write extra decimals past each bucket's usual ones", () => {
  expect([4.62, 44.6, 192, null].map((minutes) => formatDuration(minutes, 1))).toEqual([
    "4.62m",
    "44.6m",
    "3.20h",
    "n/a",
  ]);
  expect([59.6, 210, 3024, null].map((minutes) => formatMinutes(minutes, 1))).toEqual([
    "59.6m",
    "3.50h",
    "2.10d",
    "n/a",
  ]);
});

test("formatRate: a whole percentage, n/a when nothing ended in success or failure", () => {
  expect(formatRate(0.8667)).toBe("87%");
  expect(formatRate(null)).toBe("n/a");
});

test("pct: a percentage to the given number of decimals", () => {
  expect(pct(0.0496, 2)).toBe("4.96%");
  expect(pct(0.3306)).toBe("33.1%");
});

test("formatDue: a YYYY-MM-DD day as month and day, anything else an error", () => {
  expect(formatDue("2026-10-01")).toBe("Oct 1");
  expect(formatDue("2026-12-25")).toBe("Dec 25");
  expect(() => formatDue("tomorrow")).toThrow("day tomorrow is not YYYY-MM-DD");
});
